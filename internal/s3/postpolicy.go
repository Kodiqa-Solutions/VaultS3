package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// A browser form upload signed with a POST policy (what the SDKs call a
// presigned POST) carries its credentials in the form, not in an Authorization
// header, so the router hands it here instead of authenticating it. Everything
// is checked from the fields that come before the file, and only then is the
// file read: the signature over the policy, the policy's expiry and every one
// of its conditions, the caller's IP allowlist, and the IAM permission to write
// the key. Nothing of the file is read, let alone stored, for a request that
// fails any of them.

const (
	// maxPolicyField bounds one form field's value, and maxPolicyFields all
	// of them before the file, so a form cannot make the server buffer much
	// before its signature is checked. Real forms are a few KiB.
	maxPolicyField  = 64 << 10
	maxPolicyFields = 256 << 10
	// maxPolicyPreamble bounds every byte read before the file, part
	// headers and boundaries included. mime/multipart allows 10 MiB of
	// header for each part, held in memory, so the value bounds alone let an
	// anonymous client make the server hold 10 MiB a connection: 150 such
	// connections took a 700 MiB server down.
	maxPolicyPreamble = maxPolicyFields + 64<<10
	// maxPolicyParts bounds how many parts may come before the file.
	maxPolicyParts = 1000
	// maxPostObject is the largest object a form upload may store, as for a
	// single PutObject.
	maxPostObject int64 = 5 << 30
)

// isPolicyPost reports a form upload whose credentials are in the form: a POST
// to a bucket with a multipart form and no Authorization header or query
// signature. A header-signed form upload goes through normal authentication.
func isPolicyPost(r *http.Request, bucket, key string) bool {
	if r.Method != http.MethodPost || bucket == "" || key != "" || !isFormPost(r) {
		return false
	}
	if r.Header.Get("Authorization") != "" {
		return false
	}
	q := r.URL.Query()
	return !q.Has("X-Amz-Signature") && !q.Has("X-Amz-Credential") && !q.Has("delete")
}

type bodyDeadlineKey struct{}

// WithBodyDeadlineRelease records, for a request whose body is read under a
// deadline before it is authenticated, the function that lifts the deadline
// once it is. The listener sets the deadline outside the middleware, where it
// reaches the connection.
func WithBodyDeadlineRelease(r *http.Request, release func()) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), bodyDeadlineKey{}, release))
}

// MayBePolicyPost reports, from the request alone, whether it may be a form
// upload signed with a POST policy, whose fields are read before anyone is
// authenticated. It is wider than isPolicyPost, which also needs the bucket
// and key the router resolves; a request it wrongly includes is refused at
// authentication before its body is read anyway.
func MayBePolicyPost(r *http.Request) bool {
	if r.Method != http.MethodPost || !isFormPost(r) || r.Header.Get("Authorization") != "" {
		return false
	}
	q := r.URL.Query()
	return !q.Has("X-Amz-Signature") && !q.Has("X-Amz-Credential")
}

var errPreambleTooLong = errors.New("the form before its file is too long")

var errFieldsTooLarge = &reqError{"MaxPostPreDataLengthExceeded", "Your POST request fields preceding the upload file were too large.", http.StatusBadRequest}

// preambleBudget fails the read once more than left bytes arrive before the
// file part, and stops counting once lifted.
type preambleBudget struct {
	io.ReadCloser
	left   int64
	lifted bool
}

