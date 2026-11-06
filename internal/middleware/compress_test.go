package middleware

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompressGzip(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/example", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}

	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if string(body) != `{"hello":"world"}` {
		t.Fatalf("body = %q", body)
	}
}

func TestCompressDeflate(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("deflated"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	req.Header.Set("Accept-Encoding", "deflate")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "deflate" {
		t.Fatalf("Content-Encoding = %q, want deflate", got)
	}
	zr, err := zlib.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("zlib.NewReader: %v", err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read deflate body: %v", err)
	}
	if string(body) != "deflated" {
		t.Fatalf("body = %q", body)
	}
}

func TestCompressNegotiation(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	h := Compress(handler)

	tests := []struct {
		name   string
		accept string
		want   string
	}{
		{"gzip preferred when both", "gzip, deflate", "gzip"},
		{"deflate when gzip refused", "gzip;q=0, deflate", "deflate"},
		{"gzip only", "gzip", "gzip"},
		{"unsupported br ignored", "br", ""},
		{"wildcard allows gzip", "*", "gzip"},
		{"explicit refusal overrides wildcard", "gzip;q=0, *", "deflate"},
		{"no header", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/example", nil)
			if tc.accept != "" {
				req.Header.Set("Accept-Encoding", tc.accept)
			}
			h.ServeHTTP(rec, req)
			if got := rec.Header().Get("Content-Encoding"); got != tc.want {
				t.Fatalf("Accept-Encoding %q: Content-Encoding = %q, want %q", tc.accept, got, tc.want)
			}
		})
	}
}

func TestCompressPassesThrough(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*http.Request)
		handler    http.HandlerFunc
		wantEncode string
	}{
		{
			name:    "binary content",
			handler: binaryHandler,
		},
		{
			name:    "range request",
			prepare: func(r *http.Request) { r.Header.Set("Range", "bytes=0-4") },
			handler: textHandler,
		},
		{
			name:    "HEAD request",
			prepare: func(r *http.Request) { r.Method = http.MethodHead },
			handler: textHandler,
		},
		{
			name:       "already encoded",
			handler:    alreadyEncodedHandler,
			wantEncode: "br",
		},
		{
			name:    "no content",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/example", nil)
			req.Header.Set("Accept-Encoding", "gzip, deflate")
			if tc.prepare != nil {
				tc.prepare(req)
			}
			Compress(tc.handler).ServeHTTP(rec, req)
			if got := rec.Header().Get("Content-Encoding"); got != tc.wantEncode {
				t.Fatalf("Content-Encoding = %q, want %q", got, tc.wantEncode)
			}
			if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
				t.Fatalf("Vary = %q, want Accept-Encoding", got)
			}
		})
	}
}

func TestCompressSSEKeepsFlusher(t *testing.T) {
	flushed := false
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("wrapper does not implement http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: tick\n\n"))
		f.Flush()
		flushed = true
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if !flushed {
		t.Fatal("handler did not flush")
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("SSE Content-Encoding = %q, want none", got)
	}
	if rec.Body.String() != "data: tick\n\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func textHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<html></html>"))
}

func binaryHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write([]byte{0x00, 0x01, 0x02, 0x03})
}

func alreadyEncodedHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Encoding", "br")
	_, _ = w.Write([]byte("opaque"))
}

func TestCompressCompressibleMediaType(t *testing.T) {
	tests := map[string]bool{
		"text/html":                true,
		"text/css":                 true,
		"text/javascript":          true,
		"text/event-stream":        false,
		"application/json":         true,
		"application/javascript":   true,
		"application/xml":          true,
		"application/wasm":         true,
		"application/zip":          false,
		"application/octet-stream": false,
		"image/svg+xml":            true,
		"image/png":                false,
		"font/woff2":               false,
	}
	for ct, want := range tests {
		if got := compressibleMediaType(ct); got != want {
			t.Errorf("compressibleMediaType(%q) = %v, want %v", ct, got, want)
		}
	}
}

func TestCompressMultipleWrites(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for _, part := range []string{`{"a":`, `1}`} {
			if _, err := w.Write([]byte(part)); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/example", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	var buf bytes.Buffer
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	if _, err := io.Copy(&buf, zr); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if buf.String() != `{"a":1}` {
		t.Fatalf("body = %q", buf.String())
	}
}

func TestCompressPreservesExistingVary(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/example", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	vary := strings.Join(rec.Header().Values("Vary"), ",")
	if !strings.Contains(vary, "Origin") || !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want both Origin and Accept-Encoding", vary)
	}
}

func TestCompressPreservesVaryWhenCompressionIsSkipped(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))

	tests := []struct {
		name    string
		prepare func(*http.Request)
	}{
		{name: "no accepted encoding"},
		{name: "HEAD", prepare: func(r *http.Request) { r.Method = http.MethodHead }},
		{name: "range", prepare: func(r *http.Request) { r.Header.Set("Range", "bytes=0-1") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/example", nil)
			if tc.prepare != nil {
				tc.prepare(req)
			}
			h.ServeHTTP(rec, req)

			vary := strings.Join(rec.Header().Values("Vary"), ",")
			if !strings.Contains(vary, "Origin") || !strings.Contains(vary, "Accept-Encoding") {
				t.Fatalf("Vary = %q, want both Origin and Accept-Encoding", vary)
			}
			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want none", got)
			}
		})
	}
}
