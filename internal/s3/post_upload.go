package s3

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// PostUpload handles POST /{bucket} for a form upload signed with an
// Authorization header. One signed with a POST policy is servePolicyPost.
func (h *ObjectHandler) PostUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	if err := r.ParseMultipartForm(128 * 1024 * 1024); err != nil {
		if !writeReqError(w, err) {
			writeS3Error(w, "MalformedPOSTRequest", "Could not parse multipart form", http.StatusBadRequest)
		}
		return
	}
	// The form parser stops at the closing boundary, and its buffered reader
	// can swallow an error returned with the body's last bytes. So read on to
	// the end of the body: a body checked against a signed SHA-256 or signed
	// chunks reports its failure there (the checks keep reporting it), and
	// must before anything is stored. A checked body is read to its end
	// however long the rest is, or a long epilogue would skip the check; any
	// other body only up to 1 MiB.
	var rest io.Reader = io.LimitReader(r.Body, 1<<20)
	if bodyIsVerified(r.Body) {
		rest = r.Body
	}
	if _, err := io.Copy(io.Discard, rest); err != nil {
		if !writeReqError(w, err) {
			writeS3Error(w, "IncompleteBody", "The request body could not be read", http.StatusBadRequest)
		}
		return
	}

	key := r.FormValue("key")
	if key == "" {
		writeS3Error(w, "InvalidArgument", "Missing required field: key", http.StatusBadRequest)
		return
	}

	// Replace ${filename} in key template
	file, header, err := r.FormFile("file")
	if err != nil {
		writeS3Error(w, "InvalidArgument", "Missing required field: file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if strings.Contains(key, "${filename}") {
		key = strings.ReplaceAll(key, "${filename}", header.Filename)
	}
	// The key comes from a form field, not the request path, so the router never
	// saw it. "../victim/a.txt" here wrote into another bucket.
	if msg := objectKeyProblem(key); msg != "" {
		writeS3Error(w, "InvalidArgument", msg, http.StatusBadRequest)
		return
	}

	// A policy in the form is not consulted here: this request was signed
	// with an Authorization header and authorized like any other. A form
	// signed with a policy instead goes through servePolicyPost.

	// The router could only authorize the bucket, because the key is a form
	// field. Authorize the key itself now that it is known.
	if err := h.authorizeEntry(r, "s3:PutObject", formatResource(bucket, key)); err != nil {
		slog.Info("s3: form upload refused by policy", "bucket", bucket, "key", key, "reason", err.Error())
		writeS3Error(w, "AccessDenied", "Access Denied", http.StatusForbidden)
		return
	}

	// Check quota
	if !h.checkQuota(w, bucket, header.Size) {
		return
	}

	ct := header.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}

	// Through writeObject like every other write, so a form upload honours the
	// bucket's versioning and cannot replace a locked object.
	meta, err := h.writeObject(newObject{
		bucket:   bucket,
		key:      key,
		body:     file,
		size:     header.Size,
		meta:     metadata.ObjectMeta{ContentType: ct},
		lockFrom: r,
	})
	if err != nil {
		answerWriteError(w, err, bucket, key)
		return
	}

	// The engine's ETag is already quoted. Quoting it again sent the client
	// ""<md5>"", which no SDK recognises as the ETag of what it uploaded.
	w.Header().Set("ETag", meta.ETag)
	if meta.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", meta.VersionID)
	}
	w.Header().Set("Location", fmt.Sprintf("/%s/%s", bucket, key))
	w.WriteHeader(http.StatusNoContent)
	h.notifyCreated("s3:ObjectCreated:Post", meta)
}
