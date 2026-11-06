package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestID_GeneratesNew(t *testing.T) {
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	id := rr.Header().Get("X-Request-Id")
	if id == "" {
		t.Error("expected X-Request-Id header to be set")
	}
	if !strings.Contains(id, "-") {
		t.Errorf("expected ID format timestamp-counter, got %q", id)
	}
}

func TestRequestID_ReusesExisting(t *testing.T) {
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "my-custom-id")
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("X-Request-Id"); got != "my-custom-id" {
		t.Errorf("expected my-custom-id, got %q", got)
	}
}

func TestRequestID_Unique(t *testing.T) {
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		handler.ServeHTTP(rr, req)
		id := rr.Header().Get("X-Request-Id")
		if ids[id] {
			t.Fatalf("duplicate request ID: %s", id)
		}
		ids[id] = true
	}
}

type mockRecorder struct {
	recorded time.Duration
}

func (m *mockRecorder) RecordLatency(d time.Duration) {
	m.recorded = d
}

func TestLatency_Records(t *testing.T) {
	rec := &mockRecorder{}
	handler := Latency(rec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	if rec.recorded < 5*time.Millisecond {
		t.Errorf("expected latency >= 5ms, got %v", rec.recorded)
	}
}

func TestPanicRecovery_NoPanic(t *testing.T) {
	handler := PanicRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestPanicRecovery_CatchesPanic(t *testing.T) {
	handler := PanicRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Internal Server Error") {
		t.Errorf("expected error body, got %q", rr.Body.String())
	}
}

func TestSecurityHeaders_SetsAllHeaders(t *testing.T) {
	handler := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	handler.ServeHTTP(rr, req)

	expected := map[string]string{
		"Content-Security-Policy":   "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'",
		"X-Frame-Options":           "DENY",
		"X-Content-Type-Options":    "nosniff",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
	}

	for header, want := range expected {
		got := rr.Header().Get(header)
		if got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}
}

func TestSecurityHeaders_PassesThrough(t *testing.T) {
	called := false
	handler := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("inner handler was not called")
	}
	if rr.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", rr.Code)
	}
}

func TestMiddlewareChain(t *testing.T) {
	rec := &mockRecorder{}
	handler := PanicRecovery(RequestID(Latency(rec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/bucket", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", rr.Code)
	}
	if rr.Header().Get("X-Request-Id") == "" {
		t.Error("missing X-Request-Id from chain")
	}
	if rec.recorded == 0 {
		t.Error("latency not recorded in chain")
	}
}

// ErrAbortHandler must reach net/http so it cuts the connection. Recovering it
// and writing a 500 after part of the body was sent ended the response
// normally, so a client read a truncated body as complete.
func TestPanicRecoveryLetsAbortCutTheConnection(t *testing.T) {
	srv := httptest.NewServer(PanicRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if body, err := io.ReadAll(resp.Body); err == nil {
		t.Fatalf("aborted response read as complete: %d bytes, ends %q", len(body), body[len(body)-min(len(body), 30):])
	}
}

// Any other panic is still answered with a 500.
func TestPanicRecoveryStillAnswersOtherPanics(t *testing.T) {
	rec := httptest.NewRecorder()
	PanicRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
