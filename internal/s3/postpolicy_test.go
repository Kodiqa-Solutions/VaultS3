package s3

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// policyPost builds a POST policy form the way the SDKs' presigned POST does:
// the policy names the bucket, the key prefix, the signing fields and a size
// range, and is signed with the access key's SigV4 key.
type policyPost struct {
	accessKey, secretKey string
	bucket, key          string
	prefix               string
	expires              time.Time
	minSize, maxSize     int
	extraConds           []any
	fields               map[string]string // more form fields, sent as is
	upper                bool              // send the signing fields in mixed case
	file                 []byte
	signFor              string            // sign a different policy than the one sent (tamper)
	fileType             string            // Content-Type header of the file part itself
	nameless             int               // bytes of a part with no form name, sent first
	covered              map[string]string // fields sent AND named by an exact condition
	amzDate              string            // x-amz-date to send and sign, instead of now
}

func (p policyPost) build(t *testing.T) (string, []byte) {
	t.Helper()
	now := time.Now().UTC()
	date := now.Format("20060102")
	cred := fmt.Sprintf("%s/%s/%s/s3/aws4_request", p.accessKey, date, testRegion)
	amzDate := now.Format("20060102T150405Z")
	if p.amzDate != "" {
		amzDate = p.amzDate
	}
	if p.expires.IsZero() {
		p.expires = now.Add(time.Hour)
	}
	conds := []any{
		map[string]string{"bucket": p.bucket},
		[]any{"starts-with", "$key", p.prefix},
		map[string]string{"x-amz-algorithm": "AWS4-HMAC-SHA256"},
		map[string]string{"x-amz-credential": cred},
		map[string]string{"x-amz-date": amzDate},
		[]any{"content-length-range", p.minSize, p.maxSize},
	}
	conds = append(conds, p.extraConds...)
	for k, v := range p.covered {
		conds = append(conds, map[string]string{k: v})
	}
	doc, _ := json.Marshal(map[string]any{"expiration": p.expires.Format(time.RFC3339), "conditions": conds})
	policy := base64.StdEncoding.EncodeToString(doc)
	signed := policy
	if p.signFor != "" {
		signed = p.signFor
	}
	mac := hmac.New(sha256.New, deriveSigningKey(p.secretKey, date, testRegion, "s3"))
	mac.Write([]byte(signed))
	sig := hex.EncodeToString(mac.Sum(nil))

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	name := func(n string) string {
		if p.upper {
			return strings.ToUpper(n[:1]) + n[1:]
		}
		return n
	}
	if p.nameless > 0 {
		pw, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
		_, _ = pw.Write(bytes.Repeat([]byte("n"), p.nameless))
	}
	_ = mw.WriteField("key", p.key)
	_ = mw.WriteField(name("x-amz-algorithm"), "AWS4-HMAC-SHA256")
	_ = mw.WriteField(name("x-amz-credential"), cred)
	_ = mw.WriteField(name("x-amz-date"), amzDate)
	_ = mw.WriteField(name("policy"), policy)
	_ = mw.WriteField(name("x-amz-signature"), sig)
	for k, v := range p.fields {
		_ = mw.WriteField(k, v)
	}
	for k, v := range p.covered {
		_ = mw.WriteField(k, v)
	}
	fileHdr := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="photo.jpg"`}}
	if p.fileType != "" {
		fileHdr.Set("Content-Type", p.fileType)
	}
	fw, _ := mw.CreatePart(fileHdr)
	_, _ = fw.Write(p.file)
	_ = mw.Close()
	return mw.FormDataContentType(), b.Bytes()
}

func postForm(t *testing.T, base, bucket, ct string, body []byte) result {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/"+bucket, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	return send(t, req)
}

// Regression (5.0.11): a browser form upload signed with a POST policy (the
// SDKs' presigned POST) was refused at authentication, because the router
// required an Authorization header the form never has. It now works, and is
// checked as strictly as AWS checks it.
func TestPresignedPostUploads(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/ppost", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/pother", "").must(t, "other bucket")
	base := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "ppost", key: "uploads/a.txt",
		prefix: "uploads/", minSize: 1, maxSize: 1000, file: []byte("hello from a browser")}

	ct, body := base.build(t)
	if r := postForm(t, e.ts.URL, "ppost", ct, body); r.code != http.StatusNoContent {
		t.Fatalf("honest presigned POST: %d %s, want 204", r.code, r.body)
	}
	if got := e.admin(t, http.MethodGet, "/ppost/uploads/a.txt", ""); got.body != "hello from a browser" {
		t.Fatalf("stored %d %q", got.code, got.body)
	}

	upper := base
	upper.upper, upper.key = true, "uploads/upper.txt"
	ct, body = upper.build(t)
	if r := postForm(t, e.ts.URL, "ppost", ct, body); r.code != http.StatusNoContent {
		t.Fatalf("field names in another case (Policy, X-amz-signature): %d %s, want 204", r.code, r.body)
	}

	created := base
	created.key, created.fields = "uploads/c.txt", map[string]string{"success_action_status": "201"}
	created.extraConds = []any{map[string]string{"success_action_status": "201"}}
	ct, body = created.build(t)
	if r := postForm(t, e.ts.URL, "ppost", ct, body); r.code != http.StatusCreated || !strings.Contains(r.body, "<Key>uploads/c.txt</Key>") {
		t.Fatalf("success_action_status 201: %d %s, want 201 with a PostResponse", r.code, r.body)
	}

	refused := func(name string, p policyPost, bucket, code string) {
		t.Helper()
		ct, body := p.build(t)
		r := postForm(t, e.ts.URL, bucket, ct, body)
		if r.code < 400 || !strings.Contains(r.body, code) {
			t.Errorf("%s: %d %s, want %s", name, r.code, r.body, code)
		}
		if got := e.admin(t, http.MethodGet, "/"+bucket+"/"+p.key, ""); got.code != http.StatusNotFound {
			t.Errorf("%s: %s/%s was stored", name, bucket, p.key)
		}
	}
	tampered := base
	tampered.key = "uploads/t.txt"
	tampered.signFor = "a policy the signer never saw"
	refused("signature over another policy", tampered, "ppost", "SignatureDoesNotMatch")

	refused("policy for one bucket used on another", policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "ppost",
		key: "uploads/x.txt", prefix: "uploads/", minSize: 1, maxSize: 1000, file: []byte("x")}, "pother", "AccessDenied")

	outside := base
	outside.key = "elsewhere/x.txt"
	refused("key outside the signed prefix", outside, "ppost", "AccessDenied")

	extra := base
	extra.key, extra.fields = "uploads/e.txt", map[string]string{"x-amz-meta-evil": "1"}
	refused("a field the policy does not name", extra, "ppost", "Extra input fields")

	expired := base
	expired.key, expired.expires = "uploads/old.txt", time.Now().Add(-time.Minute)
	refused("expired policy", expired, "ppost", "Policy expired")

	unknown := base
	unknown.key, unknown.extraConds = "uploads/u.txt", []any{[]any{"matches-regex", "$key", ".*"}}
	refused("a condition the server does not understand", unknown, "ppost", "InvalidPolicyDocument")

	big := base
	big.key, big.file = "uploads/big.txt", bytes.Repeat([]byte("b"), 1001)
	refused("file over content-length-range", big, "ppost", "EntityTooLarge")

	empty := base
	empty.key, empty.file = "uploads/empty.txt", nil
	refused("file under content-length-range", empty, "ppost", "EntityTooSmall")

	stranger := base
	stranger.key, stranger.accessKey = "uploads/s.txt", "NOSUCHKEY0000000000"
	refused("unknown access key", stranger, "ppost", "AccessDenied")

	// The signer may only write what IAM lets them: a valid policy from a key
	// without s3:PutObject on the key stores nothing.
	ak, sk := e.addUser(t, "reader", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::ppost/*"]}]}`)
	readOnly := base
	readOnly.key, readOnly.accessKey, readOnly.secretKey = "uploads/r.txt", ak, sk
	refused("signer without s3:PutObject", readOnly, "ppost", "AccessDenied")
}

