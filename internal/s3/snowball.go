package s3

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// SnowballUpload handles PUT /{bucket}/{key} with x-amz-meta-snowball-auto-extract: true.
// It extracts a TAR archive into individual objects in the bucket.
func (h *ObjectHandler) SnowballUpload(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	tr := tar.NewReader(r.Body)
	var count, refused int

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeS3Error(w, "InvalidArgument", fmt.Sprintf("TAR read error: %v", err), http.StatusBadRequest)
			return
		}

		// Skip directories
		if hdr.Typeflag == tar.TypeDir {
			continue
		}

		// Only regular files become objects. Links and devices carry no data.
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// The raw entry name is checked before it is cleaned. path.Clean keeps a
		// leading "..", and nothing checked it afterwards, so an entry named
		// "../victim/a.txt" overwrote another bucket's object. A refused entry is
		// counted and skipped, and the rest of the archive goes on. Cleaning
		// still turns "./a.txt", as `tar -C dir .` writes it, into "a.txt".
		if msg := objectKeyProblem(hdr.Name); msg != "" {
			slog.Warn("snowball: entry refused", "bucket", bucket, "entry", hdr.Name, "reason", msg)
			refused++
			continue
		}
		key := path.Clean(hdr.Name)
		if key == "." {
			continue
		}

		// The request was authorized for the archive's own key. Every entry is
		// a write to a key of its own, so each is authorized on that key.
		if err := h.authorizeEntry(r, "s3:PutObject", formatResource(bucket, key)); err != nil {
			slog.Warn("snowball: entry refused by policy", "bucket", bucket, "entry", key, "reason", err.Error())
			refused++
			continue
		}

		// Check quota
		if !h.checkQuota(w, bucket, hdr.Size) {
			return
		}

		// Each entry is written like any other object, so the import honours the
		// bucket's versioning and object lock. It used to write the plain path
		// directly, replacing locked objects and bypassing versioning.
		meta, err := h.writeObject(newObject{
			bucket:   bucket,
			key:      key,
			body:     tr,
			size:     hdr.Size,
			meta:     metadata.ObjectMeta{ContentType: "application/octet-stream"},
			lockFrom: nil,
		})
		if err != nil {
			// One entry of a batch import: record it and keep going, but never
			// count it as imported.
			slog.Error("snowball: entry not imported", "bucket", bucket, "key", key, "error", err)
			var re *reqError
			if errors.As(err, &re) {
				refused++
			}
			continue
		}

		if h.onNotification != nil {
			h.onNotification("s3:ObjectCreated:Put", bucket, key, meta.Size, meta.ETag, meta.VersionID)
		}
		if h.onSearchUpdate != nil {
			h.onSearchUpdate("put", bucket, key)
		}

		count++
	}

	w.Header().Set("X-Amz-Snowball-Extracted-Count", fmt.Sprintf("%d", count))
	w.Header().Set("X-Amz-Snowball-Refused-Count", fmt.Sprintf("%d", refused))
	w.WriteHeader(http.StatusOK)
}
