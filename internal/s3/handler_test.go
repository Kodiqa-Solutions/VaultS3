package s3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// mockEngine implements storage.Engine for handler testing.
type mockEngine struct {
	dataDir string
}

func (m *mockEngine) CreateBucketDir(string) error             { return nil }
func (m *mockEngine) DeleteBucketDir(string) error             { return nil }
func (m *mockEngine) ObjectExists(string, string) bool         { return false }
func (m *mockEngine) ObjectSize(string, string) (int64, error) { return 0, nil }
func (m *mockEngine) DeleteObject(string, string) error        { return nil }
func (m *mockEngine) BucketSize(string) (int64, int64, error)  { return 0, 0, nil }
func (m *mockEngine) DataDir() string                          { return m.dataDir }
func (m *mockEngine) ObjectPath(bucket, key string) string {
	return filepath.Join(m.dataDir, bucket, key)
}
func (m *mockEngine) GetObject(string, string) (storage.ReadSeekCloser, int64, error) {
	return nil, 0, nil
}
func (m *mockEngine) PutObject(string, string, io.Reader, int64) (int64, string, error) {
	return 0, "", nil
}
func (m *mockEngine) ListObjects(string, string, string, int) ([]storage.ObjectInfo, bool, error) {
	return nil, false, nil
}
func (m *mockEngine) PutObjectVersion(string, string, string, io.Reader, int64) (int64, string, error) {
	return 0, "", nil
}
func (m *mockEngine) GetObjectVersion(string, string, string) (storage.ReadSeekCloser, int64, error) {
	return nil, 0, nil
}
func (m *mockEngine) DeleteObjectVersion(string, string, string) error { return nil }

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	engine := &mockEngine{dataDir: filepath.Join(dir, "data")}
	auth := NewAuthenticator("testkey", "testsecret", store, nil, nil)
	return NewHandler(store, engine, auth, false, "", nil)
}

// signRequest adds a minimal AWS4 Authorization header so requests pass auth.
func signRequest(r *http.Request) {
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=testkey/20260224/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=fakesig")
	r.Header.Set("X-Amz-Date", "20260224T000000Z")
}

func TestParsePath(t *testing.T) {
	tests := []struct {
		path       string
		wantBucket string
		wantKey    string
	}{
		{"", "", ""},
		{"mybucket", "mybucket", ""},
		{"mybucket/file.txt", "mybucket", "file.txt"},
		{"mybucket/dir/file.txt", "mybucket", "dir/file.txt"},
	}
	for _, tt := range tests {
		bucket, key := parsePath(tt.path)
		if bucket != tt.wantBucket || key != tt.wantKey {
			t.Errorf("parsePath(%q): got (%q, %q), want (%q, %q)", tt.path, bucket, key, tt.wantBucket, tt.wantKey)
		}
	}
}

func TestParseRequest_PathStyle(t *testing.T) {
	h := &Handler{domain: ""}
	bucket, key := h.parseRequest("localhost:9000", "mybucket/file.txt")
	if bucket != "mybucket" || key != "file.txt" {
		t.Errorf("got (%q, %q), want (mybucket, file.txt)", bucket, key)
	}
}

func TestParseRequest_VirtualHosted(t *testing.T) {
	h := &Handler{domain: "s3.example.com"}
	bucket, key := h.parseRequest("mybucket.s3.example.com:9000", "file.txt")
	if bucket != "mybucket" || key != "file.txt" {
		t.Errorf("got (%q, %q), want (mybucket, file.txt)", bucket, key)
	}
}

func TestParseRequest_VirtualHostedNoDomain(t *testing.T) {
	h := &Handler{domain: ""}
	// Without domain configured, should fall back to path style
	bucket, key := h.parseRequest("mybucket.s3.example.com:9000", "file.txt")
	if bucket != "file.txt" {
		t.Errorf("expected path-style fallback, got bucket=%q", bucket)
	}
	if key != "" {
		t.Errorf("expected empty key, got %q", key)
	}
}