// The policy, signature and permission are checked from the fields before the
// file, and the file is not read for a request that fails them: a bad
// signature is answered while the file is still arriving.
func TestPresignedPostIsRefusedBeforeTheFileIsRead(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pstall", "").must(t, "bucket")
	p := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "pstall", key: "uploads/a",
		prefix: "uploads/", minSize: 0, maxSize: 1 << 30, signFor: "not this policy", file: []byte("FILE-CONTENT-STARTS")}
	ct, body := p.build(t)
	// Cut inside the file's content: the closing boundary never arrives, so a
	// server that read the file before checking the signature never answers.
	head := body[:bytes.Index(body, []byte("FILE-CONTENT"))+4]
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/pstall", io.MultiReader(bytes.NewReader(head), &stallingReader{after: 1 << 20, release: release}))
	req.Header.Set("Content-Type", ct)
	done := make(chan result, 1)
	go func() { done <- send(t, req) }()
	select {
	case r := <-done:
		if !strings.Contains(r.body, "SignatureDoesNotMatch") {
			t.Fatalf("%d %s, want SignatureDoesNotMatch", r.code, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a form with a bad signature was not answered while its file was still arriving")
	}
}

// The fields before the file are bounded, so a form cannot make the server
// buffer much before its signature is checked.
func TestPresignedPostFieldsAreBounded(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pbound", "").must(t, "bucket")
	p := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "pbound", key: "uploads/a",
		prefix: "uploads/", minSize: 0, maxSize: 10, file: []byte("x"),
		fields: map[string]string{"x-ignore-pad": strings.Repeat("p", maxPolicyField+1)}}
	ct, body := p.build(t)
	if r := postForm(t, e.ts.URL, "pbound", ct, body); r.code != http.StatusBadRequest || !strings.Contains(r.body, "MaxPostPreDataLengthExceeded") {
		t.Fatalf("a field over %d bytes: %d %s, want 400 MaxPostPreDataLengthExceeded", maxPolicyField, r.code, r.body)
	}
}

