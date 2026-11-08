package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/api"
	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metrics"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// shrinkTimeouts makes the limits short enough to test, and restores them.
func shrinkTimeouts(t *testing.T, header, idle time.Duration) {
	t.Helper()
	oldH, oldI, oldD := readHeaderTimeout, idleTimeout, bodyDrainTimeout
	readHeaderTimeout, idleTimeout, bodyDrainTimeout = header, idle, header
	t.Cleanup(func() { readHeaderTimeout, idleTimeout, bodyDrainTimeout = oldH, oldI, oldD })
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

// Live streams end only when told to. Both listeners that serve the console
// API must end them as shutdown starts (see api.CloseStreams), or one open
// stream holds a restart for the whole shutdown timeout. The behaviour itself
// is tested in internal/api; this pins the wiring in Run.
func TestBothAPIListenersEndStreamsOnShutdown(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"httpServer.RegisterOnShutdown(apiHandler.CloseStreams)",
		"consoleServer.RegisterOnShutdown(apiHandler.CloseStreams)",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("server.go no longer calls %s", want)
		}
	}
}

// The idle timeout comes from server.idle_timeout_secs, 650s by default, which
// is above the idle timeout of common load balancers (Google Cloud keeps
// backend connections for 600s and needs the backend to keep them longer).
func TestIdleTimeoutComesFromConfig(t *testing.T) {
	old := idleTimeout
	t.Cleanup(func() { idleTimeout = old })
	if got := config.Defaults().Server.IdleTimeoutSecs; got != 650 {
		t.Fatalf("default idle_timeout_secs = %d, want 650", got)
	}
	// Applied when the server is built, so every listener gets it.
	for _, c := range []struct{ secs, want int }{{900, 900}, {0, 650}, {-5, 650}} {
		cfg := shardTestConfig(t, 1)
		cfg.Server.IdleTimeoutSecs = c.secs
		idleTimeout = time.Second
		srv, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if hs := newHTTPServer(":0", http.NotFoundHandler()); hs.IdleTimeout != time.Duration(c.want)*time.Second {
			t.Errorf("idle_timeout_secs %d: server IdleTimeout = %v, want %ds", c.secs, hs.IdleTimeout, c.want)
		}
	}
}

// The shutdown drain runs the listeners side by side and, at the deadline,
// closes the connections still open, so a stuck request cannot keep the
// process alive. The first listener's error is the one returned.
func TestShutdownDrainClosesStuckConnectionsInParallel(t *testing.T) {
	release := make(chan struct{})
	var handlers sync.WaitGroup
	// The handlers outlive their closed connections: let them return before
	// the next test changes the shared limits they read.
	t.Cleanup(func() { close(release); handlers.Wait() })
	var servers []namedServer
	var results []chan error
	var addrs []string
	for _, name := range []string{"s3", "console"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		srv := newHTTPServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release // a request that never finishes on its own
		}))
		inner := srv.Handler // includes the drain bound, which reads the limits too
		srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlers.Add(1)
			defer handlers.Done()
			inner.ServeHTTP(w, r)
		})
		go func() { _ = srv.Serve(ln) }()
		res := make(chan error, 1)
		go func() {
			resp, err := http.Get("http://" + ln.Addr().String() + "/")
			if err == nil {
				resp.Body.Close()
			}
			res <- err
		}()
		<-started
		servers = append(servers, namedServer{name, srv})
		results = append(results, res)
		addrs = append(addrs, ln.Addr().String())
	}
	servers = append(servers, namedServer{"inter-node", nil}) // not configured

	const deadline = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	t0 := time.Now()
	done := make(chan error, 1)
	go func() { done <- drainServers(ctx, deadline, servers...) }()
	// Side by side, every listener stops accepting at once. One after the
	// other, the console would take new connections until the stuck s3
	// listener gave up.
	time.Sleep(100 * time.Millisecond)
	for i, addr := range addrs {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			t.Errorf("%s still accepts connections while shutting down", servers[i].name)
		}
	}
	err := <-done
	took := time.Since(t0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain returned %v, want the s3 listener's deadline error", err)
	}
	// One after the other would take two deadlines.
	if took > deadline+400*time.Millisecond {
		t.Fatalf("drain took %v for two stuck listeners, want about %v (side by side)", took, deadline)
	}
	for i, res := range results {
		select {
		case err := <-res:
			if err == nil {
				t.Fatalf("%s: the stuck request got an answer, want its connection closed", servers[i].name)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the stuck connection was not closed at the deadline", servers[i].name)
		}
	}
}

// Regression (fifth review): a client that sent complete headers and then
// stalled its body held the connection for ever after a refusal (the handler
// answered without reading the body, and net/http waited, with no deadline,
// to drain it). The drain is now bounded.
func TestStalledBodyAfterAnAnswerIsCutOff(t *testing.T) {
	shrinkTimeouts(t, 300*time.Millisecond, 300*time.Millisecond)
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "AccessDenied", http.StatusForbidden) // body never read
	}))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("PUT /b/k HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\nab")); err != nil {
		t.Fatal(err)
	}
	if !closedWithin(conn, 3*time.Second) {
		t.Fatal("connection still open 3s after an answered request whose body stalls")
	}
}

