package s3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketkeys"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// fullServer is an S3 server over the real store and the per-bucket encryption
// engine, the stack a per-bucket-mode deployment runs. The store is returned so
// a test can look at what a request left behind.
func fullServer(t *testing.T) (*httptest.Server, *metadata.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fs, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	pe, err := storage.NewPerBucketEngine(fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	km, err := bucketkeys.NewManager(store, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	pe.SetManager(km)
	auth := NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil)
	h := NewHandler(store, pe, auth, false, "", nil)
	h.SetKeyManager(km)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, store
}

// result is a response's status and full body. A body cut short is reported in
// the text, so a test sees it.
type result struct {
	code int
	body string
}

// must fails the test unless the request succeeded.
func (r result) must(t *testing.T, what string) result {
	t.Helper()
	if r.code < 200 || r.code > 299 {
		t.Fatalf("%s: %d %s", what, r.code, r.body)
	}
	return r
}

// call sends a signed request.
func call(t *testing.T, ts *httptest.Server, method, path, body string, headers ...string) result {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	signV4Request(req, testAccessKey, testSecretKey, []byte(body))
	return send(t, req)
}

// anonCall sends an unsigned request.
func anonCall(t *testing.T, ts *httptest.Server, method, path string) result {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return send(t, req)
}

func send(t *testing.T, req *http.Request) result {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return result{resp.StatusCode, string(b) + " [body cut short: " + err.Error() + "]"}
	}
	return result{resp.StatusCode, string(b)}
}

const versioningOn = `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`
