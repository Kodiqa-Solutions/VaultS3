package api

import (
	"net/http"
	"runtime"
	"time"
)

// bucketStat returns a bucket's size + object count from the maintained counter,
// backfilling it once (a single filesystem walk) the first time it is missing.
// Keeps the stats/buckets/tco pages O(1) instead of walking every object on every
// request — see issue #16 (1M objects → ~13s page loads).
func (h *APIHandler) bucketStatCounter(bucket string) (size, count int64) {
	if st, ok, _ := h.store.BucketStats(bucket); ok {
		return st.Size, st.Count
	}
	// One-time backfill from the metadata index (a single atomic walk + seed),
	// which is correct for versioned/compressed/encrypted buckets — an engine
	// filesystem walk would count on-disk (compressed/encrypted) bytes and skip
	// versioned data under .vs/.
	st, err := h.store.BackfillBucketStats(bucket)
	if err != nil {
		size, count, _ = h.engine.BucketSize(bucket)
		return size, count
	}
	return st.Size, st.Count
}

type bucketStat struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	ObjectCount  int64  `json:"objectCount"`
	MaxSizeBytes int64  `json:"maxSizeBytes,omitempty"`
	MaxObjects   int64  `json:"maxObjects,omitempty"`
}

type requestMethodStat struct {
	Method string `json:"method"`
	Count  int64  `json:"count"`
}

type statsResponse struct {
	TotalBuckets     int                 `json:"totalBuckets"`
	TotalObjects     int64               `json:"totalObjects"`
	TotalSize        int64               `json:"totalSize"`
	UptimeSeconds    float64             `json:"uptimeSeconds"`
	Goroutines       int                 `json:"goroutines"`
	MemoryMB         float64             `json:"memoryMB"`
	Buckets          []bucketStat        `json:"buckets"`
	RequestsByMethod []requestMethodStat `json:"requestsByMethod"`
	TotalRequests    int64               `json:"totalRequests"`
	TotalErrors      int64               `json:"totalErrors"`
	BytesIn          int64               `json:"bytesIn"`
	BytesOut         int64               `json:"bytesOut"`
}

func (h *APIHandler) handleStats(w http.ResponseWriter, r *http.Request) {
	buckets, err := h.store.ListBuckets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get stats")
		return
	}
	// A non-admin sees the buckets they may list and nothing else. This route
	// used to return every bucket's name, size and quota to any session, which
	// undid the filtering the bucket list applies.
	if visible := h.visibleBucketSet(r, buckets); visible != nil {
		kept := buckets[:0:0]
		for _, b := range buckets {
			if visible[b.Name] {
				kept = append(kept, b)
			}
		}
		buckets = kept
	}

	var totalSize, totalObjects int64
	bucketStats := make([]bucketStat, 0, len(buckets))

	for _, b := range buckets {
		size, count := h.bucketStatCounter(b.Name)
		totalSize += size
		totalObjects += count
		bucketStats = append(bucketStats, bucketStat{
			Name:         b.Name,
			Size:         size,
			ObjectCount:  count,
			MaxSizeBytes: b.MaxSizeBytes,
			MaxObjects:   b.MaxObjects,
		})
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	// Request metrics
	reqByMethod := h.metrics.RequestsByMethod()
	methodStats := make([]requestMethodStat, 0, len(reqByMethod))
	for method, count := range reqByMethod {
		methodStats = append(methodStats, requestMethodStat{Method: method, Count: count})
	}

	writeJSON(w, http.StatusOK, statsResponse{
		TotalBuckets:     len(buckets),
		TotalObjects:     totalObjects,
		TotalSize:        totalSize,
		UptimeSeconds:    time.Since(h.metrics.StartTime()).Seconds(),
		Goroutines:       runtime.NumGoroutine(),
		MemoryMB:         float64(mem.Alloc) / 1024 / 1024,
		Buckets:          bucketStats,
		RequestsByMethod: methodStats,
		TotalRequests:    h.metrics.TotalRequests(),
		TotalErrors:      h.metrics.TotalErrors(),
		BytesIn:          h.metrics.TotalBytesIn(),
		BytesOut:         h.metrics.TotalBytesOut(),
	})
}
