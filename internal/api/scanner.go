package api

import (
	"encoding/json"
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/scanner"
)

func (h *APIHandler) handleScannerStatus(w http.ResponseWriter, r *http.Request) {
	if h.scanner == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": false,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled":     true,
		"queue_depth": h.scanner.QueueDepth(),
		"recent":      h.scanner.RecentResults(20),
	})
}

func (h *APIHandler) handleQuarantineList(w http.ResponseWriter, r *http.Request) {
	if h.scanner == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}

	objects := h.scanner.QuarantineList(h.store, h.engine)
	if objects == nil {
		objects = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(objects)
}

// SetScanner sets the scanner instance for API endpoints.
func (h *APIHandler) SetScanner(s *scanner.Scanner) {
	h.scanner = s
}
