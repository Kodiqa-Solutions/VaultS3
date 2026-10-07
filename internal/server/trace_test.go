package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type recordedTrace struct {
	method, path string
	status       int
}

type fakeTrace struct{ got []recordedTrace }

func (f *fakeTrace) Record(method, path string, status int, _ time.Duration, _ string) {
	f.got = append(f.got, recordedTrace{method, path, status})
}

// The trace view answered 503 forever because nothing recorded into it. Every
// request through the middleware must reach it, with the status the handler set.
func TestTraceMiddlewareRecordsRequests(t *testing.T) {
	tb := &fakeTrace{}
	h := traceRequests(tb, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/b/k", nil))
	if rr.Code != http.StatusTeapot {
		t.Fatalf("the wrapper changed the status: %d", rr.Code)
	}
	if len(tb.got) != 1 || tb.got[0] != (recordedTrace{"GET", "/b/k", http.StatusTeapot}) {
		t.Errorf("trace recorded %+v, want one GET /b/k 418", tb.got)
	}
}