// Regressions (5.0.11 review): a form field the server does not act on was
// dropped, so a form asking for SSE-C was stored in the clear and readable
// without the key; the file part's own Content-Type header, which the policy
// does not sign, chose the object's type (text/html); and parts with no name
// were read without counting toward the bound before the signature check.
func TestPresignedPostRefusesWhatItCannotHonour(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/phon", "").must(t, "bucket")
	base := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "phon", prefix: "u/",
		minSize: 0, maxSize: 100, file: []byte("customer secret")}

	ssec := base
	ssec.key = "u/ssec"
	ssec.covered = map[string]string{
		"x-amz-server-side-encryption-customer-algorithm": "AES256",
		"x-amz-server-side-encryption-customer-key":       "pO3upElrwuEXSoFwCfnZPdSsmt/xWeFa0N9KgDijwVs=",
		"x-amz-server-side-encryption-customer-key-md5":   "DWygnHRtgiJ77HCm+1rvHw==",
	}
	storage := base
	storage.key, storage.covered = "u/glacier", map[string]string{"x-amz-storage-class": "GLACIER"}
	for name, p := range map[string]policyPost{"SSE-C fields": ssec, "a storage class": storage} {
		ct, body := p.build(t)
		if r := postForm(t, e.ts.URL, "phon", ct, body); r.code != http.StatusNotImplemented || !strings.Contains(r.body, "NotImplemented") {
			t.Errorf("%s: %d %s, want 501 NotImplemented", name, r.code, r.body)
		}
		if got := e.admin(t, http.MethodGet, "/phon/"+p.key, ""); got.code != http.StatusNotFound {
			t.Errorf("%s: the object was stored anyway", name)
		}
	}

	html := base
	html.key, html.fileType = "u/page", "text/html"
	ct, body := html.build(t)
	postForm(t, e.ts.URL, "phon", ct, body).must(t, "form whose file part claims text/html")
	if got := e.admin(t, http.MethodHead, "/phon/u/page", ""); got.header.Get("Content-Type") == "text/html" {
		t.Errorf("stored with the file part's unsigned Content-Type text/html")
	}

	headers := base
	headers.key, headers.covered = "u/cached", map[string]string{"Cache-Control": "max-age=60", "Content-Type": "text/plain"}
	ct, body = headers.build(t)
	postForm(t, e.ts.URL, "phon", ct, body).must(t, "form with signed Cache-Control and Content-Type")
	got := e.admin(t, http.MethodHead, "/phon/u/cached", "")
	if got.header.Get("Cache-Control") != "max-age=60" || got.header.Get("Content-Type") != "text/plain" {
		t.Errorf("signed headers not stored: Cache-Control %q, Content-Type %q", got.header.Get("Cache-Control"), got.header.Get("Content-Type"))
	}

	padded := base
	padded.key, padded.nameless = "u/padded", maxPolicyFields+1
	ct, body = padded.build(t)
	if r := postForm(t, e.ts.URL, "phon", ct, body); r.code != http.StatusBadRequest || !strings.Contains(r.body, "MaxPostPreDataLengthExceeded") {
		t.Errorf("%d bytes of nameless parts: %d %s, want 400 MaxPostPreDataLengthExceeded", padded.nameless, r.code, r.body)
	}
}

