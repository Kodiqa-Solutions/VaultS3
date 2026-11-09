package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"sync/atomic"
	"time"
)

var requestIDSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._\-]`)

// requestCounter is used to generate unique request IDs.
var requestCounter uint64

// generateRequestID creates a short unique ID: timestamp-counter.
func generateRequestID() string {
	n := atomic.AddUint64(&requestCounter, 1)
	return fmt.Sprintf("%d-%06d", time.Now().UnixMilli()%1000000, n)
}

// RequestID adds an X-Request-Id header to every response.
// If the incoming request already has one, it is reused.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = generateRequestID()
		} else {
			// Sanitize client-provided request ID to prevent header injection
			id = requestIDSanitizer.ReplaceAllString(id, "")
			if len(id) > 128 {
				id = id[:128]
			}
			if id == "" {
				id = generateRequestID()
			}
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

// LatencyRecorder is the interface for recording request latency.
type LatencyRecorder interface {
	RecordLatency(d time.Duration)
}

// Latency measures request duration and records it via the LatencyRecorder.
func Latency(recorder LatencyRecorder, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		recorder.RecordLatency(time.Since(start))
	})
}

// SecurityHeaders adds security headers to responses (for dashboard routes).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// ObjectSandboxCSP is the policy for an object served by the S3 API: it is
// displayed, but runs no script and is its own origin. Objects are served
// from the same origin as the dashboard, so an HTML page a user uploaded ran
// as the dashboard: one link opened by the admin handed it the admin's
// session token.
const ObjectSandboxCSP = "sandbox"

// WebsiteSandboxCSP is the policy for a website bucket's pages: their scripts
// run, but in an origin of their own (no allow-same-origin), so they cannot
// read the dashboard's storage or call its API as the user.
const WebsiteSandboxCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads allow-popups-to-escape-sandbox"

// PanicRecovery catches panics, logs the stack trace, and returns 500.
func PanicRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// ErrAbortHandler means "cut the connection", for example when
				// ReverseProxy loses the peer part way through a body. net/http
				// does that only if it sees the panic. Answering it here would
				// end a half sent response normally, so the client could take a
				// truncated body as complete.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				stack := string(debug.Stack())
				reqID := w.Header().Get("X-Request-Id")
				slog.Error("panic recovered",
					"request_id", reqID,
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
					"stack", stack,
				)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
