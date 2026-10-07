package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metrics"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// failingEngine drains the body (as the real storage path does) then fails the
// write, simulating e.g. a full disk mid-upload.
type failingEngine struct {
	storage.Engine
}

func (failingEngine) PutObject(bucket, key string, r io.Reader, size int64) (int64, string, error) {
	io.Copy(io.Discard, r)
	return 0, "", errors.New("write object: no space left on device")
}

// TestUploadReportsStorageError covers issue #26: a mid-upload storage failure
// (e.g. a full disk) must be logged and returned to the client with the real
// reason, not silently swallowed into a 200 with an empty result.
func TestUploadReportsStorageError(t *testing.T) {
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	dataDir := filepath.Join(dir, "data")
	os.MkdirAll(dataDir, 0755)
	base, err := storage.NewFileSystem(dataDir)
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}
	engine := failingEngine{Engine: base}
	cfg := &config.Config{}
	cfg.Auth.AdminAccessKey = "admin"
	cfg.Auth.AdminSecretKey = "secret"
	h := NewAPIHandler(store, engine, metrics.NewCollector(store, engine), cfg, NewActivityLog())
	if err := store.CreateBucket("vault"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "big.img")
	part.Write([]byte("some object bytes that will fail to store"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/buckets/vault/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// The handler authorizes each file against the caller, as ServeHTTP does
	// for the route, so the request carries the admin session it would have.
	req.Header.Set("Authorization", "Bearer "+getToken(t, h))
	rr := httptest.NewRecorder()
	h.handleUpload(rr, req, "vault")

	// A storage failure must NOT be reported as success.
	if rr.Code == http.StatusOK {
		t.Fatalf("storage failure returned 200 (silently swallowed); body %s", rr.Body.String())
	}
	var results []uploadResult
	if err := json.Unmarshal(rr.Body.Bytes(), &results); err != nil {
		t.Fatalf("unmarshal body: %v (%s)", err, rr.Body.String())
	}
	if len(results) != 1 || results[0].Error == "" {
		t.Fatalf("expected one failed result carrying an error reason, got %+v", results)
	}
	if results[0].Key != "big.img" {
		t.Fatalf("failed result key = %q, want big.img", results[0].Key)
	}
	// And nothing should be recorded as stored.
	if _, err := store.GetObjectMeta("vault", "big.img"); err == nil {
		t.Fatal("object metadata was written despite the storage failure")
	}
}

// TestUploadStreamsAndPreservesFolderPath covers the dashboard upload rewrite for
// issue #26: it streams the multipart body (no whole-file temp buffering) and
// preserves a relative folder path in the filename instead of flattening it to
// the base name.
func TestUploadStreamsAndPreservesFolderPath(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("vault"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// The dashboard sends folder uploads with the relative path as the filename.
	part, err := mw.CreateFormFile("file", "backups/2026/report.bin")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	content := []byte("hello-large-content-streamed-through")
	part.Write(content)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/buckets/vault/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// The handler authorizes each file against the caller, as ServeHTTP does
	// for the route, so the request carries the admin session it would have.
	req.Header.Set("Authorization", "Bearer "+getToken(t, h))
	rr := httptest.NewRecorder()
	h.handleUpload(rr, req, "vault")

	if rr.Code != http.StatusOK {
		t.Fatalf("upload: status %d, body %s", rr.Code, rr.Body.String())
	}

	// The object must be stored under the full nested key, not the base name.
	meta, err := store.GetObjectMeta("vault", "backups/2026/report.bin")
	if err != nil {
		t.Fatalf("object not stored under nested key (folder flattened?): %v", err)
	}
	if meta.Size != int64(len(content)) {
		t.Fatalf("stored size = %d, want %d", meta.Size, len(content))
	}
	if _, err := store.GetObjectMeta("vault", "report.bin"); err == nil {
		t.Fatal("object was flattened to the base name report.bin")
	}
}
