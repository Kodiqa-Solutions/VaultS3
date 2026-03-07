package api

import (
	"io"
	"net/http"

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

	// Read the specified version
	reader, size, err := h.engine.GetObjectVersion(req.Bucket, req.Key, req.VersionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	defer reader.Close()

	// Get metadata from the old version
	oldMeta, err := h.store.GetObjectVersion(req.Bucket, req.Key, req.VersionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "version metadata not found")
		return
	}

	// Create a new version with the old content (PutObject via engine)
	written, etag, err := h.engine.PutObject(req.Bucket, req.Key, io.Reader(reader), size)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update metadata to point to this as latest
	newMeta := *oldMeta
	newMeta.ETag = etag
	newMeta.Size = written
	newMeta.IsLatest = true
	newMeta.VersionID = ""
	h.store.PutObjectMeta(newMeta)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "rolled back",
		"bucket": req.Bucket,
		"key":    req.Key,
		"from":   req.VersionID,
		"size":   written,
		"etag":   etag,
	})
}
