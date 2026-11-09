package s3

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// One CSV row of commas: 64 MiB input (about 64 KB gzipped) becomes one
// []string of 64M empty fields before budget.record sees it.
func TestSelectOneWideCSVRowIsBounded(t *testing.T) {
	in := bytes.Repeat([]byte{','}, int(maxSelectInput))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := parseCSVInput(&selectLimit{r: bytes.NewReader(in), n: maxSelectInput}, &csvInput{})
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("err=%v, allocated %d MiB for %d MiB input", err, alloc>>20, len(in)>>20)
	if alloc > 4*uint64(maxSelectInput) {
		t.Errorf("allocated %d MiB (> 4x the 64 MiB input)", alloc>>20)
	}
}

// The select handler sets its write deadline through http.ResponseController,
// but ServeHTTP wraps w in statusWriter, which has no Unwrap: the deadline is
// never set and a slow reader keeps one of the two Select slots.
func TestSelectWriteDeadlineReachesTheConnection(t *testing.T) {
	var got error
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = &sandboxWriter{ResponseWriter: w}
		w = &statusWriter{ResponseWriter: w, status: 200}
		got = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
	}))
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != nil {
		t.Errorf("SetWriteDeadline through ServeHTTP's writers: %v", got)
	}
}
