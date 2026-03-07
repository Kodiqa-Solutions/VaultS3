package s3

import (
	"crypto/md5"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// checkGetPreconditions checks If-Modified-Since, If-Unmodified-Since,
// If-Match, If-None-Match on GET/HEAD. Returns true if response was written (caller should return).
func checkGetPreconditions(w http.ResponseWriter, r *http.Request, meta *metadata.ObjectMeta) bool {
	if meta == nil {
		return false
	}

	lastMod := time.Unix(meta.LastModified, 0).UTC()
	etag := meta.ETag

	// If-Match: 412 if ETag doesn't match
	if im := r.Header.Get("If-Match"); im != "" {
		if !etagMatch(im, etag) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return true
		}
	}

	// If-None-Match: 304 if ETag matches
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if etagMatch(inm, etag) {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}

	// If-Modified-Since: 304 if not modified
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil {
			if !lastMod.After(t) {
				w.Header().Set("ETag", etag)
				w.WriteHeader(http.StatusNotModified)
				return true
			}
		}
	}

	// If-Unmodified-Since: 412 if modified after
	if ius := r.Header.Get("If-Unmodified-Since"); ius != "" {
		if t, err := http.ParseTime(ius); err == nil {
			if lastMod.After(t) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return true
			}
		}
	}

	return false
}

// checkPutPreconditions checks If-Match, If-None-Match on PUT for conditional writes.
// Returns true if response was written (caller should return).
func checkPutPreconditions(w http.ResponseWriter, r *http.Request, store *metadata.Store, bucket, key string) bool {
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
	}
	if v := r.Header.Get("X-Amz-Copy-Source-If-None-Match"); v != "" {
		if etagMatch(v, etag) {
			writeS3Error(w, "PreconditionFailed", "Copy source ETag matches", http.StatusPreconditionFailed)
			return true
		}
	}
	if v := r.Header.Get("X-Amz-Copy-Source-If-Modified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			if !lastMod.After(t) {
				writeS3Error(w, "PreconditionFailed", "Copy source not modified since specified time", http.StatusPreconditionFailed)
				return true
			}
		}
	}
	if v := r.Header.Get("X-Amz-Copy-Source-If-Unmodified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			if lastMod.After(t) {
				writeS3Error(w, "PreconditionFailed", "Copy source modified since specified time", http.StatusPreconditionFailed)
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
		w.Header().Set("X-Amz-Meta-"+http.CanonicalHeaderKey(k), v)
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

// parseInlineTags parses the x-amz-tagging header (URL-encoded key=value pairs).
func parseInlineTags(r *http.Request) map[string]string {
	tagging := r.Header.Get("X-Amz-Tagging")
	if tagging == "" {
		return nil
	}
	tags := make(map[string]string)
	for _, pair := range strings.Split(tagging, "&") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			tags[kv[0]] = kv[1]
		}
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}
