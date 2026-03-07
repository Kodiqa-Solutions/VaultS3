package api

import (
	"bytes"
	"encoding/json"
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

func newTestAPI(t *testing.T) (*APIHandler, *metadata.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	dataDir := filepath.Join(dir, "data")
	os.MkdirAll(dataDir, 0755)
	engine, err := storage.NewFileSystem(dataDir)
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}

	cfg := &config.Config{}
	cfg.Auth.AdminAccessKey = "admin"
	cfg.Auth.AdminSecretKey = "secret"
	cfg.Server.Port = 9000

	mc := metrics.NewCollector(store, engine)
	activity := NewActivityLog()

	return NewAPIHandler(store, engine, mc, cfg, activity), store
}

func doRequest(h http.Handler, method, path string, body interface{}, token string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// --- Auth tests ---

func TestLogin_Success(t *testing.T) {
	h, _ := newTestAPI(t)
	rr := doRequest(h, "POST", "/auth/login", loginRequest{
		AccessKey: "admin",
		SecretKey: "secret",
	}, "")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp loginResponse
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Token == "" {
		t.Error("expected non-empty token")
	}
}

func TestLogin_BadCredentials(t *testing.T) {
	h, _ := newTestAPI(t)
	rr := doRequest(h, "POST", "/auth/login", loginRequest{
		AccessKey: "admin",
		SecretKey: "wrong",
	}, "")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuth_MissingToken(t *testing.T) {
	h, _ := newTestAPI(t)
	rr := doRequest(h, "GET", "/buckets", nil, "")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuth_InvalidToken(t *testing.T) {
	h, _ := newTestAPI(t)
	rr := doRequest(h, "GET", "/buckets", nil, "invalid-token")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func getToken(t *testing.T, h *APIHandler) string {
	t.Helper()
	rr := doRequest(h, "POST", "/auth/login", loginRequest{
		AccessKey: "admin",
		SecretKey: "secret",
	}, "")
	var resp loginResponse
	json.NewDecoder(rr.Body).Decode(&resp)
	return resp.Token
}

// --- Bucket CRUD tests ---

func TestCreateBucket_Success(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	rr := doRequest(h, "POST", "/buckets", createBucketRequest{Name: "test-bucket"}, token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateBucket_InvalidName(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	tests := []struct {
		name string
		want int
	}{
		{"", http.StatusBadRequest},
		{"ab", http.StatusBadRequest},        // too short
		{"-invalid", http.StatusBadRequest},  // starts with hyphen
		{"UPPERCASE", http.StatusBadRequest}, // uppercase
		{"has..dots", http.StatusBadRequest}, // consecutive dots
	}

	for _, tt := range tests {
		rr := doRequest(h, "POST", "/buckets", createBucketRequest{Name: tt.name}, token)
		if rr.Code != tt.want {
			t.Errorf("bucket name %q: expected %d, got %d", tt.name, tt.want, rr.Code)
		}
	}
}

func TestCreateBucket_Duplicate(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	doRequest(h, "POST", "/buckets", createBucketRequest{Name: "test-bucket"}, token)
	rr := doRequest(h, "POST", "/buckets", createBucketRequest{Name: "test-bucket"}, token)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rr.Code)
	}
}

func TestListBuckets(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	doRequest(h, "POST", "/buckets", createBucketRequest{Name: "bucket-a"}, token)
	doRequest(h, "POST", "/buckets", createBucketRequest{Name: "bucket-b"}, token)

	rr := doRequest(h, "GET", "/buckets", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var items []bucketListItem
	json.NewDecoder(rr.Body).Decode(&items)
	if len(items) != 2 {
		t.Errorf("expected 2 buckets, got %d", len(items))
	}
}

func TestGetBucket(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	doRequest(h, "POST", "/buckets", createBucketRequest{Name: "test-bucket"}, token)

	rr := doRequest(h, "GET", "/buckets/test-bucket", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var detail bucketDetail
	json.NewDecoder(rr.Body).Decode(&detail)
	if detail.Name != "test-bucket" {
		t.Errorf("expected test-bucket, got %s", detail.Name)
	}
}

func TestGetBucket_NotFound(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	rr := doRequest(h, "GET", "/buckets/nonexistent", nil, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestDeleteBucket(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	doRequest(h, "POST", "/buckets", createBucketRequest{Name: "test-bucket"}, token)

	rr := doRequest(h, "DELETE", "/buckets/test-bucket", nil, token)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(h, "GET", "/buckets/test-bucket", nil, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", rr.Code)
	}
}

func TestDeleteBucket_NotFound(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	rr := doRequest(h, "DELETE", "/buckets/nonexistent", nil, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

// --- CORS tests ---

func TestCORS_Preflight(t *testing.T) {
	h, _ := newTestAPI(t)
	req := httptest.NewRequest("OPTIONS", "/api/v1/buckets", nil)
	req.Header.Set("Origin", "http://localhost:9000")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	if rr.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("expected CORS methods header")
	}
}

func TestCORS_LocalhostAllowed(t *testing.T) {
	h, _ := newTestAPI(t)
	req := httptest.NewRequest("GET", "/api/v1/auth/oidc/config", nil)
	req.Header.Set("Origin", "http://localhost:9000")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Header().Get("Access-Control-Allow-Origin") != "http://localhost:9000" {
		t.Error("expected localhost origin on same port to be allowed")
	}
}

func TestCORS_ForeignOriginRejected(t *testing.T) {
	h, _ := newTestAPI(t)
	req := httptest.NewRequest("GET", "/api/v1/auth/oidc/config", nil)
	req.Header.Set("Origin", "http://evil.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("expected foreign origin to be rejected")
	}
}

// --- Route tests ---

func TestNotFoundRoute(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	rr := doRequest(h, "GET", "/nonexistent", nil, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestAuthMe(t *testing.T) {
	h, _ := newTestAPI(t)
	token := getToken(t, h)

	rr := doRequest(h, "GET", "/auth/me", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp meResponse
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.User != "admin" {
		t.Errorf("expected admin, got %s", resp.User)
	}
}

// --- Validation tests ---

func TestValidateBucketName(t *testing.T) {
	valid := []string{"my-bucket", "bucket.name", "a1b", "abc123def456"}
	for _, name := range valid {
		if err := validateBucketName(name); err != nil {
			t.Errorf("expected %q to be valid, got: %v", name, err)
		}
	}

	invalid := []string{"", "ab", "-start", "end-", ".dot", "dot.", "has..two", "UPPER", "a\x00b"}
	for _, name := range invalid {
		if err := validateBucketName(name); err == nil {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

func TestValidateObjectKey(t *testing.T) {
	if err := validateObjectKey("valid/key.txt"); err != nil {
		t.Errorf("expected valid key, got: %v", err)
	}
	if err := validateObjectKey(""); err == nil {
		t.Error("expected empty key to be invalid")
	}
	if err := validateObjectKey("key\x00null"); err == nil {
		t.Error("expected null byte key to be invalid")
	}

	longKey := make([]byte, 1025)
	for i := range longKey {
		longKey[i] = 'a'
	}
	if err := validateObjectKey(string(longKey)); err == nil {
		t.Error("expected >1024 char key to be invalid")
	}
}
