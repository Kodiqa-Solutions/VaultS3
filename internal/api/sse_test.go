package api

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamingAPI returns a handler with all three live streams enabled, a token,
// and a function that publishes one item on the named stream.
func streamingAPI(t *testing.T) (*APIHandler, string, map[string]func()) {
	t.Helper()
	h, _ := newTestAPI(t)
	token := getToken(t, h)
	lb, eb, tb := NewLogBroadcaster(), NewEventBus(), NewTraceBroadcaster()
	h.SetLogBroadcaster(lb)
	h.SetEventBus(eb)
	h.SetTraceBroadcaster(tb)
	publish := map[string]func(){
		"/logs":   func() { _, _ = lb.Write([]byte("hello log\n")) },
		"/events": func() { eb.Publish("s3:ObjectCreated:Put", "b", "k", 1, "e") },
		"/trace":  func() { tb.Record("GET", "/b/k", 200, time.Millisecond, "10.0.0.1") },
	}
	return h, token, publish
}

func openStream(t *testing.T, base, path, token string) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		done <- result{resp, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("%s: %v", path, res.err)
		}
		return res.resp, bufio.NewReader(res.resp.Body)
	case <-ctx.Done():
		t.Fatalf("%s: no response headers within 2s while no event was published", path)
		return nil, nil
	}
}

// endsWithin reports whether the stream body ends (EOF) within d.
func endsWithin(br *bufio.Reader, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, br)
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// Each stream answers at once (headers before any event), delivers what is
// published, and ends when CloseStreams is called. A stream opened after
// CloseStreams ends at once.
func TestLiveStreamsOpenAtOnceAndEndOnCloseStreams(t *testing.T) {
	for _, path := range []string{"/logs", "/events", "/trace"} {
		t.Run(path, func(t *testing.T) {
			h, token, publish := streamingAPI(t)
			srv := httptest.NewServer(h)
			defer srv.Close()
			// Runs before srv.Close, which waits for every open request: if a
			// guard is broken the test must fail, not hang on its own cleanup.
			defer h.CloseStreams()

			resp, br := openStream(t, srv.URL, path, token)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
			}

			got := make(chan string, 1)
			go func() {
				line, _ := br.ReadString('\n')
				got <- line
			}()
			// The subscription is taken before the headers are sent, so a
			// publish now is delivered.
			publish[path]()
			select {
			case line := <-got:
				if !strings.HasPrefix(line, "data: ") {
					t.Fatalf("first line %q, want an SSE data line", line)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("published item not delivered")
			}

			h.CloseStreams()
			if !endsWithin(br, 2*time.Second) {
				t.Fatal("stream still open 2s after CloseStreams")
			}

			resp2, br2 := openStream(t, srv.URL, path, token)
			defer func() { _ = resp2.Body.Close() }()
			if !endsWithin(br2, 2*time.Second) {
				t.Fatal("a stream opened after CloseStreams did not end at once")
			}
		})
	}
}

// The server registers CloseStreams with RegisterOnShutdown. With that, a
// graceful shutdown finishes at once although a stream is open. Without it
// (the control) the same shutdown waits for the stream until its deadline,
// which is what held a production restart for 30 seconds.
func TestShutdownIsNotHeldByAnOpenStream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register bool
	}{
		{"CloseStreams registered", true},
		{"control without it", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, token, _ := streamingAPI(t)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: h}
			if tc.register {
				srv.RegisterOnShutdown(h.CloseStreams)
			}
			go func() { _ = srv.Serve(ln) }()
			defer func() { _ = srv.Close() }()
			defer h.CloseStreams()

			resp, _ := openStream(t, "http://"+ln.Addr().String(), "/logs", token)
			defer func() { _ = resp.Body.Close() }()

			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			start := time.Now()
			err = srv.Shutdown(ctx)
			took := time.Since(start)
			if tc.register && (err != nil || took > 500*time.Millisecond) {
				t.Fatalf("shutdown took %v (err %v) with a stream open, want under 500ms", took, err)
			}
			if !tc.register && err == nil {
				t.Fatalf("control: shutdown finished in %v, so this test cannot see the bug", took)
			}
		})
	}
}

// A handler built without NewAPIHandler has no stream channel. CloseStreams
// must not panic on it, and calling it twice is fine.
func TestCloseStreamsIsSafeToCallOnAnyHandler(t *testing.T) {
	(&APIHandler{}).CloseStreams()
	h, _ := newTestAPI(t)
	h.CloseStreams()
	h.CloseStreams()
}
