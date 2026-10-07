package s3

import (
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// checkGetPreconditions checks If-Modified-Since, If-Unmodified-Since,
// If-Match, If-None-Match on GET/HEAD. Returns true if response was written (caller should return).
//
// RFC 7232 section 6 fixes the order: If-Match, and only when it is absent
// If-Unmodified-Since; then If-None-Match, and only when it is absent
// If-Modified-Since. The date conditions used to be applied on top of the ETag
// ones, so If-Match true with If-Unmodified-Since false answered 412, and
// If-None-Match false with If-Modified-Since false answered 304, where S3 and
// the RFC both serve the object.
func checkGetPreconditions(w http.ResponseWriter, r *http.Request, meta *metadata.ObjectMeta) bool {
	if meta == nil {
		return false
	}

	lastMod := time.Unix(meta.LastModified, 0).UTC()
	etag := meta.ETag

	if im := r.Header.Get("If-Match"); im != "" {
		// If-Match: 412 if ETag doesn't match
		if !etagMatch(im, etag) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return true
		}
	} else if ius := r.Header.Get("If-Unmodified-Since"); ius != "" {
		// If-Unmodified-Since: 412 if modified after
		if t, err := http.ParseTime(ius); err == nil {
			if lastMod.After(t) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return true
			}
		}
	}

	if inm := r.Header.Get("If-None-Match"); inm != "" {
		// If-None-Match: 304 if ETag matches
		if etagMatch(inm, etag) {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	} else if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		// If-Modified-Since: 304 if not modified
		if t, err := http.ParseTime(ims); err == nil {
			if !lastMod.After(t) {
				w.Header().Set("ETag", etag)
				w.WriteHeader(http.StatusNotModified)
				return true
			}
		}
	}

	return false
}

// checkPutPreconditions checks If-Match, If-None-Match on PUT for conditional writes.
// Returns true if response was written (caller should return).
func checkPutPreconditions(w http.ResponseWriter, r *http.Request, store metadata.StoreAPI, bucket, key string) bool {
	im := r.Header.Get("If-Match")
	inm := r.Header.Get("If-None-Match")
	if im == "" && inm == "" {
		return false
	}

	meta, _ := store.GetObjectMeta(bucket, key)

	if im != "" {
		if meta == nil || !etagMatch(im, meta.ETag) {
			writeS3Error(w, "PreconditionFailed", "At least one condition specified evaluated to false", http.StatusPreconditionFailed)
			return true
		}
	}

	if inm == "*" && meta != nil {
		writeS3Error(w, "PreconditionFailed", "At least one condition specified evaluated to false", http.StatusPreconditionFailed)
		return true
	}

	if inm != "" && inm != "*" && meta != nil && etagMatch(inm, meta.ETag) {
		writeS3Error(w, "PreconditionFailed", "At least one condition specified evaluated to false", http.StatusPreconditionFailed)
		return true
	}

	return false
}

// checkCopyPreconditions checks x-amz-copy-source-if-* headers.
// Returns true if response was written (caller should return).
//
// The pairs follow the same precedence as a GET: S3 copies when if-match holds
// even though if-unmodified-since does not, and refuses on if-none-match alone
// when it is present.
func checkCopyPreconditions(w http.ResponseWriter, r *http.Request, srcMeta *metadata.ObjectMeta) bool {
	if srcMeta == nil {
		return false
	}

	lastMod := time.Unix(srcMeta.LastModified, 0).UTC()
	etag := srcMeta.ETag

	if v := r.Header.Get("X-Amz-Copy-Source-If-Match"); v != "" {
		if !etagMatch(v, etag) {
			writeS3Error(w, "PreconditionFailed", "Copy source ETag does not match", http.StatusPreconditionFailed)
			return true
		}
	} else if v := r.Header.Get("X-Amz-Copy-Source-If-Unmodified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			if lastMod.After(t) {
				writeS3Error(w, "PreconditionFailed", "Copy source modified since specified time", http.StatusPreconditionFailed)
				return true
			}
		}
	}
	if v := r.Header.Get("X-Amz-Copy-Source-If-None-Match"); v != "" {
		if etagMatch(v, etag) {
			writeS3Error(w, "PreconditionFailed", "Copy source ETag matches", http.StatusPreconditionFailed)
			return true
		}
	} else if v := r.Header.Get("X-Amz-Copy-Source-If-Modified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			if !lastMod.After(t) {
				writeS3Error(w, "PreconditionFailed", "Copy source not modified since specified time", http.StatusPreconditionFailed)
				return true
			}
		}
	}

	return false
}

