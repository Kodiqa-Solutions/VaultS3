package s3

import (
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/middleware"
)

// sandboxWriter adds the object sandbox policy (middleware.ObjectSandboxCSP)
// to a response whose type a browser could run as a page: HTML, SVG, XML,
// script, or no type at all. Objects are served from the dashboard's origin,
// so such a page uploaded by a user ran as the dashboard. A response that
// already has a policy (a website page) keeps it.
//
// Passive types are left alone: the sandbox made a browser refuse to show a
// PDF (its viewer is a script), and nothing in an image, a video or plain
// text runs.
type sandboxWriter struct {
	http.ResponseWriter
	done bool
}

func (s *sandboxWriter) decide() {
	if s.done {
		return
	}
	s.done = true
	h := s.Header()
	if h.Get("Content-Security-Policy") == "" && activeContentType(h.Get("Content-Type")) {
		h.Set("Content-Security-Policy", middleware.ObjectSandboxCSP)
	}
}

func (s *sandboxWriter) WriteHeader(code int) {
	s.decide()
	s.ResponseWriter.WriteHeader(code)
}

func (s *sandboxWriter) Write(b []byte) (int, error) {
	s.decide()
	return s.ResponseWriter.Write(b)
}

// ReadFrom keeps io.Copy's fast path (sendfile) to the connection.
func (s *sandboxWriter) ReadFrom(r io.Reader) (int64, error) {
	s.decide()
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(s.ResponseWriter, r)
}

func (s *sandboxWriter) Flush() {
	s.decide()
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *sandboxWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// activeContentType reports whether a response of this type could run
// script in a browser. Only types known to be passive say no; anything
// unknown is treated as active.
func activeContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || mt == "" {
		return true
	}
	mt = strings.ToLower(mt)
	if strings.Contains(mt, "xml") || strings.Contains(mt, "html") || strings.Contains(mt, "script") {
		return true
	}
	major, _, _ := strings.Cut(mt, "/")
	switch major {
	case "image", "video", "audio", "font":
		return false
	}
	switch mt {
	case "text/plain", "text/csv", "text/markdown", "text/css",
		"application/pdf", "application/json", "application/octet-stream",
		"application/zip", "application/gzip", "application/x-gzip", "application/x-tar",
		"application/zstd", "application/x-7z-compressed", "application/vnd.apache.parquet",
		"application/x-ndjson", "application/msword":
		return false
	}
	if strings.HasPrefix(mt, "application/vnd.openxmlformats-officedocument.") || strings.HasPrefix(mt, "application/vnd.ms-") {
		return false
	}
	return true
}
