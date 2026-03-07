package api

import (
	"net/http"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/backup"
)

type backupRecordResponse struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Target    string `json:"target"`
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
	Objects   int64  `json:"objects"`
	Size      int64  `json:"size"`
	Status    string `json:"status"`
}

func (h *APIHandler) handleBackupList(w http.ResponseWriter, r *http.Request) {
	if h.backupSched == nil {
		writeJSON(w, http.StatusOK, []backupRecordResponse{})
		return
	}

	records, err := h.backupSched.ListRecords(50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := make([]backupRecordResponse, 0, len(records))
	for _, r := range records {
		startTime := ""
		if r.StartTime > 0 {
			startTime = time.Unix(r.StartTime, 0).UTC().Format(time.RFC3339)
		}
		endTime := ""
		if r.EndTime > 0 {
			endTime = time.Unix(r.EndTime, 0).UTC().Format(time.RFC3339)
		}
		items = append(items, backupRecordResponse{
			ID:        r.ID,
			Type:      r.Type,
			Target:    r.Target,
			StartTime: startTime,
			EndTime:   endTime,
			Objects:   r.ObjectCount,
			Size:      r.TotalSize,
			Status:    r.Status,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *APIHandler) handleBackupTrigger(w http.ResponseWriter, r *http.Request) {
	if h.backupSched == nil {
		writeError(w, http.StatusBadRequest, "backup not enabled")
		return
	}

	msg := h.backupSched.TriggerBackup()
	writeJSON(w, http.StatusOK, map[string]string{"status": msg})
}

func (h *APIHandler) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	if h.backupSched == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": false,
			"running": false,
			"targets": 0,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": true,
		"running": h.backupSched.IsRunning(),
		"targets": len(h.cfg.Backup.Targets),
	})
}

// SetBackupScheduler sets the backup scheduler reference.
func (h *APIHandler) SetBackupScheduler(sched *backup.Scheduler) {
	h.backupSched = sched
}
