package server

import (
	"net"
	"net/http"
	"time"
)

// traceRequests records every request for the dashboard's live trace view
// (/api/v1/trace), which answered 503 forever because nothing recorded into it.
// The client address is the TCP peer, not X-Forwarded-For.
func traceRequests(tb traceRecorder, next http.Handler) http.Handler {
	if tb == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		tb.Record(r.Method, r.URL.Path, sw.status, time.Since(start), ip)
	})
}

// traceRecorder is what api.TraceBroadcaster provides.
type traceRecorder interface {
	Record(method, path string, status int, duration time.Duration, clientIP string)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush keeps streaming responses (event and log streams, large object reads)
// working through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
