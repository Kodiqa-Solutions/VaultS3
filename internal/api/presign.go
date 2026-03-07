package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	s3handler "github.com/Kodiqa-Solutions/VaultS3/internal/s3"
)

type presignRequest struct {
	Bucket        string `json:"bucket"`
	Key           string `json:"key"`
	Method        string `json:"method"` // GET or PUT
	ExpiresSecs   int    `json:"expires"`
	MaxSize       int64  `json:"maxSize"`
	AllowTypes    string `json:"allowTypes"`
	RequirePrefix string `json:"requirePrefix"`
}

func (h *APIHandler) handleGeneratePresign(w http.ResponseWriter, r *http.Request) {
	var req presignRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Bucket == "" || req.Key == "" {
		writeError(w, http.StatusBadRequest, "bucket and key are required")
		return
	}

	// Validate bucket exists
	if !h.store.BucketExists(req.Bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	if req.ExpiresSecs <= 0 {
		req.ExpiresSecs = 3600
	}
	if req.ExpiresSecs > 604800 {
		req.ExpiresSecs = 604800 // max 7 days
	}

	expires := time.Duration(req.ExpiresSecs) * time.Second
	host := fmt.Sprintf("%s:%d", h.cfg.Server.Address, h.cfg.Server.Port)
	if h.cfg.Server.Address == "0.0.0.0" {
		host = fmt.Sprintf("localhost:%d", h.cfg.Server.Port)
	}

	// Use admin credentials for presigned URLs. The presigned URL is already
	// scoped to a specific bucket/key and time-limited by the signature.
	presignAccessKey := h.cfg.Auth.AdminAccessKey
	presignSecretKey := h.cfg.Auth.AdminSecretKey

	var presignedURL string
	method := req.Method
	if method == "" {
		method = "GET"
	}

	switch method {
	case "GET":
		presignedURL = s3handler.GeneratePresignedURL(
			host, req.Bucket, req.Key,
			presignAccessKey, presignSecretKey,
			"us-east-1", expires,
		)
	case "PUT":
		var restrictions *s3handler.PresignedUploadRestrictions
		if req.MaxSize > 0 || req.AllowTypes != "" || req.RequirePrefix != "" {
			restrictions = &s3handler.PresignedUploadRestrictions{
				MaxSize:       req.MaxSize,
				AllowTypes:    req.AllowTypes,
				RequirePrefix: req.RequirePrefix,
			}
		}
		presignedURL = s3handler.GeneratePresignedPutURL(
			host, req.Bucket, req.Key,
			presignAccessKey, presignSecretKey,
			"us-east-1", expires, restrictions,
		)
	default:
		writeError(w, http.StatusBadRequest, "method must be GET or PUT")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"url":     presignedURL,
		"method":  method,
		"expires": req.ExpiresSecs,
	})
}
