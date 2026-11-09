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
		// Shredding destroys every object, so object lock forbids it while
		// any object or version is held, the admin included, as it forbids
		// deleting them one by one.
		if reason, lerr := h.bucketLockReason(bucket); lerr != nil {
			writeError(w, http.StatusInternalServerError, "could not check the bucket for locked objects")
			return
		} else if reason != "" {
			writeError(w, http.StatusConflict, "cannot shred: "+reason)
			return
		}
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

// bucketLockReason says why object lock forbids destroying the bucket's data
// (an object or version under legal hold or unexpired retention), or "".
func (h *APIHandler) bucketLockReason(bucket string) (string, error) {
	after := ""
	for {
		objs, more, err := h.store.ListLatestObjects(bucket, "", after, 1000)
		if err != nil {
			return "", err
		}
		for i := range objs {
			if r := objectLockReason(&objs[i]); r != "" {
				return objs[i].Key + ": " + r, nil
			}
		}
		if !more || len(objs) == 0 {
			break
		}
		after = objs[len(objs)-1].Key
	}
	keyMarker, versionMarker := "", ""
	for {
		vers, more, err := h.store.ListObjectVersions(bucket, "", keyMarker, versionMarker, 1000)
		if err != nil {
			return "", err
		}
		for i := range vers {
			if r := objectLockReason(&vers[i]); r != "" {
				return vers[i].Key + " (version " + vers[i].VersionID + "): " + r, nil
			}
		}
		if !more || len(vers) == 0 {
			return "", nil
		}
		keyMarker, versionMarker = vers[len(vers)-1].Key, vers[len(vers)-1].VersionID
	}
}