func (b *preambleBudget) Read(p []byte) (int, error) {
	if b.lifted {
		return b.ReadCloser.Read(p)
	}
	if b.left <= 0 {
		return 0, errPreambleTooLong
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}

// policyForm is a POST policy form read up to its file.
type policyForm struct {
	fields   map[string]string // lower-cased field name -> value
	file     *multipart.Part
	filename string
}

// readPolicyForm reads the form fields up to the file part, which it leaves
// unread. Field names are case-insensitive, as on AWS.
func readPolicyForm(r *http.Request) (*policyForm, *reqError) {
	budget := &preambleBudget{ReadCloser: r.Body, left: maxPolicyPreamble}
	r.Body = budget
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, &reqError{"MalformedPOSTRequest", "The body of the POST request is not well-formed multipart/form-data.", http.StatusBadRequest}
	}
	f := &policyForm{fields: map[string]string{}}
	total := 0
	for parts := 0; ; parts++ {
		// Counted as well as sized: empty parts cost nothing toward the byte
		// bound but each still has headers to parse.
		if parts > maxPolicyParts {
			return nil, &reqError{"MaxPostPreDataLengthExceeded", "Your POST request fields preceding the upload file were too large.", http.StatusBadRequest}
		}
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil, &reqError{"InvalidArgument", "POST requires exactly one file upload per request.", http.StatusBadRequest}
		}
		if err != nil {
			if errors.Is(err, errPreambleTooLong) {
				return nil, errFieldsTooLarge
			}
			if re := bodyFailure(err); re != nil {
				return nil, re
			}
			return nil, &reqError{"MalformedPOSTRequest", "The body of the POST request is not well-formed multipart/form-data.", http.StatusBadRequest}
		}
		name := part.FormName()
		if name == "" {
			// A part without a name still counts toward the bound, or an
			// anonymous client could make the server read any amount before
			// the signature is checked.
			n, _ := io.Copy(io.Discard, io.LimitReader(part, int64(maxPolicyFields-total)+1))
			if total += int(n); total > maxPolicyFields {
				return nil, &reqError{"MaxPostPreDataLengthExceeded", "Your POST request fields preceding the upload file were too large.", http.StatusBadRequest}
			}
			continue
		}
		if strings.EqualFold(name, "file") {
			// The file itself is held to the policy's size range and the
			// quota as it streams, not to the preamble bound.
			budget.lifted = true
			f.file, f.filename = part, part.FileName()
			return f, nil
		}
		value, err := io.ReadAll(io.LimitReader(part, maxPolicyField+1))
		if errors.Is(err, errPreambleTooLong) {
			return nil, errFieldsTooLarge
		}
		if err != nil {
			return nil, &reqError{"MalformedPOSTRequest", "The body of the POST request is not well-formed multipart/form-data.", http.StatusBadRequest}
		}
		total += len(value)
		if len(value) > maxPolicyField || total > maxPolicyFields {
			return nil, &reqError{"MaxPostPreDataLengthExceeded", "Your POST request fields preceding the upload file were too large.", http.StatusBadRequest}
		}
		lname := strings.ToLower(name)
		if _, dup := f.fields[lname]; dup {
			return nil, &reqError{"InvalidArgument", "Form field " + lname + " appears more than once.", http.StatusBadRequest}
		}
		f.fields[lname] = string(value)
	}
}

// policyUpload is what a valid POST policy allows.
type policyUpload struct {
	identity    *iam.Identity
	key         string
	minSize     int64
	maxSize     int64
	contentType string
	headers     objectHeaders
	userMeta    map[string]string
	tags        map[string]string
	// checksums are the form's x-amz-checksum-* fields, as the request
	// headers the body verifier reads.
	checksums     http.Header
	successStatus int
	redirect      string
}

func policyDenied(msg string) *reqError {
	return &reqError{"AccessDenied", msg, http.StatusForbidden}
}

