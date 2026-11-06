package middleware

import (
	"compress/gzip"
	"compress/zlib"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// Compress transparently gzip- or deflate-encodes responses when the client
// advertises support through Accept-Encoding. Only gzip and deflate are
// supported; any other encoding (br, zstd, ...) is ignored.
//
// The dashboard and its REST API are mounted behind this middleware. Binary,
// already-compressed payloads and streaming responses are passed through
// untouched, as are range requests and responses that already carry a
// Content-Encoding.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always advertise that the response depends on Accept-Encoding, even
		// when this particular request is served uncompressed.
		addVary(w.Header(), "Accept-Encoding")

		encoding := ""
		if r.Method != http.MethodHead && r.Header.Get("Range") == "" {
			encoding = negotiatedEncoding(r.Header.Get("Accept-Encoding"))
		}

		cw := &compressWriter{
			ResponseWriter: w,
			encoding:       encoding,
		}
		defer cw.Close()
		next.ServeHTTP(cw, r)
	})
}

// negotiatedEncoding picks the compression to use from an Accept-Encoding
// header. gzip is preferred over deflate when both are acceptable. A q-value of
// zero explicitly disables that encoding.
func negotiatedEncoding(header string) string {
	gzipOK := acceptsEncoding(header, "gzip")
	deflateOK := acceptsEncoding(header, "deflate")

	switch {
	case gzipOK:
		return "gzip"
	case deflateOK:
		return "deflate"
	default:
		return ""
	}
}

// acceptsEncoding reports whether the Accept-Encoding header allows name with a
// non-zero q-value. A wildcard also permits everything, except that an explicit
// entry for the encoding overrides it (so "gzip;q=0, *" refuses gzip).
func acceptsEncoding(header, name string) bool {
	if strings.TrimSpace(header) == "" {
		return false
	}

	allowed := false
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		token := strings.ToLower(strings.TrimSpace(fields[0]))
		if token == "" {
			continue
		}
		q := 1.0
		for _, field := range fields[1:] {
			kv := strings.SplitN(strings.TrimSpace(field), "=", 2)
			if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "q") {
				q = parseQ(strings.TrimSpace(kv[1]))
			}
		}
		switch token {
		case name:
			return q > 0
		case "*":
			allowed = q > 0
		}
	}
	return allowed
}

func parseQ(s string) float64 {
	q, err := strconv.ParseFloat(s, 64)
	if err != nil || q < 0 || q > 1 {
		return 1
	}
	return q
}

// compressWriter delays the compression decision until WriteHeader, when the
// handler has set the content type and status. It implements http.Flusher so
// streaming handlers (the API's SSE endpoints) keep working.
type compressWriter struct {
	http.ResponseWriter
	encoding string

	writer      io.WriteCloser
	wroteHeader bool
}

func (w *compressWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	// A handler may have replaced Vary (the API's CORS path does), so make
	// sure Accept-Encoding survives before the headers go out.
	addVary(w.Header(), "Accept-Encoding")

	if compressibleResponse(w.Header(), status) {
		w.startCompression()
	}
	w.ResponseWriter.WriteHeader(status)
}

// addVary appends value to Vary unless it is already listed.
func addVary(h http.Header, value string) {
	for _, line := range h.Values("Vary") {
		for _, token := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(token), value) {
				return
			}
		}
	}
	h.Add("Vary", value)
}

func (w *compressWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.writer != nil {
		return w.writer.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// Flush implements http.Flusher. SSE responses are not compressed, but the
// wrapper must still expose the interface so handlers don't reject them.
func (w *compressWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.writer.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Close flushes and closes the compressor, writing the gzip/zlib trailer.
func (w *compressWriter) Close() error {
	if w.writer == nil {
		return nil
	}
	err := w.writer.Close()
	w.writer = nil
	return err
}

func (w *compressWriter) startCompression() {
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Encoding", w.encoding)

	switch w.encoding {
	case "gzip":
		w.writer = gzip.NewWriter(w.ResponseWriter)
	case "deflate":
		// HTTP "deflate" means a zlib-wrapped DEFLATE stream (RFC 1950).
		w.writer = zlib.NewWriter(w.ResponseWriter)
	}
}

// compressibleResponse decides whether a response should be compressed.
func compressibleResponse(h http.Header, status int) bool {
	// No body, or a body we must not transform.
	if status < 200 || status == http.StatusNoContent || status == http.StatusNotModified {
		return false
	}
	if h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" {
		return false
	}

	ct := h.Get("Content-Type")
	if ct == "" {
		// net/http sniffs the type on the first write; most handlers that leave
		// it empty are returning text or JSON, so allow it.
		return true
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return compressibleMediaType(mediaType)
}

func compressibleMediaType(mediaType string) bool {
	switch {
	case strings.HasPrefix(mediaType, "text/"):
		// Server-sent events are an unbuffered stream.
		return mediaType != "text/event-stream"
	case strings.HasPrefix(mediaType, "application/"):
		switch mediaType {
		case "application/json",
			"application/ld+json",
			"application/manifest+json",
			"application/javascript",
			"application/x-javascript",
			"application/xml",
			"application/xhtml+xml",
			"application/rss+xml",
			"application/atom+xml",
			"application/wasm":
			return true
		}
		// Types suffixed with +json or +xml are text-like as well.
		return strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")
	case mediaType == "image/svg+xml":
		return true
	default:
		return false
	}
}
