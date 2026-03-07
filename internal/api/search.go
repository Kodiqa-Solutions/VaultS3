package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Kodiqa-Solutions/VaultS3/internal/search"
)

func (h *APIHandler) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "query parameter 'q' is required")
		return
	}

	bucket := r.URL.Query().Get("bucket")
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}

	if h.searchIndex == nil {
		writeError(w, http.StatusServiceUnavailable, "search index not available")
		return
	}

	results := h.searchIndex.Search(q, bucket, limit)
	if results == nil {
		results = []search.Result{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}