// checkPolicyForm verifies the form's signature and policy and returns what it
// allows. The signature is checked before anything the policy says is trusted.
func (h *Handler) checkPolicyForm(r *http.Request, bucket string, f *policyForm) (*policyUpload, *reqError) {
	policyB64 := f.fields["policy"]
	if policyB64 == "" {
		return nil, policyDenied("Bucket POST must contain a field named policy.")
	}
	if f.fields["awsaccesskeyid"] != "" || f.fields["signature"] != "" {
		return nil, policyDenied("Signature Version 2 is not supported, sign the policy with AWS4-HMAC-SHA256.")
	}
	if f.fields["x-amz-algorithm"] != "AWS4-HMAC-SHA256" {
		return nil, policyDenied("The x-amz-algorithm field must be AWS4-HMAC-SHA256.")
	}
	scope := strings.Split(f.fields["x-amz-credential"], "/")
	if len(scope) != 5 || scope[0] == "" || scope[3] != "s3" || scope[4] != "aws4_request" {
		return nil, policyDenied("The x-amz-credential field is malformed.")
	}
	amzDate := f.fields["x-amz-date"]
	if _, err := time.Parse("20060102T150405Z", amzDate); err != nil || amzDate[:8] != scope[1] {
		return nil, policyDenied("The x-amz-date field is missing, malformed, or not the date in x-amz-credential.")
	}

	// A session credential's token arrives as a form field.
	tokReq := r.Clone(r.Context())
	tokReq.Header.Set("X-Amz-Security-Token", f.fields["x-amz-security-token"])
	identity, secret, idErr := h.auth.resolveIdentity(scope[0], tokReq)
	if secret == "" {
		return nil, policyDenied("Access Denied")
	}
	mac := hmac.New(sha256.New, deriveSigningKey(secret, scope[1], scope[2], "s3"))
	mac.Write([]byte(policyB64))
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(strings.ToLower(f.fields["x-amz-signature"]))) {
		return nil, &reqError{"SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", http.StatusForbidden}
	}
	if idErr != nil {
		slog.Info("s3: form upload refused at authentication", "reason", idErr.Error())
		return nil, policyDenied("Access Denied")
	}

	raw, err := base64.StdEncoding.DecodeString(policyB64)
	if err != nil {
		return nil, &reqError{"InvalidPolicyDocument", "The policy is not base64 encoded.", http.StatusBadRequest}
	}
	var policy struct {
		Expiration string            `json:"expiration"`
		Conditions []json.RawMessage `json:"conditions"`
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, &reqError{"InvalidPolicyDocument", "The policy is not a valid JSON document.", http.StatusBadRequest}
	}
	expiry, err := time.Parse(time.RFC3339, policy.Expiration)
	if err != nil {
		return nil, &reqError{"InvalidPolicyDocument", "The policy has no valid expiration.", http.StatusBadRequest}
	}
	if time.Now().After(expiry) {
		return nil, policyDenied("Invalid according to Policy: Policy expired.")
	}

	up := &policyUpload{identity: identity, key: f.fields["key"], minSize: 0, maxSize: maxPostObject, successStatus: http.StatusNoContent}
	if up.key == "" {
		return nil, &reqError{"InvalidArgument", "Bucket POST must contain a field named key.", http.StatusBadRequest}
	}
	value := func(name string) string {
		if name == "bucket" {
			return bucket
		}
		return f.fields[name]
	}
	covered := map[string]bool{}
	for _, c := range policy.Conditions {
		if re := applyPolicyCondition(c, value, covered, up); re != nil {
			return nil, re
		}
	}
	// Every field the form sends must be one the policy names, so nothing
	// unsigned can ride along (AWS: "Extra input fields").
	for name := range f.fields {
		switch {
		case name == "policy", name == "x-amz-signature", name == "file", strings.HasPrefix(name, "x-ignore-"):
		case !covered[name]:
			return nil, policyDenied("Invalid according to Policy: Extra input fields: " + name)
		}
	}

	// A field this server does not act on is refused, not dropped: dropping
	// an SSE-C key stored the object in the clear, readable without the key
	// the uploader asked for.
	for name, v := range f.fields {
		if !supportedPolicyField(name, v) {
			return nil, &reqError{"NotImplemented", "The form field " + name + " is not supported by this server.", http.StatusNotImplemented}
		}
	}

	// minio-go always sends the bucket as a field. It has to be the bucket
	// the form is posted to, which is the one the policy is checked against.
	if b, ok := f.fields["bucket"]; ok && b != bucket {
		return nil, &reqError{"InvalidArgument", "The bucket field does not name the bucket this form is posted to.", http.StatusBadRequest}
	}

	if strings.Contains(up.key, "${filename}") {
		up.key = strings.ReplaceAll(up.key, "${filename}", f.filename)
	}
	// Only the form field, which the policy has to name. The file part's
	// own Content-Type header is not covered by the signature, so taking it
	// let the uploader choose a type the signer never allowed (text/html).
	up.contentType = f.fields["content-type"]
	if up.contentType == "" {
		up.contentType = "application/octet-stream"
	}
	up.headers = objectHeaders{
		cacheControl:       f.fields["cache-control"],
		contentDisposition: f.fields["content-disposition"],
		contentEncoding:    f.fields["content-encoding"],
		contentLanguage:    f.fields["content-language"],
	}
	for name, v := range f.fields {
		if meta, ok := strings.CutPrefix(name, "x-amz-meta-"); ok && meta != "" {
			if up.userMeta == nil {
				up.userMeta = map[string]string{}
			}
			up.userMeta[meta] = v
		}
	}
	if raw := f.fields["tagging"]; raw != "" {
		// The form carries the tag set as the Tagging XML document that
		// PutObjectTagging takes, not as the header's query string.
		var tr taggingRequest
		if err := xml.Unmarshal([]byte(raw), &tr); err != nil {
			return nil, &reqError{"MalformedXML", "The tagging field is not a valid Tagging document.", http.StatusBadRequest}
		}
		if err := checkTagSet(tr.TagSet.Tags); err != nil {
			code := "InvalidTag"
			var te *tagError
			if errors.As(err, &te) {
				code = te.code
			}
			return nil, &reqError{code, err.Error(), http.StatusBadRequest}
		}
		up.tags = make(map[string]string, len(tr.TagSet.Tags))
		for _, tag := range tr.TagSet.Tags {
			up.tags[tag.Key] = tag.Value
		}
	}
	if re := policyChecksums(f.fields, up); re != nil {
		return nil, re
	}
	switch f.fields["success_action_status"] {
	case "200":
		up.successStatus = http.StatusOK
	case "201":
		up.successStatus = http.StatusCreated
	}
	if redirect := f.fields["success_action_redirect"]; redirect != "" {
		if u, err := url.Parse(redirect); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
			up.redirect = redirect
		}
	}
	return up, nil
}

