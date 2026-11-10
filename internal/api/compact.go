package api

import (
	"net/http"
	"strconv"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// handleCompact handles POST /api/v1/compact — reclaims dead space in packed
// volumes (deleted/overwritten objects). Only available when small-file packing
// is enabled. Optional ?min_dead_ratio=0.0..1.0 (default 0.5).
func (h *APIHandler) handleCompact(w http.ResponseWriter, r *http.Request) {
	pe, ok := storage.EngineAs[*storage.PackedEngine](h.engine)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "small-file packing is not enabled")
		return
	}
	ratio := 0.5
	if s := r.URL.Query().Get("min_dead_ratio"); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil && v >= 0 && v <= 1 {
			ratio = v
		}
	}
	reclaimed, err := pe.Compact(ratio)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "compaction failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"reclaimedBytes": reclaimed})
}
