package middleware

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
	defer func() { _ = zr.Close() }()
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
	defer func() { _ = zr.Close() }()
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
	defer func() { _ = zr.Close() }()
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

// A missing Content-Type is passed through. net/http only sniffs a type when
// no Content-Encoding is set, so compressing would send a body with no type.
func TestCompressSkipsResponsesWithoutContentType(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>untyped</body></html>"))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/example", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want none", got)
	}
	if got := rec.Body.String(); got != "<html><body>untyped</body></html>" {
		t.Fatalf("body = %q", got)
	}
}

// Attachment downloads keep their Content-Length. An inline response of the
// same type is still compressed, so the guard is about the disposition only.
func TestCompressSkipsAttachments(t *testing.T) {
	payload := strings.Repeat("a,b,c\n", 500)
	serve := func(disposition string) *httptest.ResponseRecorder {
		h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			if disposition != "" {
				w.Header().Set("Content-Disposition", disposition)
			}
			_, _ = w.Write([]byte(payload))
		}))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/example", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, d := range []string{`attachment; filename="data.csv"`, `Attachment; filename="DATA.CSV"`} {
		rec := serve(d)
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("%s: Content-Encoding = %q, want none", d, got)
		}
		if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(payload)) {
			t.Fatalf("%s: Content-Length = %q, want %d", d, got, len(payload))
		}
		if rec.Body.String() != payload {
			t.Fatalf("%s: body changed", d)
		}
	}
	for _, d := range []string{"", "inline"} {
		if got := serve(d).Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("disposition %q: Content-Encoding = %q, want gzip", d, got)
		}
	}
}

// http.ResponseController must reach the real connection through the wrapper.
// Over a real server, SetWriteDeadline fails with ErrNotSupported unless the
// wrapper unwraps.
func TestCompressUnwrapsForResponseController(t *testing.T) {
	srv := httptest.NewServer(Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SetWriteDeadline through the wrapper: %d %s", resp.StatusCode, b)
	}
}

// When nothing is negotiated (no Accept-Encoding, HEAD, Range) the response is
// untouched: no Content-Encoding header at all, not even an empty one, and the
// handler's Content-Length survives. Get("Content-Encoding") == "" cannot tell
// an absent header from an empty one, so this checks the map directly.
func TestCompressLeavesUnnegotiatedResponsesUntouched(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", "13")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	tests := []struct {
		name    string
		prepare func(*http.Request)
	}{
		{name: "no Accept-Encoding"},
		{name: "only unsupported encodings", prepare: func(r *http.Request) { r.Header.Set("Accept-Encoding", "br, zstd") }},
		{name: "HEAD", prepare: func(r *http.Request) { r.Method = http.MethodHead; r.Header.Set("Accept-Encoding", "gzip") }},
		{name: "Range", prepare: func(r *http.Request) { r.Header.Set("Range", "bytes=0-3"); r.Header.Set("Accept-Encoding", "gzip") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
			if tc.prepare != nil {
				tc.prepare(req)
			}
			h.ServeHTTP(rec, req)
			if v, ok := rec.Header()["Content-Encoding"]; ok {
				t.Fatalf("Content-Encoding present as %q, want absent", v)
			}
			if got := rec.Header().Get("Content-Length"); got != "13" {
				t.Fatalf("Content-Length = %q, want 13", got)
			}
		})
	}
}