// postChecksumFields maps each x-amz-checksum-* form field to the request
// header the body verifier checks, and the algorithm name the
// x-amz-checksum-algorithm field uses for it.
var postChecksumFields = []struct{ field, header, algorithm string }{
	{"x-amz-checksum-sha256", "X-Amz-Checksum-Sha256", "SHA256"},
	{"x-amz-checksum-sha1", "X-Amz-Checksum-Sha1", "SHA1"},
	{"x-amz-checksum-crc32", "X-Amz-Checksum-Crc32", "CRC32"},
	{"x-amz-checksum-crc32c", "X-Amz-Checksum-Crc32c", "CRC32C"},
	{"x-amz-checksum-crc64nvme", "X-Amz-Checksum-Crc64nvme", "CRC64NVME"},
}

// policyChecksums takes the form's checksum fields. The file is then checked
// against them as it streams, as PutObject checks its headers, and refused
// with BadDigest when it does not match. They used to be ignored, so a file
// changed on the way was stored as if it had matched.
func policyChecksums(fields map[string]string, up *policyUpload) *reqError {
	algorithm := strings.ToUpper(fields["x-amz-checksum-algorithm"])
	named := algorithm == ""
	for _, c := range postChecksumFields {
		v := fields[c.field]
		if v == "" {
			continue
		}
		if up.checksums == nil {
			up.checksums = http.Header{}
		}
		up.checksums.Set(c.header, v)
		if c.algorithm == algorithm {
			named = true
		}
	}
	if !named {
		return &reqError{"InvalidRequest", "x-amz-checksum-algorithm names " + algorithm + " but the form has no x-amz-checksum-" + strings.ToLower(algorithm) + " field.", http.StatusBadRequest}
	}
	return nil
}

// objectHeaders are the content headers a form upload may set on its object.
type objectHeaders struct {
	cacheControl, contentDisposition, contentEncoding, contentLanguage string
}