// The drain deadline must not affect the next request on a healthy
// keep-alive connection, nor a handler that reads a slow body itself.
func TestBodyDrainDeadlineDoesNotLeakIntoTheNextRequest(t *testing.T) {
	shrinkTimeouts(t, 2*time.Second, time.Minute)
	bodyDrainTimeout = 200 * time.Millisecond
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		_, _ = fmt.Fprintf(w, "%d", n)
	}))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	for i := 0; i < 2; i++ {
		if _, err := fmt.Fprint(conn, "PUT /b/k HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond) // slower than the drain deadline
		if _, err := fmt.Fprint(conn, "abcd"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("request %d on the kept-alive connection: %v", i+1, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(b) != "4" {
			t.Fatalf("request %d: body %q", i+1, b)
		}
	}
}

// Audit (fifth review): archives staged by a run that stopped mid-upload are
// removed when the server starts, through New, not only by the helper.
func TestServerStartClearsStagedSnowballArchives(t *testing.T) {
	cfg := shardTestConfig(t, 1)
	staging := filepath.Join(cfg.Storage.DataDir, ".snowball")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "archive-left"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staged archive survived startup: %v", err)
	}
}

// Regression (tenth review): the dashboard sign-in route reads its body
// before any authentication, with no deadline, so a client that sent the
// headers and stalled the body held the connection for ever: slowloris moved
// from the headers to the body. That body has the header limit too, and an
// honest login is unaffected.
func TestStalledSignInBodyIsCutOff(t *testing.T) {
	shrinkTimeouts(t, 300*time.Millisecond, time.Minute)
	reader := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil { // reads before any auth, as the login does
			w.WriteHeader(http.StatusBadRequest) // or net/http answers 200
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	addr := serve(t, signInDeadline(reader))
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/oidc"} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte("POST " + path + " HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{"))
		if !closedWithin(conn, 3*time.Second) {
			t.Fatalf("%s: a stalled sign-in body still holds the connection after 3s (limit 300ms)", path)
		}
		conn.Close()
	}
	// An honest sign-in, and an authenticated route's body, are not cut off.
	for _, path := range []string{"/api/v1/auth/login", "/b/k"} {
		resp, err := http.Post("http://"+addr+path, "application/json", strings.NewReader(`{"accessKey":"a","secretKey":"b"}`))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	// A slow but moving upload to any other route keeps going past the limit.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	slowPost := func(t *testing.T, addr, path string) {
		t.Helper()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("POST " + path + " HTTP/1.1\r\nHost: x\r\nContent-Length: 3\r\n\r\na"))
		time.Sleep(500 * time.Millisecond)
		_, _ = conn.Write([]byte("b"))
		time.Sleep(500 * time.Millisecond)
		_, _ = conn.Write([]byte("c"))
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || !strings.Contains(line, "200") {
			t.Fatalf("slow but moving body to %s: %q %v, want 200", path, line, err)
		}
	}
	slowPost(t, addr, "/b/k?uploadId=x")
	// Regression (eleventh review): a listener without the dashboard API (the
	// S3 listener when the console has its own port) serves the same path as
	// an S3 request, bucket "api", whose authenticated body has no limit.
	slowPost(t, serve(t, reader), "/api/v1/auth/k?uploadId=x")
}

// Only listeners that serve the dashboard API get the sign-in limit: the
// main one unless the console has its own port, and the console listener.
func TestSignInDeadlineOnlyWhereTheDashboardAPIIs(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"if !splitConsole {\n\t\thandler = signInDeadline(handler)",
		"consoleServer = newHTTPServer(consoleAddr, signInDeadline(chandler))",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("server.go no longer has %q", want)
		}
	}
	if strings.Count(string(src), "signInDeadline(") != 2 {
		t.Errorf("signInDeadline is applied %d times, want 2", strings.Count(string(src), "signInDeadline("))
	}
}

// The same through the real dashboard API: an unauthenticated client sends
// complete headers for the login, then stalls its body.
func TestStalledDashboardLoginBodyIsCutOff(t *testing.T) {
	shrinkTimeouts(t, 300*time.Millisecond, time.Second)
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dataDir := filepath.Join(dir, "data")
	_ = os.MkdirAll(dataDir, 0o755)
	engine, err := storage.NewFileSystem(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Auth.AdminAccessKey = "admin"
	cfg.Auth.AdminSecretKey = "secret"
	h := api.NewAPIHandler(store, engine, metrics.NewCollector(store, engine), cfg, api.NewActivityLog())

	addr := serve(t, signInDeadline(h))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("POST /api/v1/auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{"))
	// 10x the header timeout and 3x the idle timeout.
	if !closedWithin(conn, 3*time.Second) {
		t.Fatal("unauthenticated login with a stalled body still holds the connection after 3s (header limit 300ms, idle 1s)")
	}
}

// deadlineRecorder records SetReadDeadline calls made through
// http.ResponseController.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	calls int
}

func (d *deadlineRecorder) SetReadDeadline(time.Time) error { d.calls++; return nil }

// The post-answer drain deadline is for HTTP/1.1, where net/http drains an
// unread body. HTTP/2 resets the stream instead, and a deadline there only
// kept a timer and the stream alive for 10 s after every request (5.0.11).
func TestBodyDrainDeadlineOnlyOnHTTP1(t *testing.T) {
	h := boundBodyDrain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, c := range []struct {
		major int
		want  int
	}{{1, 1}, {2, 0}} {
		rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		req := httptest.NewRequest(http.MethodPut, "/b/k", nil)
		req.ProtoMajor = c.major
		h.ServeHTTP(rec, req)
		if rec.calls != c.want {
			t.Errorf("HTTP/%d: %d drain deadlines set, want %d", c.major, rec.calls, c.want)
		}
	}
}
