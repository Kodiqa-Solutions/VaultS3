package s3

import (
	"encoding/xml"
	"errors"
	"github.com/Kodiqa-Solutions/VaultS3/internal/tiering"
	"io"
	"log/slog"
	"net/http"
)

type restoreRequest struct {
	XMLName xml.Name `xml:"RestoreRequest"`
	Days    int      `xml:"Days"`
	Tier    string   `xml:"GlacierJobParameters>Tier,omitempty"`
}

// RestoreObject handles POST /{bucket}/{key}?restore.
func (h *ObjectHandler) RestoreObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	meta, err := h.store.GetObjectMeta(bucket, key)
	if err != nil || meta == nil {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}

	// Only the object's current data, or its null version, which lives at the
	// same path, is tiered.
	if v := r.URL.Query().Get("versionId"); v != "" && v != "null" && v != meta.VersionID {
		writeS3Error(w, "InvalidObjectState", "Only the current or null version of an object can be in the cold tier", http.StatusConflict)
		return
	}

	var req restoreRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeS3Error(w, "InternalError", "Failed to read body", http.StatusInternalServerError)
		return
	}
	if len(body) > 0 {
		if err := xml.Unmarshal(body, &req); err != nil {
			writeS3Error(w, "MalformedXML", "Invalid restore request", http.StatusBadRequest)
			return
		}
	}
	if req.Days <= 0 {
		req.Days = 1
	}

	// The data is moved back to the hot tier, where reads find it. This only
	// marked the record hot and left the bytes in the cold directory, so the
	// "restored" object still could not be read.
	// Asked even when the record says hot: 5.0.10's RestoreObject marked a
	// record hot and left the data in the cold tier, and this brings it back.
	if h.coldRestorer == nil {
		if meta.Tier == "cold" {
			writeS3Error(w, "InvalidObjectState", "The object is in the cold tier and tiering is not configured on this server, so it cannot be restored", http.StatusConflict)
			return
		}
		writeS3Error(w, "InvalidObjectState", "Object is not in a cold storage class", http.StatusConflict)
		return
	}
	if err := h.coldRestorer(bucket, key); errors.Is(err, tiering.ErrNothingToRestore) {
		writeS3Error(w, "InvalidObjectState", "Object is not in a cold storage class", http.StatusConflict)
		return
	} else if errors.Is(err, tiering.ErrHotPathBlocked) {
		writeS3Error(w, "InvalidObjectState", "The object cannot be restored: another object's folder now uses its path. Its cold copy is kept.", http.StatusConflict)
		return
	} else if err != nil {
		slog.Error("restore error", "bucket", bucket, "key", key, "error", err)
		writeS3Error(w, "InternalError", "Failed to restore the object from the cold tier", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}
