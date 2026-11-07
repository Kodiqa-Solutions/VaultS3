package api

import (
	"net/http"
	"sync"
)

// LogBroadcaster distributes log lines to SSE subscribers.
type LogBroadcaster struct {
	mu          sync.RWMutex
	subscribers map[chan string]struct{}
}

// NewLogBroadcaster creates a new log broadcaster.
func NewLogBroadcaster() *LogBroadcaster {
	return &LogBroadcaster{
		subscribers: make(map[chan string]struct{}),
	}
}

// Write implements io.Writer for slog integration.
func (lb *LogBroadcaster) Write(p []byte) (n int, err error) {
	line := string(p)
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	for ch := range lb.subscribers {
		select {
		case ch <- line:
		default:
		}
	}
	return len(p), nil
}

func (lb *LogBroadcaster) subscribe() chan string {
	ch := make(chan string, 128)
	lb.mu.Lock()
	lb.subscribers[ch] = struct{}{}
	lb.mu.Unlock()
	return ch
}

func (lb *LogBroadcaster) unsubscribe(ch chan string) {
	lb.mu.Lock()
	delete(lb.subscribers, ch)
	lb.mu.Unlock()
	close(ch)
}

// handleLogStream handles GET /api/v1/logs (SSE endpoint).
func (h *APIHandler) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if h.logBroadcaster == nil {
		writeError(w, http.StatusServiceUnavailable, "log streaming not enabled")
		return
	}

	ch := h.logBroadcaster.subscribe()
	defer h.logBroadcaster.unsubscribe(ch)
	serveSSE(h, w, r, ch, "data: %s\n")
}
