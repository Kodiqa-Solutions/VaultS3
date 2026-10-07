package api

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

const maxActivityEntries = 100

// ActivityEntry represents a single S3 operation.
type ActivityEntry struct {
	Time     time.Time `json:"time"`
	Method   string    `json:"method"`
	Bucket   string    `json:"bucket"`
	Key      string    `json:"key"`
	Status   int       `json:"status"`
	Size     int64     `json:"size"`
	ClientIP string    `json:"clientIP"`
}

// ActivityLog is a thread-safe ring buffer of recent S3 operations.
type ActivityLog struct {
	mu      sync.Mutex
	entries []ActivityEntry
	pos     int
	full    bool
}

// NewActivityLog creates an empty activity log.
func NewActivityLog() *ActivityLog {
	return &ActivityLog{
		entries: make([]ActivityEntry, maxActivityEntries),
	}
}

// Record adds an entry to the ring buffer.
func (l *ActivityLog) Record(e ActivityEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries[l.pos] = e
	l.pos++
	if l.pos >= maxActivityEntries {
		l.pos = 0
		l.full = true
	}
}

// Recent returns the last n entries, newest first.
func (l *ActivityLog) Recent(n int) []ActivityEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	count := l.pos
	if l.full {
		count = maxActivityEntries
	}
	if n > count {
		n = count
	}
	if n == 0 {
		return nil
	}

	result := make([]ActivityEntry, n)
	for i := 0; i < n; i++ {
		idx := l.pos - 1 - i
		if idx < 0 {
			idx += maxActivityEntries
		}
		result[i] = l.entries[idx]
	}
	return result
}

func (h *APIHandler) handleActivity(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if lim := r.URL.Query().Get("limit"); lim != "" {
		if v, err := strconv.Atoi(lim); err == nil && v > 0 && v <= maxActivityEntries {
			limit = v
		}
	}

	entries := h.activity.Recent(limit)
	if entries == nil {
		entries = []ActivityEntry{}
	}
	// A non-admin sees calls on the buckets they may list, without the client
	// address. Every recent S3 call, with its bucket, key and caller IP, used to
	// go to any session.
	if !h.isAdminUser(r) {
		buckets, err := h.store.ListBuckets()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list buckets")
			return
		}
		visible := h.visibleBucketSet(r, buckets)
		kept := make([]ActivityEntry, 0, len(entries))
		for _, e := range entries {
			if !visible[e.Bucket] {
				continue
			}
			e.ClientIP = ""
			kept = append(kept, e)
		}
		entries = kept
	}
	writeJSON(w, http.StatusOK, entries)
}
