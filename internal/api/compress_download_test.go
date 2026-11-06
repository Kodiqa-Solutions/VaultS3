package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/middleware"
)

// The server mounts the console API behind middleware.Compress. A file browser
// download of a text object must still arrive as the stored bytes with its
// Content-Length, or the browser shows no size or progress and the server
// gzips a multi-GB log on the fly. A JSON answer from the same API must still
// be compressed, so the guard does not switch compression off altogether.
func TestCompressedConsoleKeepsDownloadsUncompressed(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	wrapped := middleware.Compress(h)

	if rr := doRequest(h, "POST", "/buckets", map[string]string{"name": "logs"}, token); rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("create bucket: %d %s", rr.Code, rr.Body.String())
	}
	body := strings.Repeat("timestamp,level,message\n2026-11-06,info,hello\n", 2000)
	if _, _, err := h.engine.PutObject("logs", "app.csv", strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: "logs", Key: "app.csv", Size: int64(len(body)), ContentType: "text/csv"}); err != nil {
		t.Fatal(err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept-Encoding", "gzip, deflate")
		rr := httptest.NewRecorder()
		wrapped.ServeHTTP(rr, req)
		return rr
	}

	rr := get("/buckets/logs/download/app.csv")
	if rr.Code != http.StatusOK {
		t.Fatalf("download: %d %s", rr.Code, rr.Body.String())
	}
	if ce := rr.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("download Content-Encoding = %q, want none", ce)
	}
	if cl := rr.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Fatalf("download Content-Length = %q, want %d", cl, len(body))
	}
	if rr.Body.String() != body {
		t.Fatalf("download body differs from the stored object (%d vs %d bytes)", rr.Body.Len(), len(body))
	}

	rr = get("/buckets")
	if rr.Code != http.StatusOK {
		t.Fatalf("list buckets: %d %s", rr.Code, rr.Body.String())
	}
	if ce := rr.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("JSON Content-Encoding = %q, want gzip", ce)
	}
	zr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plain), `"logs"`) {
		t.Fatalf("decompressed bucket list = %s", plain)
	}
}
