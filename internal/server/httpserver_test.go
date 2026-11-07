package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shrinkTimeouts makes the limits short enough to test, and restores them.
func shrinkTimeouts(t *testing.T, header, idle time.Duration) {
	t.Helper()
	oldH, oldI := readHeaderTimeout, idleTimeout
	readHeaderTimeout, idleTimeout = header, idle
	t.Cleanup(func() { readHeaderTimeout, idleTimeout = oldH, oldI })
}

// serve starts newHTTPServer on a free local port and returns its address.
func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer(ln.Addr().String(), h)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// closedWithin reports whether the server closes conn within d.
func closedWithin(conn net.Conn, d time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, conn)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return false
	}
	return true
}

// A client that sends its headers a byte at a time is cut off once the header
// deadline passes, instead of holding the connection for ever (slowloris).
func TestServerCutsOffSlowHeaders(t *testing.T) {
	shrinkTimeouts(t, 300*time.Millisecond, time.Minute)
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() { // keep trickling a header, never finishing it
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				if _, err := conn.Write([]byte("X")); err != nil {
					return
				}
			}
		}
	}()
	if !closedWithin(conn, 3*time.Second) {
		t.Fatal("connection still open 3s after a 300ms header deadline")
	}
}

// The header deadline must not limit the body. An upload whose body arrives
// over several times the header timeout still completes, so a large upload on
// a slow link is not cut off.
func TestServerAllowsSlowBodyAfterHeaders(t *testing.T) {
	shrinkTimeouts(t, 200*time.Millisecond, time.Minute)
	got := make(chan int, 1)
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got <- int(n)
		w.WriteHeader(http.StatusOK)
	}))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	const chunks = 10
	if _, err := fmt.Fprintf(conn, "PUT /b/k HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", chunks*100); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < chunks; i++ { // 10 x 100ms = 1s, five times the header timeout
		time.Sleep(100 * time.Millisecond)
		if _, err := conn.Write([]byte(strings.Repeat("x", 100))); err != nil {
			t.Fatalf("write %d: %v (server cut the body)", i, err)
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response after a slow body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || <-got != chunks*100 {
		t.Fatalf("status %d, want 200 with the whole body", resp.StatusCode)
	}
}

// The idle timeout applies only between requests. A live response (the
// dashboard's log and event streams) that lasts longer than both limits keeps
// flowing, while an idle keep-alive connection is closed.
func TestServerIdleTimeoutSparesLiveResponses(t *testing.T) {
	shrinkTimeouts(t, 200*time.Millisecond, 300*time.Millisecond)
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stream" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 8; i++ { // 8 x 150ms = 1.2s, four times the idle timeout
			_, _ = fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
		}
	}))

	resp, err := http.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || strings.Count(string(body), "data: ") != 8 {
		t.Fatalf("stream cut short: %d events, err %v", strings.Count(string(body), "data: "), err)
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	r2, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if !closedWithin(conn, 3*time.Second) {
		t.Fatal("idle keep-alive connection still open 3s after a 300ms idle timeout")
	}
}

// Every listener must get the limits. A server built directly with
// &http.Server{...} anywhere in this package would silently have none.
func TestEveryServerUsesNewHTTPServer(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "httpserver.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "http.Server{") {
			t.Errorf("%s builds an http.Server directly, use newHTTPServer so it gets the timeouts", f)
		}
	}
}
