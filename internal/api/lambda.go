package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/lambda"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// SetLambdaManager sets the lambda trigger manager.
func (h *APIHandler) SetLambdaManager(mgr *lambda.TriggerManager) {
	h.lambdaMgr = mgr
}

type lambdaTriggerResponse struct {
	ID          string   `json:"id"`
	FunctionURL string   `json:"functionURL"`
	Events      []string `json:"events"`
	KeyFilter   string   `json:"keyFilter"`
}

type bucketTriggersResponse struct {
	Bucket   string                  `json:"bucket"`
	Triggers []lambdaTriggerResponse `json:"triggers"`
}

func convertTriggers(triggers []metadata.LambdaTrigger) []lambdaTriggerResponse {
	result := make([]lambdaTriggerResponse, 0, len(triggers))
	for _, t := range triggers {
		keyFilter := ""
		if t.Filters.Prefix != "" {
			keyFilter = t.Filters.Prefix + "*"
		}
		if t.Filters.Suffix != "" {
			if keyFilter != "" {
				keyFilter += ", "
			}
			keyFilter += "*" + t.Filters.Suffix
		}
		result = append(result, lambdaTriggerResponse{
			ID:          t.ID,
			FunctionURL: t.FunctionURL,
			Events:      t.Events,
			KeyFilter:   keyFilter,
		})
	}
	return result
}

// handleListLambdaTriggers returns all lambda trigger configurations across buckets.
func (h *APIHandler) handleListLambdaTriggers(w http.ResponseWriter, r *http.Request) {
	configs, err := h.store.ListLambdaConfigs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result := make([]bucketTriggersResponse, 0, len(configs))
	for bucket, cfg := range configs {
		result = append(result, bucketTriggersResponse{
			Bucket:   bucket,
			Triggers: convertTriggers(cfg.Triggers),
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// handleGetLambdaTriggers returns lambda triggers for a specific bucket.
func (h *APIHandler) handleGetLambdaTriggers(w http.ResponseWriter, r *http.Request, bucket string) {
	cfg, err := h.store.GetLambdaConfig(bucket)
	if err != nil {
		writeJSON(w, http.StatusOK, bucketTriggersResponse{
			Bucket:   bucket,
			Triggers: []lambdaTriggerResponse{},
		})
		return
	}
	writeJSON(w, http.StatusOK, bucketTriggersResponse{
		Bucket:   bucket,
		Triggers: convertTriggers(cfg.Triggers),
	})
}

// handlePutLambdaTriggers sets lambda triggers for a specific bucket.
func (h *APIHandler) handlePutLambdaTriggers(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeError(w, http.StatusNotFound, "bucket not found")
		return
	}

	var cfg metadata.BucketLambdaConfig
	if err := readJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate function URLs to prevent SSRF
	for _, t := range cfg.Triggers {
		if err := ValidateWebhookURL(t.FunctionURL); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid function URL: %v", err))
			return
		}
	}

	if err := h.store.PutLambdaConfig(bucket, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleDeleteLambdaTriggers removes lambda triggers for a specific bucket.
func (h *APIHandler) handleDeleteLambdaTriggers(w http.ResponseWriter, r *http.Request, bucket string) {
	h.store.DeleteLambdaConfig(bucket)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLambdaStatus returns status of the lambda trigger manager.
func (h *APIHandler) handleLambdaStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"enabled":       h.lambdaMgr != nil,
		"totalTriggers": 0,
		"buckets":       0,
		"queueDepth":    0,
	}
	if h.lambdaMgr != nil {
		status["queueDepth"] = h.lambdaMgr.QueueDepth()
		configs, _ := h.store.ListLambdaConfigs()
		totalTriggers := 0
		for _, c := range configs {
			totalTriggers += len(c.Triggers)
		}
		status["totalTriggers"] = totalTriggers
		status["buckets"] = len(configs)
	}
	writeJSON(w, http.StatusOK, status)
}

// routeLambda handles /lambda/* API routes.
func (h *APIHandler) routeLambda(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case path == "triggers" && r.Method == http.MethodGet:
		h.handleListLambdaTriggers(w, r)
	case path == "status" && r.Method == http.MethodGet:
		h.handleLambdaStatus(w, r)
	case strings.HasPrefix(path, "triggers/"):
		bucket := strings.TrimPrefix(path, "triggers/")
		switch r.Method {
		case http.MethodGet:
			h.handleGetLambdaTriggers(w, r, bucket)
		case http.MethodPut:
			h.handlePutLambdaTriggers(w, r, bucket)
		case http.MethodDelete:
			h.handleDeleteLambdaTriggers(w, r, bucket)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}
