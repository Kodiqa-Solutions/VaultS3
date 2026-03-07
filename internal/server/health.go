package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

type healthResponse struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
}

type readyResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func healthHandler(startTime time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(healthResponse{
			Status: "ok",
			Uptime: formatDuration(time.Since(startTime)),
		})
	}
}

func readyHandler(store *metadata.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		_, err := store.ListBuckets()
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(readyResponse{
				Status: "not ready",
				Error:  "metadata store unavailable",
			})
			return
		}

		json.NewEncoder(w).Encode(readyResponse{Status: "ready"})
	}
}

func formatDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd%dh%dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}
