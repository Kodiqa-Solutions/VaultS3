package cluster

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Regression (5.0.11 round 4): the holder answers 100 Continue, reads part of
// the upload, then dies. The entry counted the 1xx as an answer, so it wrote
// nothing more and net/http sent 200 for an upload that was never stored.
func TestHopFailureAfter100ContinueIsNotAnsweredOK(t *testing.T) {
	stored := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		_, _ = io.ReadFull(r.Body, buf) // sends 100 Continue
		hj, _ := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer peer.Close()
	p := newTestProxy("self", map[string]string{"peer": hostOf(t, peer)}, nil, 1)
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.ForwardWithRetry(w, r, "b", "k") {
			stored = true
			w.WriteHeader(200)
		}
	}))
	defer entry.Close()
	req, _ := http.NewRequest(http.MethodPut, entry.URL+"/b/k", bytes.NewReader(bytes.Repeat([]byte("x"), 4<<20)))
	req.Header.Set("Expect", "100-continue")
	c := &http.Client{Transport: &http.Transport{ExpectContinueTimeout: 2 * time.Second}}
	resp, err := c.Do(req)
	if err != nil {
		return // the connection failed: the client knows the upload did not happen
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 && !stored {
		t.Errorf("the upload was lost on the holder but the client was told 200 OK (%q)", body)
	}
}
