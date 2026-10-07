package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthHandler(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	handler := healthHandler(start, newStoreProbe(func() error { return nil }, time.Second))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}

	var resp healthResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status: got %q, want ok", resp.Status)
	}
	if resp.Uptime == "" {
		t.Error("expected non-empty uptime")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0m"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h30m"},
		{2*time.Hour + 15*time.Minute, "2h15m"},
		{25 * time.Hour, "1d1h0m"},
		{48*time.Hour + 30*time.Minute, "2d0h30m"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v): got %q, want %q", tt.d, got, tt.want)
		}
	}
}

// A deadlocked metadata store left /health answering 200, so nothing restarted
// a node that could serve no request. It must answer 503 once the store does
// not respond in time, and a stuck store must not grow a goroutine per probe.
func TestHealthFailsWhenTheStoreDoesNotAnswer(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	var calls int32
	probe := newStoreProbe(func() error { atomic.AddInt32(&calls, 1); <-block; return nil }, 50*time.Millisecond)
	h := healthHandler(time.Now(), probe)
	r := readyHandler(probe)
	for i := 0; i < 5; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("/health with a stuck store: %d, want 503", rr.Code)
		}
		rr = httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ready", nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("/ready with a stuck store: %d, want 503", rr.Code)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("a stuck store was asked %d times, want one read in flight", n)
	}
}
