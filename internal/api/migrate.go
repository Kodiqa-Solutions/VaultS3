package api

import (
	"log/slog"
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/migrate"
)

type migrateRequest struct {
	Endpoint  string   `json:"endpoint"`
	AccessKey string   `json:"accessKey"`
	SecretKey string   `json:"secretKey"`
	Region    string   `json:"region"`
	Buckets   []string `json:"buckets"`
}

func (r migrateRequest) toConfig() migrate.StartConfig {
	return migrate.StartConfig{
		Endpoint:  r.Endpoint,
		AccessKey: r.AccessKey,
		SecretKey: r.SecretKey,
		Region:    r.Region,
		Buckets:   r.Buckets,
	}
}

// handleMigrateTest handles POST /api/v1/migrate/test — validates credentials by
// listing the source buckets.
func (h *APIHandler) handleMigrateTest(w http.ResponseWriter, r *http.Request) {
	if h.migrator == nil {
		writeError(w, http.StatusServiceUnavailable, "migration is not available")
		return
	}
	var req migrateRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	buckets, err := h.migrator.TestConnection(req.toConfig())
	if err != nil {
		// Do not reflect the source's response or the connection error: it used
		// to return up to 4 KiB of whatever an internal service replied with,
		// which turned a connection test into a read primitive (security
		// assessment finding 6). The detail goes to the log instead.
		slog.Warn("migration source test failed", "error", err)
		writeError(w, http.StatusBadGateway, "could not connect to the source with the supplied endpoint and credentials")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"buckets": buckets})
}

// handleMigrateStart handles POST /api/v1/migrate — starts an async migration.
func (h *APIHandler) handleMigrateStart(w http.ResponseWriter, r *http.Request) {
	if h.migrator == nil {
		writeError(w, http.StatusServiceUnavailable, "migration is not available")
		return
	}
	var req migrateRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	id, err := h.migrator.Start(req.toConfig())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not start migration: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"jobId": id})
}

// handleMigrateJobs handles GET /api/v1/migrate/jobs — lists migration jobs.
func (h *APIHandler) handleMigrateJobs(w http.ResponseWriter, r *http.Request) {
	if h.migrator == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	writeJSON(w, http.StatusOK, h.migrator.ListJobs())
}

// handleMigrateCancel handles POST /api/v1/migrate/cancel — cancels a running job.
func (h *APIHandler) handleMigrateCancel(w http.ResponseWriter, r *http.Request) {
	if h.migrator == nil {
		writeError(w, http.StatusServiceUnavailable, "migration is not available")
		return
	}
	var req struct {
		JobID string `json:"jobId"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.JobID == "" {
		writeError(w, http.StatusBadRequest, "jobId is required")
		return
	}
	if !h.migrator.Cancel(req.JobID) {
		writeError(w, http.StatusNotFound, "job not found or not running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelling"})
}
