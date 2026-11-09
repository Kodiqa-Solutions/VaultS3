package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

// Regression (5.0.14 security review): a client that stopped reading held
// its compressor for as long as it kept the connection, so anyone could tie
// up hundreds of megabytes. A compressed write now has a deadline, cleared
// once the response is done so the connection's next request has none.
func TestACompressedWriteHasADeadline(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	duringWrite := 0
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(strings.Repeat("var x = 1;\n", 1000)))
		duringWrite = len(rec.deadlines)
	}))
	req := httptest.NewRequest("GET", "/dashboard/assets/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Skip("not compressed; the test needs a compressed response")
	}
	if duringWrite == 0 {
		t.Fatal("no deadline while the body was written")
	}
	if len(rec.deadlines) < 2 {
		t.Fatalf("deadlines set: %v, want one while writing and a clear after", rec.deadlines)
	}
	if rec.deadlines[0].IsZero() || time.Until(rec.deadlines[0]) <= 0 {
		t.Errorf("first deadline %v, want one in the future", rec.deadlines[0])
	}
	if last := rec.deadlines[len(rec.deadlines)-1]; !last.IsZero() {
		t.Errorf("the deadline was left at %v for the next request", last)
	}
}