// supportedPolicyField reports whether a POST policy form field is one this
// server acts on (or, for acl, deliberately accepts and ignores, as
// PutObject does).
func supportedPolicyField(name, value string) bool {
	switch name {
	case "key", "policy", "x-amz-signature", "x-amz-algorithm", "x-amz-credential", "x-amz-date",
		"x-amz-security-token", "content-type", "cache-control", "content-disposition",
		"content-encoding", "content-language", "success_action_status", "success_action_redirect", "acl",
		"tagging", "x-amz-checksum-algorithm", "bucket":
		return true
	case "x-amz-server-side-encryption":
		// As PutObject's header: the object is encrypted as the server and
		// bucket are configured to (AES256 is what AWS's own form example
		// sends). A KMS key the form names cannot be honoured.
		return value == "AES256"
	case "x-amz-storage-class":
		return value == "" || value == "STANDARD"
	}
	for _, c := range postChecksumFields {
		if name == c.field {
			return true
		}
	}
	return strings.HasPrefix(name, "x-amz-meta-") || strings.HasPrefix(name, "x-ignore-")
}

// applyPolicyCondition checks one policy condition against the form. An
// unknown condition is refused: a policy that cannot be understood allows
// nothing.
func applyPolicyCondition(raw json.RawMessage, value func(string) string, covered map[string]bool, up *policyUpload) *reqError {
	invalid := &reqError{"InvalidPolicyDocument", "The policy has a condition this server does not understand: " + string(raw), http.StatusBadRequest}
	failed := func(name string) *reqError {
		return policyDenied("Invalid according to Policy: Policy Condition failed: " + name)
	}
	var exact map[string]json.RawMessage
	if json.Unmarshal(raw, &exact) == nil {
		for name, v := range exact {
			var want string
			if json.Unmarshal(v, &want) != nil {
				return invalid
			}
			lname := strings.ToLower(name)
			covered[lname] = true
			if value(lname) != want {
				return failed(lname)
			}
		}
		return nil
	}
	var cond []json.RawMessage
	if json.Unmarshal(raw, &cond) != nil || len(cond) != 3 {
		return invalid
	}
	var op string
	if json.Unmarshal(cond[0], &op) != nil {
		return invalid
	}
	switch strings.ToLower(op) {
	case "eq", "starts-with":
		var field, want string
		if json.Unmarshal(cond[1], &field) != nil || json.Unmarshal(cond[2], &want) != nil || !strings.HasPrefix(field, "$") {
			return invalid
		}
		lname := strings.ToLower(field[1:])
		covered[lname] = true
		got := value(lname)
		if strings.EqualFold(op, "eq") && got != want || strings.EqualFold(op, "starts-with") && !strings.HasPrefix(got, want) {
			return failed(lname)
		}
		return nil
	case "content-length-range":
		lo, okLo := policyInt(cond[1])
		hi, okHi := policyInt(cond[2])
		if !okLo || !okHi || lo < 0 || hi < lo {
			return invalid
		}
		up.minSize, up.maxSize = lo, min(hi, maxPostObject)
		return nil
	}
	return invalid
}

