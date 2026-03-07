package s3

import (
	"archive/tar"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"time"

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
	var count int

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

		key := path.Clean(hdr.Name)
		if key == "." || key == "" {
			continue
		}

		// Check quota
		if !h.checkQuota(w, bucket, hdr.Size) {
			return
		}

		size, etag, err := h.engine.PutObject(bucket, key, tr, hdr.Size)
		if err != nil {
			slog.Error("snowball extract error", "key", key, "error", err)
			continue
		}

		ct := "application/octet-stream"
		h.store.PutObjectMeta(metadata.ObjectMeta{
			Bucket:       bucket,
			Key:          key,
			Size:         size,
			ETag:         etag,
			ContentType:  ct,
			LastModified: time.Now().UTC().UnixNano(),
		})

		if h.onNotification != nil {
			h.onNotification("s3:ObjectCreated:Put", bucket, key, size, etag, "")
		}
		if h.onSearchUpdate != nil {
			h.onSearchUpdate("put", bucket, key)
		}

		count++
	}

	w.Header().Set("X-Amz-Snowball-Extracted-Count", fmt.Sprintf("%d", count))
	w.WriteHeader(http.StatusOK)
}