// An informational status is not the final one. Before this guard the wrapper
// latched on 100 and dropped the handler's 404, so the client saw 200.
func TestCompressKeepsFinalStatusAfter1xx(t *testing.T) {
	srv := httptest.NewServer(Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != `{"error":"not found"}` {
		t.Fatalf("body = %q", body)
	}
}

// Pooled writers must never leak one response's bytes into another. Many
// concurrent responses, alternating gzip and deflate, each must decode to
// exactly its own body.
func TestCompressPooledWritersKeepResponsesApart(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + r.URL.Query().Get("id") + `","pad":"` + strings.Repeat(r.URL.Query().Get("id"), 200) + `"}`))
	}))
	var wg sync.WaitGroup
	errs := make(chan string, 400)
	for i := 0; i < 400; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := strconv.Itoa(i)
			enc := "gzip"
			if i%2 == 1 {
				enc = "deflate"
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/x?id="+id, nil)
			req.Header.Set("Accept-Encoding", enc)
			h.ServeHTTP(rec, req)
			var zr io.ReadCloser
			var err error
			if enc == "gzip" {
				zr, err = gzip.NewReader(rec.Body)
			} else {
				zr, err = zlib.NewReader(rec.Body)
			}
			if err != nil {
				errs <- id + ": " + err.Error()
				return
			}
			got, err := io.ReadAll(zr)
			want := `{"id":"` + id + `","pad":"` + strings.Repeat(id, 200) + `"}`
			if err != nil || string(got) != want {
				errs <- id + ": body mismatch"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// In a cluster the console API forwards some requests to the node that owns
// the object through httputil.ReverseProxy. Three shapes must survive the
// wrapper: a peer that already compressed (new version, passed through, not
// compressed twice), a plain peer (old version, compressed here), and a peer
// that sends 103 first, after which ReverseProxy clears the header map.
func TestCompressBehindReverseProxy(t *testing.T) {
	const body = `{"buckets":["a","b","c","d","e","f","g","h","i","j","k","l","m"]}`
	peerNew := httptest.NewServer(Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})))
	defer peerNew.Close()
	peerOld := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer peerOld.Close()
	peerHints := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</x.js>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(body))
	}))
	defer peerHints.Close()

	for _, tc := range []struct {
		name   string
		peer   string
		status int
	}{
		{"peer already compressed", peerNew.URL, http.StatusOK},
		{"plain peer", peerOld.URL, http.StatusOK},
		{"peer sends 103 then 404", peerHints.URL, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, _ := url.Parse(tc.peer)
			front := httptest.NewServer(Compress(httputil.NewSingleHostReverseProxy(target)))
			defer front.Close()

			req, _ := http.NewRequest(http.MethodGet, front.URL+"/api/v1/buckets", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if got := resp.Header.Values("Content-Encoding"); len(got) != 1 || got[0] != "gzip" {
				t.Fatalf("Content-Encoding = %q, want exactly one gzip", got)
			}
			if v := strings.Join(resp.Header.Values("Vary"), ","); !strings.Contains(v, "Accept-Encoding") {
				t.Fatalf("Vary = %q, want Accept-Encoding", v)
			}
			zr, err := gzip.NewReader(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(zr)
			if err != nil || string(got) != body {
				t.Fatalf("body = %q (err %v), want the peer's JSON decoded once", got, err)
			}
		})
	}
}

// Behind the server's PanicRecovery, a handler that dies part way through a
// compressed body must not reach the client as a complete stream. Two kinds:
// ErrAbortHandler (ReverseProxy losing a peer), which cuts the connection, and
// an ordinary panic, which PanicRecovery answers. For the second, a deferred
// Close used to write a valid gzip trailer over the first half, so a browser
// decoded the truncated half as the whole response.
func TestCompressPanicMidBodyIsNotAValidStream(t *testing.T) {
	half := strings.Repeat(`{"k":"v"},`, 2000)
	for _, tc := range []struct {
		name string
		val  any
	}{
		{"abort", http.ErrAbortHandler},
		{"ordinary panic", "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(PanicRecovery(Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("[" + half))
				w.(http.Flusher).Flush()
				panic(tc.val)
			}))))
			defer srv.Close()

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				return // cut before the headers: also not a complete response
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(resp.Body)
			// Decode only the first gzip member, as a browser does: bytes
			// after a complete member are ignored, so a trailer there would
			// make the cut invisible.
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				return
			}
			zr.Multistream(false)
			if got, err := io.ReadAll(zr); err == nil {
				t.Fatalf("truncated body decoded as a complete gzip stream (%d bytes)", len(got))
			}
		})
	}
}

// A compressed response must not advertise byte ranges, which would count the
// uncompressed body. An uncompressed one keeps the handler's header.
func TestCompressDropsAcceptRangesWhenCompressing(t *testing.T) {
	h := Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		w.Header().Set("Accept-Ranges", "bytes")
		_, _ = w.Write([]byte(strings.Repeat("let a = 1;\n", 100)))
	}))
	for _, tc := range []struct {
		accept, wantCE, wantAR string
	}{
		{"gzip", "gzip", ""},
		{"", "", "bytes"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/dashboard/assets/app.js", nil)
		if tc.accept != "" {
			req.Header.Set("Accept-Encoding", tc.accept)
		}
		h.ServeHTTP(rec, req)
		if ce, ar := rec.Header().Get("Content-Encoding"), rec.Header().Get("Accept-Ranges"); ce != tc.wantCE || ar != tc.wantAR {
			t.Fatalf("Accept-Encoding %q: Content-Encoding=%q Accept-Ranges=%q, want %q and %q", tc.accept, ce, ar, tc.wantCE, tc.wantAR)
		}
	}
}