// The tagging field and the x-amz-checksum-* fields are applied, not dropped:
// the tags are stored, and a file that does not match its checksum is refused.
func TestPresignedPostTagsAndChecksums(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/ptag", "").must(t, "bucket")
	file := []byte("form file body")
	base := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "ptag", prefix: "u/",
		minSize: 0, maxSize: 100, file: file}

	tagged := base
	tagged.key, tagged.covered = "u/tag", map[string]string{"tagging": "<Tagging><TagSet><Tag><Key>team</Key><Value>a b</Value></Tag></TagSet></Tagging>"}
	ct, body := tagged.build(t)
	postForm(t, e.ts.URL, "ptag", ct, body).must(t, "form with a tagging field")
	if got := e.admin(t, http.MethodGet, "/ptag/u/tag?tagging", ""); !strings.Contains(got.body, "<Key>team</Key><Value>a b</Value>") {
		t.Errorf("tags not stored: %s", got.body)
	}

	badTags := base
	badTags.key, badTags.covered = "u/badtag", map[string]string{"tagging": "not xml"}
	ct, body = badTags.build(t)
	if r := postForm(t, e.ts.URL, "ptag", ct, body); r.code != http.StatusBadRequest || !strings.Contains(r.body, "MalformedXML") {
		t.Errorf("unparsable tagging field: %d %s, want 400 MalformedXML", r.code, r.body)
	}

	sum := sha256.Sum256(file)
	right := base64.StdEncoding.EncodeToString(sum[:])
	wrongSum := sha256.Sum256([]byte("other bytes"))
	wrong := base64.StdEncoding.EncodeToString(wrongSum[:])

	good := base
	good.key, good.covered = "u/sum", map[string]string{"x-amz-checksum-algorithm": "SHA256", "x-amz-checksum-sha256": right}
	ct, body = good.build(t)
	postForm(t, e.ts.URL, "ptag", ct, body).must(t, "form with a matching checksum")
	if got := e.admin(t, http.MethodGet, "/ptag/u/sum", ""); got.body != string(file) {
		t.Errorf("matching checksum: stored %q", got.body)
	}

	bad := base
	bad.key, bad.covered = "u/badsum", map[string]string{"x-amz-checksum-algorithm": "SHA256", "x-amz-checksum-sha256": wrong}
	ct, body = bad.build(t)
	if r := postForm(t, e.ts.URL, "ptag", ct, body); r.code != http.StatusBadRequest || !strings.Contains(r.body, "BadDigest") {
		t.Errorf("non-matching checksum: %d %s, want 400 BadDigest", r.code, r.body)
	}
	if got := e.admin(t, http.MethodGet, "/ptag/u/badsum", ""); got.code != http.StatusNotFound {
		t.Errorf("a file that did not match its checksum was stored: %d %q", got.code, got.body)
	}

	missing := base
	missing.key, missing.covered = "u/nosum", map[string]string{"x-amz-checksum-algorithm": "CRC32"}
	ct, body = missing.build(t)
	if r := postForm(t, e.ts.URL, "ptag", ct, body); r.code != http.StatusBadRequest || !strings.Contains(r.body, "InvalidRequest") {
		t.Errorf("algorithm without its checksum field: %d %s, want 400 InvalidRequest", r.code, r.body)
	}
}