func TestMapMethodToAction(t *testing.T) {
	tests := []struct {
		method string
		bucket string
		key    string
		query  map[string][]string
		want   string
	}{
		{http.MethodGet, "b", "k", nil, "s3:GetObject"},
		{http.MethodHead, "b", "k", nil, "s3:GetObject"},
		{http.MethodPut, "b", "k", nil, "s3:PutObject"},
		{http.MethodDelete, "b", "k", nil, "s3:DeleteObject"},
		{http.MethodPut, "b", "", nil, "s3:CreateBucket"},
		{http.MethodDelete, "b", "", nil, "s3:DeleteBucket"},
		{http.MethodGet, "b", "", nil, "s3:ListBucket"},
		{http.MethodGet, "", "", nil, "s3:ListAllMyBuckets"},
		{http.MethodPut, "b", "", map[string][]string{"policy": {""}}, "s3:PutBucketPolicy"},
		{http.MethodGet, "b", "", map[string][]string{"policy": {""}}, "s3:GetBucketPolicy"},
	}
	for _, tt := range tests {
		got := mapMethodToAction(tt.method, tt.bucket, tt.key, tt.query)
		if got != tt.want {
			t.Errorf("mapMethodToAction(%s, %q, %q): got %q, want %q", tt.method, tt.bucket, tt.key, got, tt.want)
		}
	}
}

func TestFormatResource(t *testing.T) {
	tests := []struct {
		bucket, key, want string
	}{
		{"", "", "*"},
		{"mybucket", "", "arn:aws:s3:::mybucket"},
		{"mybucket", "file.txt", "arn:aws:s3:::mybucket/file.txt"},
	}
	for _, tt := range tests {
		got := formatResource(tt.bucket, tt.key)
		if got != tt.want {
			t.Errorf("formatResource(%q, %q): got %q, want %q", tt.bucket, tt.key, got, tt.want)
		}
	}
}

func TestExtractAccessKeyFromAuth(t *testing.T) {
	tests := []struct {
		name string
		auth string
		want string
	}{
		{"empty", "", ""},
		{"aws4", "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20230101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc", "AKIAEXAMPLE"},
		{"no credential", "Bearer token123", ""},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tt.auth != "" {
			r.Header.Set("Authorization", tt.auth)
		}
		got := extractAccessKeyFromAuth(r)
		if got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestExtractAccessKeyFromQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?X-Amz-Credential=AKIAQUERY/20230101/us-east-1/s3/aws4_request", nil)
	got := extractAccessKeyFromAuth(r)
	if got != "AKIAQUERY" {
		t.Errorf("got %q, want AKIAQUERY", got)
	}
}

func TestHandler_UnauthenticatedReturns403(t *testing.T) {
	h := newTestHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/newbucket", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "AccessDenied") {
		t.Errorf("expected AccessDenied in body, got %s", rr.Body.String())
	}
}

func TestMatchOrigin(t *testing.T) {
	if !matchOrigin([]string{"*"}, "http://example.com") {
		t.Error("wildcard should match any origin")
	}
	if !matchOrigin([]string{"http://example.com"}, "http://example.com") {
		t.Error("exact origin should match")
	}
	if matchOrigin([]string{"http://other.com"}, "http://example.com") {
		t.Error("different origin should not match")
	}
	if matchOrigin(nil, "http://example.com") {
		t.Error("empty list should not match")
	}
}

func TestMatchMethod(t *testing.T) {
	if !matchMethod([]string{"GET", "PUT"}, "get") {
		t.Error("case-insensitive match should work")
	}
	if matchMethod([]string{"GET"}, "POST") {
		t.Error("non-matching method should not match")
	}
}

func TestStatusWriter(t *testing.T) {
	rr := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rr, status: http.StatusOK}

	sw.WriteHeader(http.StatusNotFound)
	if sw.status != http.StatusNotFound {
		t.Errorf("expected 404, got %d", sw.status)
	}
	if rr.Code != http.StatusNotFound {
		t.Errorf("underlying writer should also get 404, got %d", rr.Code)
	}
}
