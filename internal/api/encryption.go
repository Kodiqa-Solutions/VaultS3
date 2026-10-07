package api

import (
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
)

// SetKeyManager wires the per-bucket encryption key manager (may be nil).
func (h *APIHandler) SetKeyManager(m *bucketcrypto.Manager) {
	h.keyMgr = m
}

// handleBucketEncryption serves the dashboard's per-bucket encryption controls:
//
//	GET    /api/v1/buckets/{bucket}/encryption        → status
//	POST   /api/v1/buckets/{bucket}/encryption/enable → provision a per-bucket key
//	POST   /api/v1/buckets/{bucket}/encryption/rotate → new key version
//	POST   /api/v1/buckets/{bucket}/encryption/shred  → crypto-shred (irreversible)
func (h *APIHandler) handleBucketEncryption(w http.ResponseWriter, r *http.Request, bucket, action string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	if r.Method == http.MethodGet && action == "" {
		cfg, _ := h.store.GetEncryptionConfig(bucket)
		resp := map[string]any{
			"available": h.keyMgr != nil, // per-bucket encryption configured on the server
			// A bucket whose default encryption was removed keeps its keys for
			// the objects already encrypted, so a key version alone does not
			// mean new objects are encrypted.
			"enabled": cfg != nil && cfg.KeyVersion > 0 && cfg.SSEAlgorithm == "AES256",
		}
		if cfg != nil {
			resp["keyVersion"] = cfg.KeyVersion
			resp["algorithm"] = cfg.SSEAlgorithm
			resp["shredded"] = cfg.Shredded
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.keyMgr == nil {
		writeError(w, http.StatusBadRequest, "per-bucket encryption is not configured on this server (set encryption.per_bucket)")
		return
	}

	var err error
	switch action {
	case "enable":
		err = h.keyMgr.EnableBucket(bucket)
	case "rotate":
		err = h.keyMgr.Rotate(bucket)
	case "shred":
		err = h.keyMgr.ShredBucket(bucket)
	default:
		writeError(w, http.StatusNotFound, "unknown encryption action")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encryption operation failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": action, "bucket": bucket})
}