// etagMatch checks if an ETag matches a header value (handles comma-separated list).
func etagMatch(header, etag string) bool {
	if header == "*" {
		return true
	}
	for _, v := range strings.Split(header, ",") {
		v = strings.TrimSpace(v)
		v = strings.Trim(v, "\"")
		e := strings.Trim(etag, "\"")
		if v == e {
			return true
		}
	}
	return false
}

// validateContentMD5 checks Content-MD5 header against body.
// Returns true if validation failed and error was written.
func validateContentMD5(w http.ResponseWriter, contentMD5 string, body []byte) bool {
	if contentMD5 == "" {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(contentMD5)
	if err != nil || len(expected) != md5.Size {
		writeS3Error(w, "InvalidDigest", "Content-MD5 is invalid", http.StatusBadRequest)
		return true
	}
	actual := md5.Sum(body)
	for i := range expected {
		if expected[i] != actual[i] {
			writeS3Error(w, "BadDigest", "Content-MD5 does not match", http.StatusBadRequest)
			return true
		}
	}
	return false
}

// parseUserMetadata extracts x-amz-meta-* headers.
// Limits: max 100 metadata entries, max 2KB per key, max 8KB per value (S3 limits).
func parseUserMetadata(r *http.Request) map[string]string {
	meta := make(map[string]string)
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-meta-") && len(v) > 0 {
			name := strings.TrimPrefix(lk, "x-amz-meta-")
			if len(name) > 2048 || len(v[0]) > 8192 || len(meta) >= 100 {
				continue // skip oversized or excess metadata
			}
			meta[name] = v[0]
		}
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// setUserMetadataHeaders emits x-amz-meta-* headers on GET/HEAD.
func setUserMetadataHeaders(w http.ResponseWriter, meta *metadata.ObjectMeta) {
	for k, v := range meta.UserMetadata {
		// Emit the key lowercased like AWS does. Direct map assignment bypasses
		// Header.Set's Title-Case canonicalization (HTTP/2 lowercases anyway).
		w.Header()["x-amz-meta-"+strings.ToLower(k)] = []string{v}
	}
}

// setHTTPMetadataHeaders emits Content-Encoding, Content-Disposition, etc.
func setHTTPMetadataHeaders(w http.ResponseWriter, meta *metadata.ObjectMeta) {
	if meta.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", meta.ContentEncoding)
	}
	if meta.ContentDisposition != "" {
		w.Header().Set("Content-Disposition", meta.ContentDisposition)
	}
	if meta.CacheControl != "" {
		w.Header().Set("Cache-Control", meta.CacheControl)
	}
	if meta.ContentLanguage != "" {
		w.Header().Set("Content-Language", meta.ContentLanguage)
	}
	if meta.WebsiteRedirect != "" {
		w.Header().Set("X-Amz-Website-Redirect-Location", meta.WebsiteRedirect)
	}
	if meta.ReplicationStatus != "" {
		w.Header().Set("X-Amz-Replication-Status", meta.ReplicationStatus)
	}
}

// applyResponseOverrides overrides response headers from query params.
func applyResponseOverrides(w http.ResponseWriter, r *http.Request) {
	overrides := map[string]string{
		"response-content-type":        "Content-Type",
		"response-content-disposition": "Content-Disposition",
		"response-content-encoding":    "Content-Encoding",
		"response-content-language":    "Content-Language",
		"response-cache-control":       "Cache-Control",
		"response-expires":             "Expires",
	}
	q := r.URL.Query()
	for param, header := range overrides {
		if v := q.Get(param); v != "" {
			w.Header().Set(header, v)
		}
	}
}

// maxObjectTags is the number of tags S3 allows on one object. PutObjectTagging
// enforces it on the XML body, so the header path has to agree.
const maxObjectTags = 10

