package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
)

// Connection limits shared by every listener (S3 and dashboard, inter-node,
// separate console). Variables rather than constants only so tests can shrink
// them.
var (
	// readHeaderTimeout bounds how long a client may take to send the request
	// line and headers. Without it a client that sends headers a byte at a
	// time holds a connection and a goroutine open for ever (slowloris), and
	// enough of them exhaust the server's file descriptors.
	readHeaderTimeout = 30 * time.Second

	// idleTimeout closes a keep-alive connection that has sat between
	// requests this long. It never applies while a request is in flight, so
	// a long download or a live stream is not cut. Set from
	// server.idle_timeout_secs; the default, 650s, is above the idle timeout
	// of common load balancers (Google Cloud 600s, AWS ALB 60s by default),
	// which must be shorter than the server's or they reuse a connection the
	// server just closed.
	idleTimeout = 650 * time.Second

	// bodyDrainTimeout bounds how long, after a handler has answered, net/http
	// may wait for the rest of a body the handler did not read (it drains up
	// to 256 KiB to reuse the connection). Without it, a client that sent
	// complete headers and then stalled its body held the connection for
	// ever, even after an answer such as an authentication refusal.
	bodyDrainTimeout = 10 * time.Second
)

// setIdleTimeout applies server.idle_timeout_secs, or its default when it is
// 0 or negative.
func setIdleTimeout(cfg config.ServerConfig) {
	idleTimeout = time.Duration(cfg.EffectiveIdleTimeoutSecs()) * time.Second
}

// newHTTPServer builds an http.Server with the limits above.
//
// ReadTimeout and WriteTimeout stay unset on purpose. They cover the whole
// body, so any fixed value would cut off a large upload or download on a slow
// link. Once the headers are read, net/http clears the header deadline, and
// the body is read with no deadline, exactly as before.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           boundBodyDrain(handler),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// boundBodyDrain sets a read deadline once the handler has answered, so the
// drain of an unread body cannot wait for ever. The next request on the same
// connection resets the deadline as usual, and the handler itself still reads
// its body with no deadline.
func boundBodyDrain(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		// HTTP/2 does not drain an unread body (the stream is reset), and a
		// deadline there only kept a timer and the stream alive for 10 s
		// after every request.
		if r.ProtoMajor >= 2 {
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyDrainTimeout))
	})
}

// readsBodyBeforeAuth reports a request whose handler reads its body before
// authenticating: the dashboard sign-in routes (password and OIDC). Every
// other route authenticates from the headers first.
func readsBodyBeforeAuth(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/auth/")
}

// signInDeadline gives the dashboard sign-in body the header limit. Those
// routes read their small body before anyone is authenticated, so a client
// that sent the headers and stalled the body held the connection for ever
// without a login. It wraps only a listener that serves the dashboard API:
// on an S3-only listener the same path is an S3 request (bucket "api"),
// whose authenticated body has no limit. Applied outside the middleware, so
// it reaches the connection itself.
func signInDeadline(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if readsBodyBeforeAuth(r) {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(readHeaderTimeout))
		}
		h.ServeHTTP(w, r)
	})
}

// dashboardAPIPrefix is the path the dashboard API is mounted at.
const dashboardAPIPrefix = "/api/v1/"

// policyPostDeadline gives a presigned POST's form fields the header limit.
// They are read before anyone is authenticated, so a client that sent the
// headers and stalled the body held the connection for ever. The handler
// lifts the deadline once the form's signature and permission check out, and
// the file then streams with no deadline. On a listener that also serves the
// dashboard, its /api/v1/ routes are left alone: they authenticate first, and
// an upload there must not be cut off. Only that prefix: every other path,
// /api/ included, is an S3 request (bucket "api") and gets the limit.
func policyPostDeadline(servesAPI bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s3.MayBePolicyPost(r) && !(servesAPI && strings.HasPrefix(r.URL.Path, dashboardAPIPrefix)) {
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Now().Add(readHeaderTimeout))
			r = s3.WithBodyDeadlineRelease(r, func() { _ = rc.SetReadDeadline(time.Time{}) })
		}
		h.ServeHTTP(w, r)
	})
}

// namedServer is one listener to drain at shutdown.
type namedServer struct {
	name string
	srv  *http.Server
}

// drainServers shuts the listeners down side by side, so a slow one cannot
// spend the time the others need. A listener still busy when ctx ends has its
// remaining connections closed, so the process always stops. It returns the
// first listener's error, the one that decides the exit status.
func drainServers(ctx context.Context, timeout time.Duration, servers ...namedServer) error {
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, ns := range servers {
		if ns.srv == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ns.srv.Shutdown(ctx); err != nil {
				slog.Error("graceful shutdown timed out, closing the remaining connections", "listener", ns.name, "timeout", timeout, "error", err)
				_ = ns.srv.Close()
				errs[i] = err
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