// policyInt reads a content-length-range bound, a JSON number or a numeric
// string.
func policyInt(raw json.RawMessage) (int64, bool) {
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// servePolicyPost authenticates and authorizes a POST policy form upload, then
// stores its file.
func (h *Handler) servePolicyPost(w http.ResponseWriter, r *http.Request, bucket, rateLimitIP, clientIP string) {
	f, re := readPolicyForm(r)
	if re != nil {
		writeS3Error(w, re.code, re.msg, re.status)
		return
	}
	up, re := h.checkPolicyForm(r, bucket, f)
	if re != nil {
		slog.Info("s3: form upload refused", "bucket", bucket, "code", re.code, "reason", re.msg, "remote", rateLimitIP)
		if h.onAudit != nil {
			h.onAudit("", "", "s3:PutObject", formatResource(bucket, ""), "Deny", clientIP, re.status)
		}
		writeS3Error(w, re.code, re.msg, re.status)
		return
	}
	id := up.identity
	resource := formatResource(bucket, up.key)
	deny := func(reason string) {
		slog.Info("s3: form upload refused", "bucket", bucket, "key", up.key, "reason", reason, "access_key", id.AccessKey)
		if h.onAudit != nil {
			h.onAudit(id.AccessKey, id.UserID, "s3:PutObject", resource, "Deny", clientIP, http.StatusForbidden)
		}
		writeS3Error(w, "AccessDenied", "Access Denied", http.StatusForbidden)
	}
	if err := h.auth.CheckIPAccess(id, rateLimitIP); err != nil {
		deny(err.Error())
		return
	}
	reqCtx := requestConditionContext(r)
	if id.IsAdmin {
		h.auth.ExternalAuth().NoteAdminBypass()
	} else if err := h.auth.AuthorizeWithContext(id, "s3:PutObject", resource, reqCtx); err != nil {
		deny(err.Error())
		return
	}
	// The form's fields were read under the listener's header limit, since
	// nobody was authenticated yet. The file now streams with no deadline,
	// like any authenticated upload.
	if release, ok := r.Context().Value(bodyDeadlineKey{}).(func()); ok {
		release()
	}
	// In a cluster, the key's own holder stores it, as a PutObject to that
	// key would be routed. The form goes on rebuilt from the fields already
	// read and the file still arriving, and the holder checks it again.
	if h.clusterProxy != nil && r.Header.Get("X-VaultS3-Proxy") == "" {
		fwd := rebuiltPolicyPost(r, f)
		forwarded := h.clusterProxy(w, fwd, bucket, up.key)
		fwd.Body.Close()
		if forwarded {
			// The holder checks the form again, and charges and audits the
			// upload itself, as for any forwarded request.
			return
		}
	}
	if h.draining() {
		writeS3Error(w, "SlowDown", "This node is draining and not accepting writes", http.StatusServiceUnavailable)
		return
	}
	if h.rateLimiter != nil && !h.allowKey(id.AccessKey) {
		w.Header().Set("Retry-After", "1")
		writeS3Error(w, "SlowDown", "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if h.onAudit != nil {
		h.onAudit(id.AccessKey, id.UserID, "s3:PutObject", resource, "Allow", clientIP, 0)
	}
	h.objects.storePolicyUpload(w, withIdentity(r, id, reqCtx), bucket, up, f.file)
}

// rebuiltPolicyPost is r with its form put back together from the fields
// readPolicyForm took and the rest of the file, streamed, for forwarding to
// another node. Fields after the file are not carried: no node reads them.
// Nothing is read from the file until the forwarded body is read, so a
// request the cluster keeps on this node still has its whole file. The
// caller closes the body once forwarding returns.
func rebuiltPolicyPost(r *http.Request, f *policyForm) *http.Request {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	body := &lazyForm{pr: pr}
	body.start = func() {
		go func() {
			err := func() error {
				for name, v := range f.fields {
					if err := mw.WriteField(name, v); err != nil {
						return err
					}
				}
				hdr := textproto.MIMEHeader{}
				hdr.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": f.filename}))
				if ct := f.file.Header.Get("Content-Type"); ct != "" {
					hdr.Set("Content-Type", ct)
				}
				part, err := mw.CreatePart(hdr)
				if err != nil {
					return err
				}
				if _, err := io.Copy(part, f.file); err != nil {
					return err
				}
				return mw.Close()
			}()
			pw.CloseWithError(err)
		}()
	}
	out := r.Clone(r.Context())
	out.Body = body
	// Clone copies the marker MultipartReader left on r, which would make the
	// receiving handler refuse to read the form again.
	out.MultipartForm = nil
	out.ContentLength = -1
	out.Header.Del("Content-Length")
	out.Header.Set("Content-Type", mw.FormDataContentType())
	return out
}

// lazyForm starts writing the rebuilt form on its first Read.
type lazyForm struct {
	pr    *io.PipeReader
	start func()
	once  sync.Once
}

func (l *lazyForm) Read(p []byte) (int, error) {
	l.once.Do(l.start)
	return l.pr.Read(p)
}

// Close ends the writer too, if it started, so it never waits for a reader
// that left.
func (l *lazyForm) Close() error {
	return l.pr.CloseWithError(errors.New("forwarded form closed"))
}

// objectLocation is the path of an object for a Location answer, with each
// segment of the key percent-encoded. It was the raw key, so a key with a
// space, '+', '?' or '#' gave a link to a different object or none.
func objectLocation(bucket, key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/" + bucket + "/" + strings.Join(segs, "/")
}

// sizedReader holds a form upload's file to its allowed size range and the
// bucket's quota, failing the read (so nothing is stored) when it goes past
// either or ends short of the minimum.
type sizedReader struct {
	r          io.Reader
	n          int64
	min, max   int64
	quotaLimit int64 // -1 for none
}

func (s *sizedReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.n += int64(n)
	if s.quotaLimit >= 0 && s.n > s.quotaLimit {
		return n, &reqError{"QuotaExceeded", "Maximum bucket size exceeded", http.StatusForbidden}
	}
	if s.n > s.max {
		return n, &reqError{"EntityTooLarge", "Your proposed upload exceeds the maximum allowed size", http.StatusBadRequest}
	}
	if err == io.EOF && s.n < s.min {
		return n, &reqError{"EntityTooSmall", "Your proposed upload is smaller than the minimum allowed size", http.StatusBadRequest}
	}
	return n, err
}

// storePolicyUpload stores the file of an authorized POST policy upload.
func (h *ObjectHandler) storePolicyUpload(w http.ResponseWriter, r *http.Request, bucket string, up *policyUpload, file io.Reader) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	if msg := objectKeyProblem(up.key); msg != "" {
		writeS3Error(w, "InvalidArgument", msg, http.StatusBadRequest)
		return
	}
	if h.space.Low() {
		writeInsufficientStorage(w)
		return
	}
	// The key is in the form, past the router's length check.
	if keyTooLong(up.key) && !objectStored(h.store, bucket, up.key) {
		writeS3Error(w, "KeyTooLongError", "Your key is too long", http.StatusBadRequest)
		return
	}
	if !h.checkQuotaFor(w, bucket, up.key, -1) {
		return
	}
	var body io.Reader = &sizedReader{r: file, min: up.minSize, max: up.maxSize, quotaLimit: h.quotaByteLimitFor(bucket, up.key)}
	var digests *putDigests
	if up.checksums != nil {
		sums := &http.Request{Header: up.checksums, ContentLength: -1}
		digests = newPutDigests(sums, body)
		digests.checkInline(sums, -1)
		body = digests
	}
	meta, err := h.writeObject(newObject{
		bucket:  bucket,
		key:     up.key,
		body:    body,
		size:    -1,
		digests: digests,
		meta: metadata.ObjectMeta{
			Tags:               up.tags,
			ContentType:        up.contentType,
			UserMetadata:       up.userMeta,
			CacheControl:       up.headers.cacheControl,
			ContentDisposition: up.headers.contentDisposition,
			ContentEncoding:    up.headers.contentEncoding,
			ContentLanguage:    up.headers.contentLanguage,
		},
	})
	if err != nil {
		answerWriteError(w, err, bucket, up.key)
		return
	}

	w.Header().Set("ETag", meta.ETag)
	if meta.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", meta.VersionID)
	}
	h.setSSEHeader(w, bucket)
	location := objectLocation(bucket, up.key)
	h.notifyCreated("s3:ObjectCreated:Post", meta)
	if up.redirect != "" {
		u, _ := url.Parse(up.redirect)
		q := u.Query()
		q.Set("bucket", bucket)
		q.Set("key", up.key)
		q.Set("etag", meta.ETag)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
		return
	}
	w.Header().Set("Location", location)
	if up.successStatus != http.StatusCreated {
		w.WriteHeader(up.successStatus)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusCreated)
	_ = xml.NewEncoder(w).Encode(struct {
		XMLName  xml.Name `xml:"PostResponse"`
		Location string   `xml:"Location"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		ETag     string   `xml:"ETag"`
	}{Location: location, Bucket: bucket, Key: up.key, ETag: meta.ETag})
}
