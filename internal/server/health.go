package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type healthResponse struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
}

type readyResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// storeProbe asks the metadata store a trivial question with a deadline. A node
// whose store is deadlocked used to keep answering /health with 200, because
// the handler never touched the store: 87 goroutines were blocked on the store
// lock while the container stayed "healthy" and nothing restarted it.
//
// One read is in flight at a time. If the store is stuck, every later probe
// waits on that same read instead of starting another goroutine that will
// never return.
type storeProbe struct {
	check   func() error
	timeout time.Duration

	mu       sync.Mutex
	inflight chan error
}

func newStoreProbe(check func() error, timeout time.Duration) *storeProbe {
	return &storeProbe{check: check, timeout: timeout}
}

func (p *storeProbe) healthy() error {
	p.mu.Lock()
	ch := p.inflight
	if ch == nil {
		ch = make(chan error, 1)
		p.inflight = ch
		go func() {
			err := p.check()
			p.mu.Lock()
			p.inflight = nil
			p.mu.Unlock()
			ch <- err
			close(ch)
		}()
	}
	p.mu.Unlock()

	select {
	case err, ok := <-ch:
		if !ok {
			// Another probe already took this result. The read finished, so the
			// store answered.
			return nil
		}
		return err
	case <-time.After(p.timeout):
		return fmt.Errorf("metadata store did not answer within %s", p.timeout)
	}
}

func healthHandler(startTime time.Time, probe *storeProbe) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if probe != nil {
			if err := probe.healthy(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(healthResponse{Status: "unhealthy: " + err.Error(), Uptime: formatDuration(time.Since(startTime))})
				return
			}
		}
		json.NewEncoder(w).Encode(healthResponse{
			Status: "ok",
			Uptime: formatDuration(time.Since(startTime)),
		})
	}
}

func readyHandler(probe *storeProbe) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := probe.healthy(); err != nil {
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