// parseInlineTags parses the x-amz-tagging header, whose tag set is encoded as
// URL query parameters, so both keys and values arrive percent-encoded and a '+'
// stands for a space. Decoding is not optional: a value the client had to encode
// (a space, '&', '=', anything non-ASCII) is stored in the object's metadata and
// handed back by GetObjectTagging, so skipping it corrupts the tag permanently
// rather than only for the one response (issue #61).
//
// A tag set that cannot be decoded is rejected rather than half-applied, since a
// silently wrong tag gives the client nothing to notice.
func parseInlineTags(r *http.Request) (map[string]string, error) {
	tagging := r.Header.Get("X-Amz-Tagging")
	if tagging == "" {
		return nil, nil
	}
	// ParseQuery also refuses a ';' as a pair separator, which is not a character
	// S3 allows in a tag anyway, so say which of the two it was rather than
	// blaming the encoding for a rejection the encoding did not cause.
	values, err := url.ParseQuery(tagging)
	if err != nil {
		if strings.Contains(tagging, ";") {
			return nil, &tagError{
				code: "InvalidTag",
				msg:  "The x-amz-tagging header cannot contain a ';'. Separate tags with '&' and percent-encode a ';' inside a value as %3B.",
			}
		}
		return nil, &tagError{
			code: "InvalidArgument",
			msg:  fmt.Sprintf("The x-amz-tagging header is not a valid URL-encoded tag set: %v", err),
		}
	}
	if len(values) > maxObjectTags {
		return nil, &tagError{
			code: "BadRequest",
			msg:  fmt.Sprintf("Object tags cannot be greater than %d", maxObjectTags),
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	for k, v := range values {
		if err := checkTagLength(k, v[len(v)-1]); err != nil {
			return nil, err
		}
	}
	tags := make(map[string]string, len(values))
	for k, v := range values {
		// On a repeated key PutObjectTagging keeps the last one, because it fills the
		// same map from the XML tag list in order. Agree with it: the two paths must
		// not disagree about what the same tag set means.
		tags[k] = v[len(v)-1]
	}
	return tags, nil
}

// Object tag limits, as AWS sets them.
const (
	maxTagKeyLength   = 128
	maxTagValueLength = 256
)

// checkTagLength refuses a tag whose key or value is longer than S3 allows.
// They used to be stored as sent.
func checkTagLength(key, value string) error {
	if utf8.RuneCountInString(key) > maxTagKeyLength {
		return &tagError{code: "InvalidTag", msg: fmt.Sprintf("The TagKey you have provided is too long, max %d", maxTagKeyLength)}
	}
	if utf8.RuneCountInString(value) > maxTagValueLength {
		return &tagError{code: "InvalidTag", msg: fmt.Sprintf("The TagValue you have provided is too long, max %d", maxTagValueLength)}
	}
	return nil
}

// checkTagSet validates the tag set of a PutObjectTagging request. Too many
// tags is InvalidTag here, where the x-amz-tagging header answers BadRequest,
// because that is what AWS answers on each path.
func checkTagSet(tags []xmlTag) error {
	if len(tags) > maxObjectTags {
		return &tagError{code: "InvalidTag", msg: fmt.Sprintf("Object tags cannot be greater than %d", maxObjectTags)}
	}
	for _, t := range tags {
		if err := checkTagLength(t.Key, t.Value); err != nil {
			return err
		}
	}
	return nil
}

// sortedTags lists a tag map in key order. Tags are stored as a map, so
// listing them straight from it returned a different order on every request.
func sortedTags(tags map[string]string) []xmlTag {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]xmlTag, 0, len(keys))
	for _, k := range keys {
		out = append(out, xmlTag{Key: k, Value: tags[k]})
	}
	return out
}

// setTaggingCountHeader reports how many tags an object carries on GET and
// HEAD, as AWS does with x-amz-tagging-count.
func setTaggingCountHeader(w http.ResponseWriter, meta *metadata.ObjectMeta) {
	if n := len(meta.Tags); n > 0 {
		w.Header().Set("X-Amz-Tagging-Count", strconv.Itoa(n))
	}
}

// setObjectLockHeaders reports an object's retention and legal hold on GET and
// HEAD, as AWS does. They were never sent, so a client could only learn that an
// object was locked by asking for ?retention and ?legal-hold separately.
func setObjectLockHeaders(w http.ResponseWriter, meta *metadata.ObjectMeta) {
	if meta.RetentionMode != "" && meta.RetentionUntil > 0 {
		w.Header().Set("X-Amz-Object-Lock-Mode", meta.RetentionMode)
		w.Header().Set("X-Amz-Object-Lock-Retain-Until-Date", time.Unix(meta.RetentionUntil, 0).UTC().Format(time.RFC3339))
	}
	if meta.LegalHold {
		w.Header().Set("X-Amz-Object-Lock-Legal-Hold", "ON")
	}
}

// tagError carries the S3 error code a bad tag set should answer with, so the
// header path reports the same code for the same mistake as PutObjectTagging.
type tagError struct {
	code string
	msg  string
}

func (e *tagError) Error() string { return e.msg }

// writeTagError answers a tag set the server refused to store.
func writeTagError(w http.ResponseWriter, err error) {
	var te *tagError
	if errors.As(err, &te) {
		writeS3Error(w, te.code, te.msg, http.StatusBadRequest)
		return
	}
	writeS3Error(w, "InvalidArgument", err.Error(), http.StatusBadRequest)
}