// Every check before the store refuses on its own: the eq condition, the IP
// allow list, the bucket quota while the file streams and an already full
// bucket, a duplicated field, the fields' total size and part count, SigV2,
// and an expired key whose signature is right.
func TestPresignedPostGuards(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pguard", "").must(t, "bucket")
	base := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "pguard", prefix: "u/",
		minSize: 0, maxSize: 1 << 20, file: []byte("hello")}
	refused := func(what string, p policyPost, status int, code string) {
		t.Helper()
		ct, body := p.build(t)
		r := postForm(t, e.ts.URL, "pguard", ct, body)
		if r.code != status || !strings.Contains(r.body, code) {
			t.Errorf("%s: %d %s, want %d %s", what, r.code, r.body, status, code)
		}
		if got := e.admin(t, http.MethodGet, "/pguard/"+p.key, ""); got.code != http.StatusNotFound {
			t.Errorf("%s: the object was stored anyway", what)
		}
	}

	eq := base
	eq.key, eq.extraConds, eq.fields = "u/eq", []any{[]any{"eq", "$x-amz-meta-team", "blue"}}, map[string]string{"x-amz-meta-team": "red"}
	refused("eq condition not met", eq, http.StatusForbidden, "AccessDenied")
	eqOK := eq
	eqOK.key, eqOK.fields = "u/eqok", map[string]string{"x-amz-meta-team": "blue"}
	ct, body := eqOK.build(t)
	postForm(t, e.ts.URL, "pguard", ct, body).must(t, "eq condition met")

	dup := base
	dup.key, dup.fields = "u/dup", map[string]string{"KEY": "u/other"}
	refused("a field sent twice (key and KEY)", dup, http.StatusBadRequest, "InvalidArgument")

	many := base
	many.key, many.fields = "u/many", map[string]string{}
	for i := 0; i*maxPolicyField < maxPolicyFields+maxPolicyField; i++ {
		many.fields[fmt.Sprintf("x-ignore-pad%d", i)] = strings.Repeat("p", maxPolicyField-10)
	}
	refused("fields over the total bound", many, http.StatusBadRequest, "MaxPostPreDataLengthExceeded")

	parts := base
	parts.key, parts.fields = "u/parts", map[string]string{}
	for i := 0; i <= maxPolicyParts; i++ {
		parts.fields[fmt.Sprintf("x-ignore-%d", i)] = ""
	}
	refused("more parts than the part bound", parts, http.StatusBadRequest, "MaxPostPreDataLengthExceeded")

	v2 := base
	v2.key, v2.fields = "u/v2", map[string]string{"AWSAccessKeyId": testAccessKey}
	refused("a SigV2 form", v2, http.StatusForbidden, "Signature Version 2")

	// An expired key with a right signature: refused after the signature.
	now := time.Now().UTC()
	if err := e.store.CreateAccessKey(metadata.AccessKey{AccessKey: "AKEXPIRED", SecretKey: "expired-secret", CreatedAt: now,
		UserID: "nobody", ExpiresAt: now.Add(-time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	expired := base
	expired.key, expired.accessKey, expired.secretKey = "u/expired", "AKEXPIRED", "expired-secret"
	refused("an expired key", expired, http.StatusForbidden, "AccessDenied")

	ak, sk := e.addUser(t, "pipuser", allow(`"s3:PutObject"`, `"arn:aws:s3:::pguard/*"`), "10.99.0.0/16")
	ip := base
	ip.key, ip.accessKey, ip.secretKey = "u/ip", ak, sk
	refused("a key allowed only from another network", ip, http.StatusForbidden, "AccessDenied")

	if err := e.store.UpdateBucketQuota("pguard", 1000, 0); err != nil {
		t.Fatal(err)
	}
	big := base
	big.key, big.file = "u/big", bytes.Repeat([]byte("b"), 5000)
	refused("a file past the bucket quota", big, http.StatusForbidden, "QuotaExceeded")

	if err := e.store.UpdateBucketQuota("pguard", 0, 1); err != nil {
		t.Fatal(err)
	}
	full := base
	full.key = "u/full"
	refused("a bucket at its object count (u/eqok is stored)", full, http.StatusForbidden, "QuotaExceeded")
}

// Regressions (5.0.11 compatibility review): minio-go always sends the bucket
// as a form field, and AWS's own form example asks for SSE-S3; both were
// refused with 501. The guards around them, the x-amz-date check, the
// redirect check, the tag set check and bytes in unnamed parts, each refuse
// on their own. Location is percent-encoded.
func TestPresignedPostSDKFieldsAndChecks(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/psdk", "").must(t, "bucket")
	base := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "psdk", prefix: "u/",
		minSize: 0, maxSize: 1 << 20, file: []byte("hello")}
	post := func(p policyPost) result {
		t.Helper()
		ct, body := p.build(t)
		return postForm(t, e.ts.URL, "psdk", ct, body)
	}
	stored := func(key string) bool {
		return e.admin(t, http.MethodHead, "/psdk/"+key, "").code == http.StatusOK
	}

	minio := base
	minio.key, minio.covered = "u/minio", map[string]string{"bucket": "psdk"}
	post(minio).must(t, "a form with the bucket field, as minio-go sends it")

	other := base
	other.key, other.fields, other.extraConds = "u/other", map[string]string{"bucket": "elsewhere"}, []any{[]any{"starts-with", "$bucket", ""}}
	if r := post(other); r.code != http.StatusBadRequest || stored("u/other") {
		t.Errorf("a bucket field naming another bucket: %d %s, want 400 and nothing stored", r.code, r.body)
	}

	sse := base
	sse.key, sse.covered = "u/sse", map[string]string{"x-amz-server-side-encryption": "AES256"}
	post(sse).must(t, "a form asking for SSE-S3")
	kms := base
	kms.key, kms.covered = "u/kms", map[string]string{"x-amz-server-side-encryption": "aws:kms"}
	if r := post(kms); r.code != http.StatusNotImplemented || stored("u/kms") {
		t.Errorf("a form asking for SSE-KMS: %d %s, want 501 and nothing stored", r.code, r.body)
	}

	stale := base
	stale.key, stale.amzDate = "u/stale", "20200101T000000Z"
	if r := post(stale); r.code != http.StatusForbidden || stored("u/stale") {
		t.Errorf("an x-amz-date that is not the credential's date: %d %s, want 403", r.code, r.body)
	}

	js := base
	js.key, js.covered = "u/js", map[string]string{"success_action_redirect": "javascript:alert(1)"}
	if r := post(js); r.code != http.StatusNoContent || strings.Contains(r.header.Get("Location"), "javascript") {
		t.Errorf("a javascript: redirect: %d, Location %q, want a plain 204 and no redirect", r.code, r.header.Get("Location"))
	}

	var tags strings.Builder
	tags.WriteString("<Tagging><TagSet>")
	for i := 0; i < 11; i++ {
		fmt.Fprintf(&tags, "<Tag><Key>k%d</Key><Value>v</Value></Tag>", i)
	}
	tags.WriteString("</TagSet></Tagging>")
	tooMany := base
	tooMany.key, tooMany.covered = "u/tags", map[string]string{"tagging": tags.String()}
	if r := post(tooMany); r.code != http.StatusBadRequest || stored("u/tags") {
		t.Errorf("11 tags: %d %s, want 400 and nothing stored", r.code, r.body)
	}

	unnamed := base
	unnamed.key, unnamed.fields = "u/unnamed", map[string]string{}
	unnamed.nameless = maxPolicyFields / 2
	// Just over half the bound in named fields: within it alone, past it with
	// the unnamed half.
	for i := 0; i < maxPolicyFields/2/maxPolicyField+1; i++ {
		unnamed.fields[fmt.Sprintf("x-ignore-%d", i)] = strings.Repeat("p", maxPolicyField-10)
	}
	if r := post(unnamed); r.code != http.StatusBadRequest || !strings.Contains(r.body, "MaxPostPreDataLengthExceeded") {
		t.Errorf("half the bound in an unnamed part plus fields over the rest: %d %s, want 400 MaxPostPreDataLengthExceeded", r.code, r.body)
	}
	// The named fields alone are within the bound.
	unnamed.key, unnamed.nameless = "u/named-only", 0
	post(unnamed).must(t, "the same fields without the unnamed part")

	created := base
	created.key, created.covered = "u/sp ace+plus#.txt", map[string]string{"success_action_status": "201"}
	r := post(created)
	if r.code != http.StatusCreated || r.header.Get("Location") != "/psdk/u/sp%20ace+plus%23.txt" {
		t.Errorf("Location for a key with a space and '#': %d %q, want /psdk/u/sp%%20ace+plus%%23.txt", r.code, r.header.Get("Location"))
	}
}

// Regression (5.0.11 round 3): in a cluster, a presigned POST was routed by
// its bucket alone, before its key was known, so it was stored on the
// bucket's node while GET looked for the key on that key's holders: the
// object was unreadable everywhere. It is now routed by the key in the form,
// to that key's holder, which checks the form again; one this node holds is
// stored here with its whole file.
func TestPresignedPostIsRoutedByItsKeyInACluster(t *testing.T) {
	entry, holder := newFixEnv(t), newFixEnv(t)
	for _, e := range []*fixEnv{entry, holder} {
		e.admin(t, http.MethodPut, "/pclu", "").must(t, "bucket")
	}
	var routed []string
	holderURL, _ := url.Parse(holder.ts.URL)
	entry.h.SetClusterProxy(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
		routed = append(routed, r.Method+" "+key)
		if r.Method != http.MethodPost || !strings.HasPrefix(key, "remote/") {
			return false
		}
		// Over HTTP, as the cluster proxy forwards, so the rebuilt request
		// has to be one a real transport can send.
		r.Header.Set("X-VaultS3-Proxy", "entry")
		httputil.NewSingleHostReverseProxy(holderURL).ServeHTTP(w, r)
		return true
	})
	var audits []string
	for name, e := range map[string]*fixEnv{"entry": entry, "holder": holder} {
		name := name
		e.h.SetAuditFunc(func(accessKey, userID, action, resource, effect, sourceIP string, status int) {
			if action == "s3:PutObject" && effect == "Allow" && strings.Contains(resource, "remote/") {
				audits = append(audits, name+" "+effect)
			}
		})
	}
	// Larger than the bound on everything before the file, which must not
	// apply to the file itself.
	file := bytes.Repeat([]byte("f"), 2*maxPolicyPreamble)
	post := func(key string) {
		t.Helper()
		p := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "pclu", key: key, prefix: "",
			minSize: 0, maxSize: 1 << 20, file: file, covered: map[string]string{"Content-Type": "text/plain", "x-amz-meta-team": "a"}}
		ct, body := p.build(t)
		postForm(t, entry.ts.URL, "pclu", ct, body).must(t, "presigned POST of "+key)
	}
	post("remote/a.txt")
	post("local/b.txt")

	// ${filename} is filled in identically on both nodes.
	fn := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "pclu", key: "remote/${filename}", prefix: "remote/",
		minSize: 0, maxSize: 1 << 20, file: []byte("named")}
	ct, body := fn.build(t)
	postForm(t, entry.ts.URL, "pclu", ct, body).must(t, "presigned POST with ${filename}")
	if got := holder.admin(t, http.MethodGet, "/pclu/remote/photo.jpg", ""); got.body != "named" {
		t.Errorf("remote/${filename} on its holder: %d %q", got.code, got.body)
	}

	// A draining entry node still forwards to a healthy holder, as it does a
	// PutObject, and refuses only what it would store itself.
	drain := &atomic.Bool{}
	entry.h.SetWritableFlag(drain)
	post("remote/while-draining.txt")
	p := policyPost{accessKey: testAccessKey, secretKey: testSecretKey, bucket: "pclu", key: "local/while-draining.txt",
		minSize: 0, maxSize: 1 << 20, file: file}
	ct, body = p.build(t)
	if r := postForm(t, entry.ts.URL, "pclu", ct, body); r.code != http.StatusServiceUnavailable {
		t.Errorf("a presigned POST this draining node would store: %d %s, want 503", r.code, r.body)
	}
	entry.h.SetWritableFlag(nil)

	if got := holder.admin(t, http.MethodGet, "/pclu/remote/a.txt", ""); got.code != http.StatusOK || got.body != string(file) || got.header.Get("Content-Type") != "text/plain" || got.header.Get("X-Amz-Meta-Team") != "a" {
		t.Errorf("remote/a.txt on its holder: %d, %d bytes, type %q, meta %q", got.code, len(got.body), got.header.Get("Content-Type"), got.header.Get("X-Amz-Meta-Team"))
	}
	if got := entry.admin(t, http.MethodHead, "/pclu/remote/a.txt", ""); got.code != http.StatusNotFound {
		t.Errorf("remote/a.txt was also stored on the entry node")
	}
	if got := entry.admin(t, http.MethodGet, "/pclu/local/b.txt", ""); got.code != http.StatusOK || got.body != string(file) {
		t.Errorf("local/b.txt on the entry node: %d, %d bytes, want the whole file", got.code, len(got.body))
	}
	// The forwarded upload is audited once, by the node that stored it.
	if strings.Join(audits, ",") != "holder Allow,holder Allow,holder Allow" {
		t.Errorf("audit entries for the three forwarded uploads: %v, want one each, from the holder", audits)
	}
	for _, r := range routed {
		if r == "POST " {
			t.Errorf("a presigned POST was routed by its bucket alone (empty key): %v", routed)
		}
	}
}

// Regression (5.0.11 round 5): mime/multipart holds up to 10 MiB of header
// for each part, read before the signature is checked, and the field bounds
// counted only values. 150 anonymous connections, each sending one 9 MiB part
// header, took a 700 MiB server down. Every byte before the file is now
// bounded, headers included.
func TestPresignedPostPartHeadersAreBounded(t *testing.T) {
	e := newFixEnv(t)
	var b bytes.Buffer
	b.WriteString("--XX\r\nContent-Disposition: form-data; name=\"key\"\r\n")
	line := "X-Pad: " + strings.Repeat("h", 2000) + "\r\n"
	for b.Len() < 4<<20 {
		b.WriteString(line)
	}
	b.WriteString("\r\nk\r\n--XX--\r\n")
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/anybucket", bytes.NewReader(b.Bytes()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=XX")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := send(t, req)
	runtime.ReadMemStats(&after)
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "MaxPostPreDataLengthExceeded") {
		t.Errorf("a 4 MiB part header: %d %s, want 400 MaxPostPreDataLengthExceeded", r.code, r.body)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 3<<20 {
		t.Errorf("a 4 MiB part header allocated %d bytes before it was refused, want well under its size", grew)
	}
}
