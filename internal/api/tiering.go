package api

import (
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/tiering"
)

func (h *APIHandler) handleTieringStatus(w http.ResponseWriter, r *http.Request) {
	if h.tieringMgr == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, h.tieringMgr.Status())
}

func (h *APIHandler) handleTieringMigrate(w http.ResponseWriter, r *http.Request) {
	if h.tieringMgr == nil {
		writeError(w, http.StatusBadRequest, "tiering not enabled")
		return
	}

	var req struct {
		Bucket    string `json:"bucket"`
		Key       string `json:"key"`
		Direction string `json:"direction"` // "hot" or "cold"
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Bucket == "" || req.Key == "" {
		writeError(w, http.StatusBadRequest, "bucket and key are required")
		return
	}
	if req.Direction == "" {
		req.Direction = "cold"
	}

	if err := h.tieringMgr.ManualMigrate(req.Bucket, req.Key, req.Direction); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":    "migrated",
		"bucket":    req.Bucket,
		"key":       req.Key,
		"direction": req.Direction,
	})
}

// SetTieringManager sets the tiering manager reference.
func (h *APIHandler) SetTieringManager(mgr *tiering.Manager) {
	h.tieringMgr = mgr
}
