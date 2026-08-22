package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

type bucketListItem struct {
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"createdAt"`
	Size         int64     `json:"size"`
	ObjectCount  int64     `json:"objectCount"`
	MaxSizeBytes int64     `json:"maxSizeBytes,omitempty"`
	MaxObjects   int64     `json:"maxObjects,omitempty"`
}

type bucketDetail struct {
	Name         string          `json:"name"`
	CreatedAt    time.Time       `json:"createdAt"`
	Size         int64           `json:"size"`
	ObjectCount  int64           `json:"objectCount"`
	MaxSizeBytes int64           `json:"maxSizeBytes,omitempty"`
	MaxObjects   int64           `json:"maxObjects,omitempty"`
	Policy       json.RawMessage `json:"policy,omitempty"`
	// How much protection this bucket's data gets, after its own overrides are
	// applied to the server defaults (issue #39).
	Durability metadata.Durability `json:"durability"`
}

type createBucketRequest struct {
	Name string `json:"name"`
}

type quotaRequest struct {
	MaxSizeBytes int64 `json:"maxSizeBytes"`
	MaxObjects   int64 `json:"maxObjects"`
}

func (h *APIHandler) handleListBuckets(w http.ResponseWriter, _ *http.Request) {
	buckets, err := h.store.ListBuckets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list buckets")
		return
	}

	items := make([]bucketListItem, 0, len(buckets))
	for _, b := range buckets {
		size, count := h.bucketStatCounter(b.Name)
		items = append(items, bucketListItem{
			Name:         b.Name,
			CreatedAt:    b.CreatedAt,
			Size:         size,
			ObjectCount:  count,
			MaxSizeBytes: b.MaxSizeBytes,
			MaxObjects:   b.MaxObjects,
		})
	}

	writeJSON(w, http.StatusOK, items)
}

func (h *APIHandler) handleCreateBucket(w http.ResponseWriter, r *http.Request) {
	var req createBucketRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "bucket name is required")
		return
	}
	if err := validateBucketName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if h.store.BucketExists(req.Name) {
		writeError(w, http.StatusConflict, "bucket already exists")
		return
	}

	if err := h.store.CreateBucket(req.Name); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create bucket")
		return
	}
	if err := h.engine.CreateBucketDir(req.Name); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create bucket storage")
		return
	}

	b, _ := h.store.GetBucket(req.Name)
	if b == nil {
		writeJSON(w, http.StatusCreated, bucketListItem{
			Name: req.Name,
		})
		return
	}
	writeJSON(w, http.StatusCreated, bucketListItem{
		Name:      b.Name,
		CreatedAt: b.CreatedAt,
	})
}

func (h *APIHandler) handleGetBucket(w http.ResponseWriter, _ *http.Request, name string) {
	b, err := h.store.GetBucket(name)
	if err != nil || b == nil {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	size, count := h.bucketStatCounter(name)
	policyBytes, _ := h.store.GetBucketPolicy(name)

	detail := bucketDetail{
		Name:         b.Name,
		CreatedAt:    b.CreatedAt,
		Size:         size,
		ObjectCount:  count,
		MaxSizeBytes: b.MaxSizeBytes,
		MaxObjects:   b.MaxObjects,
		Durability:   h.store.BucketDurability(name, h.cfg.Erasure.Enabled, h.cfg.Cluster.Placement.ReplicaCount),
	}
	if len(policyBytes) > 0 {
		detail.Policy = json.RawMessage(policyBytes)
	}

	writeJSON(w, http.StatusOK, detail)
}

func (h *APIHandler) handleDeleteBucket(w http.ResponseWriter, _ *http.Request, name string) {
	if !h.store.BucketExists(name) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	_, count, _ := h.engine.BucketSize(name)
	if count > 0 {
		writeError(w, http.StatusConflict, "bucket is not empty")
		return
	}

	h.store.DeleteBucketPolicy(name)
	// Two phases, object metadata first. When metadata is sharded the records
	// live in the shard that owns the bucket, not with the bucket record, so
	// dropping the bucket while that shard is unreachable would strand them:
	// nothing would own them and a bucket recreated with the same name would
	// inherit them (issue #50).
	if err := h.store.DeleteBucketObjectMeta(name); err != nil {
		writeError(w, http.StatusServiceUnavailable, "could not clear the bucket's object metadata, retry")
		return
	}
	if err := h.store.DeleteBucket(name); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete bucket")
		return
	}
	h.engine.DeleteBucketDir(name)

	w.WriteHeader(http.StatusNoContent)
}

func (h *APIHandler) handlePutBucketPolicy(w http.ResponseWriter, r *http.Request, name string) {
	if !h.store.BucketExists(name) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	if !json.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid JSON policy")
		return
	}

	if err := h.store.PutBucketPolicy(name, body); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set policy")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *APIHandler) handlePutBucketQuota(w http.ResponseWriter, r *http.Request, name string) {
	if !h.store.BucketExists(name) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	var req quotaRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.store.UpdateBucketQuota(name, req.MaxSizeBytes, req.MaxObjects); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update quota")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
