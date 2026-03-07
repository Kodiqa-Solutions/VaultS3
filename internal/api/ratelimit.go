package api

import (
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/ratelimit"
)

func (h *APIHandler) SetRateLimiter(rl *ratelimit.Limiter) {
	h.rateLimiter = rl
}

func (h *APIHandler) handleRateLimitStatus(w http.ResponseWriter, r *http.Request) {
	if h.rateLimiter == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	status := h.rateLimiter.Status()
	status["enabled"] = true
	writeJSON(w, http.StatusOK, status)
}
