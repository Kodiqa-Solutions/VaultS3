package api

import (
	"net/http"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// routeSnapshots dispatches /api/v1/buckets/{bucket}/snapshots[/...]:
//
//	GET    /snapshots               — list
//	POST   /snapshots               — create   {message}
//	GET    /snapshots/{id}/diff     — diff vs live bucket
//	POST   /snapshots/{id}/restore  — roll the bucket back to the snapshot
//	DELETE /snapshots/{id}          — delete the snapshot (object data untouched)
func (h *APIHandler) routeSnapshots(w http.ResponseWriter, r *http.Request, bucket, rest string) {
	if h.snapshots == nil {
		writeError(w, http.StatusServiceUnavailable, "snapshots are not available")
		return
	}

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			h.handleListSnapshots(w, r, bucket)
		case http.MethodPost:
			h.handleCreateSnapshot(w, r, bucket)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodDelete:
		if err := h.snapshots.Delete(bucket, id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	case action == "diff" && r.Method == http.MethodGet:
		diff, err := h.snapshots.Diff(bucket, id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, diff)
	case action == "restore" && r.Method == http.MethodPost:
		res, err := h.snapshots.Restore(bucket, id)
		if err != nil {
			if res == nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			// Part of the bucket was restored. Say how much, with the reasons,
			// rather than a bare error that hides what did change.
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":    err.Error(),
				"reverted": res.Reverted,
				"removed":  res.Removed,
				"skipped":  res.Skipped,
				"failed":   res.Failed,
				"errors":   res.Errors,
			})
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *APIHandler) handleListSnapshots(w http.ResponseWriter, _ *http.Request, bucket string) {
	snaps, err := h.snapshots.List(bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if snaps == nil {
		snaps = []metadata.BucketSnapshot{}
	}
	writeJSON(w, http.StatusOK, snaps)
}

func (h *APIHandler) handleCreateSnapshot(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Message string `json:"message"`
	}
	_ = readJSON(r, &req)
	snap, err := h.snapshots.Create(bucket, req.Message)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, snap)
}
