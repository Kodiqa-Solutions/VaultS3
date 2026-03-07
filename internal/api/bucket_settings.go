package api

import (
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// --- Versioning ---

func (h *APIHandler) handleGetBucketVersioning(w http.ResponseWriter, _ *http.Request, bucket string) {
	status, err := h.store.GetBucketVersioning(bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"versioning": status})
}

func (h *APIHandler) handlePutBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Versioning string `json:"versioning"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Versioning != "Enabled" && req.Versioning != "Suspended" {
		writeError(w, http.StatusBadRequest, "versioning must be 'Enabled' or 'Suspended'")
		return
	}
	if err := h.store.SetBucketVersioning(bucket, req.Versioning); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Lifecycle ---

func (h *APIHandler) handleGetLifecycleRule(w http.ResponseWriter, _ *http.Request, bucket string) {
	rule, err := h.store.GetLifecycleRule(bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rule == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"rule": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"rule": map[string]interface{}{
			"expirationDays": rule.ExpirationDays,
			"prefix":         rule.Prefix,
			"status":         rule.Status,
		},
	})
}

func (h *APIHandler) handlePutLifecycleRule(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		ExpirationDays int    `json:"expirationDays"`
		Prefix         string `json:"prefix"`
		Status         string `json:"status"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.ExpirationDays < 1 {
		writeError(w, http.StatusBadRequest, "expirationDays must be >= 1")
		return
	}
	if req.Status == "" {
		req.Status = "Enabled"
	}
	rule := metadata.LifecycleRule{
		ExpirationDays: req.ExpirationDays,
		Prefix:         req.Prefix,
		Status:         req.Status,
	}
	if err := h.store.PutLifecycleRule(bucket, rule); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *APIHandler) handleDeleteLifecycleRule(w http.ResponseWriter, _ *http.Request, bucket string) {
	if err := h.store.DeleteLifecycleRule(bucket); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- CORS ---

func (h *APIHandler) handleGetCORSConfig(w http.ResponseWriter, _ *http.Request, bucket string) {
	cfg, err := h.store.GetCORSConfig(bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"rules": []interface{}{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"rules": cfg.Rules})
}

func (h *APIHandler) handlePutCORSConfig(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Rules []metadata.CORSRule `json:"rules"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	cfg := metadata.CORSConfig{Rules: req.Rules}
	if err := h.store.PutCORSConfig(bucket, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *APIHandler) handleDeleteCORSConfig(w http.ResponseWriter, _ *http.Request, bucket string) {
	if err := h.store.DeleteCORSConfig(bucket); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
