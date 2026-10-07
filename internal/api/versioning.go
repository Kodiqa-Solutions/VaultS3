package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/versioning"
)

func (h *APIHandler) handleListVersions(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")

	if bucket == "" || key == "" {
		writeError(w, http.StatusBadRequest, "bucket and key are required")
		return
	}
	if err := validateObjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// List all versions for this bucket with key prefix
	versions, _, err := h.store.ListObjectVersions(bucket, key, "", "", 1000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Filter to exact key match only
	var filtered []map[string]interface{}
	for _, v := range versions {
		if v.Key != key {
			continue
		}
		filtered = append(filtered, map[string]interface{}{
			"versionId":    v.VersionID,
			"size":         v.Size,
			"lastModified": v.LastModified,
			"etag":         v.ETag,
			"isLatest":     v.IsLatest,
			"deleteMarker": v.DeleteMarker,
			"contentType":  v.ContentType,
		})
	}

	if filtered == nil {
		filtered = []map[string]interface{}{}
	}

	writeJSON(w, http.StatusOK, filtered)
}

func (h *APIHandler) handleVersionDiff(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	v1 := r.URL.Query().Get("v1")
	v2 := r.URL.Query().Get("v2")

	if bucket == "" || key == "" || v1 == "" || v2 == "" {
		writeError(w, http.StatusBadRequest, "bucket, key, v1, and v2 are required")
		return
	}
	if err := validateObjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := versioning.Diff(h.store, h.engine, bucket, key, v1, v2)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (h *APIHandler) handleVersionTags(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")

	if bucket == "" || key == "" {
		writeError(w, http.StatusBadRequest, "bucket and key are required")
		return
	}
	if err := validateObjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ts := versioning.NewTagStore(h.store)
	tags, err := ts.GetTags(bucket, key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tags == nil {
		tags = []versioning.VersionTag{}
	}
	writeJSON(w, http.StatusOK, tags)
}

func (h *APIHandler) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bucket    string `json:"bucket"`
		Key       string `json:"key"`
		VersionID string `json:"versionId"`
		Tag       string `json:"tag"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Bucket == "" || req.Key == "" || req.VersionID == "" || req.Tag == "" {
		writeError(w, http.StatusBadRequest, "bucket, key, versionId, and tag are required")
		return
	}
	if err := validateObjectKey(req.Key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ts := versioning.NewTagStore(h.store)
	tag := versioning.VersionTag{
		Name:      req.Tag,
		Bucket:    req.Bucket,
		Key:       req.Key,
		VersionID: req.VersionID,
	}
	if err := ts.PutTag(tag); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "created", "tag": req.Tag})
}

func (h *APIHandler) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	tagName := r.URL.Query().Get("tag")

	if bucket == "" || key == "" || tagName == "" {
		writeError(w, http.StatusBadRequest, "bucket, key, and tag are required")
		return
	}
	if err := validateObjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ts := versioning.NewTagStore(h.store)
	if err := ts.DeleteTag(bucket, key, tagName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "tag": tagName})
}

func (h *APIHandler) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bucket    string `json:"bucket"`
		Key       string `json:"key"`
		VersionID string `json:"versionId"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Bucket == "" || req.Key == "" || req.VersionID == "" {
		writeError(w, http.StatusBadRequest, "bucket, key, and versionId are required")
		return
	}
	if err := validateObjectKey(req.Key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Rollback means "make that version's content the latest again", which on
	// a versioned bucket is a new version, the way S3 does it with a copy. It
	// used to write the content to the plain object path with no version id,
	// leave the old latest still marked latest, and discard the metadata error.
	// Version listings then disagreed with GET, and the next upload, which only
	// preserved a previous latest that had a version id, turned the rolled back
	// content into an orphan for the reclaim scan to delete.
	if versioning, _ := h.store.GetBucketVersioning(req.Bucket); versioning != "Enabled" {
		writeError(w, http.StatusConflict, "rollback needs versioning enabled on the bucket, so the content it replaces is kept as a version")
		return
	}

	oldMeta, err := h.store.GetObjectVersion(req.Bucket, req.Key, req.VersionID)
	if err != nil || oldMeta == nil {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if oldMeta.DeleteMarker {
		writeError(w, http.StatusBadRequest, "that version is a delete marker and has no content to roll back to")
		return
	}

	reader, size, err := h.getVersionData(req.Bucket, req.Key, req.VersionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "version data not found")
		return
	}
	defer reader.Close()

	newVersionID := genVersionID()
	written, etag, err := h.engine.PutObjectVersion(req.Bucket, req.Key, newVersionID, reader, size)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not write the new version: "+err.Error())
		return
	}

	// The new version carries the old one's content and content headers. Lock
	// state is not copied, as with an S3 copy: the bucket default applies.
	newMeta := *oldMeta
	newMeta.VersionID = newVersionID
	newMeta.ETag = etag
	newMeta.Size = written
	newMeta.IsLatest = true
	newMeta.LastModified = time.Now().UTC().Unix()
	newMeta.LegalHold = false
	newMeta.RetentionMode = ""
	newMeta.RetentionUntil = 0
	newMeta.VectorClock = nil
	newMeta.ReplicationStatus = ""
	if b, err := h.store.GetBucket(req.Bucket); err == nil && b != nil &&
		b.DefaultRetentionMode != "" && b.DefaultRetentionDays > 0 {
		newMeta.RetentionMode = b.DefaultRetentionMode
		newMeta.RetentionUntil = newMeta.LastModified + int64(b.DefaultRetentionDays*86400)
	}
	if err := h.recordNewLatestVersion(newMeta); err != nil {
		if derr := h.engine.DeleteObjectVersion(req.Bucket, req.Key, newVersionID); derr != nil {
			slog.Warn("could not remove the bytes of a rollback whose metadata failed",
				"bucket", req.Bucket, "key", req.Key, "error", derr)
		}
		writeError(w, http.StatusInternalServerError, "rollback not recorded: "+err.Error())
		return
	}
	if h.onReplication != nil {
		h.onReplication("s3:ObjectCreated:Put", req.Bucket, req.Key, written, etag, newVersionID)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":    "rolled back",
		"bucket":    req.Bucket,
		"key":       req.Key,
		"from":      req.VersionID,
		"versionId": newVersionID,
		"size":      written,
		"etag":      etag,
	})
}
