package s3

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// fixEnv is a real handler over the real store and filesystem engine, with the
// handles a test needs to set up users and look at what a request left behind.
type fixEnv struct {
	ts      *httptest.Server
	store   *metadata.Store
	h       *Handler
	dataDir string
}

func newFixEnv(t *testing.T) *fixEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	dataDir := filepath.Join(dir, "data")
	fs, err := storage.NewFileSystem(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	auth := NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil)
	h := NewHandler(store, fs, auth, false, "", nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &fixEnv{ts: ts, store: store, h: h, dataDir: dataDir}
}

// as sends a request signed with the given key.
func (e *fixEnv) as(t *testing.T, ak, sk, method, path, body string, headers ...string) result {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	signV4Request(req, ak, sk, []byte(body))
	return send(t, req)
}

// admin sends a request signed with the admin key.
func (e *fixEnv) admin(t *testing.T, method, path, body string, headers ...string) result {
	t.Helper()
	return e.as(t, testAccessKey, testSecretKey, method, path, body, headers...)
}

// addUser creates an IAM user with one policy and an access key for it.
func (e *fixEnv) addUser(t *testing.T, name, policy string, cidrs ...string) (string, string) {
	t.Helper()
	now := time.Now().UTC()
	pname := "pol-" + name
	if err := e.store.CreateIAMPolicy(metadata.IAMPolicy{Name: pname, CreatedAt: now, Document: policy}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateIAMUser(metadata.IAMUser{Name: name, CreatedAt: now, PolicyARNs: []string{pname}, AllowedCIDRs: cidrs}); err != nil {
		t.Fatal(err)
	}
	ak, sk := "AK"+strings.ToUpper(name), "secret-"+name
	if err := e.store.CreateAccessKey(metadata.AccessKey{AccessKey: ak, SecretKey: sk, CreatedAt: now, UserID: name}); err != nil {
		t.Fatal(err)
	}
	return ak, sk
}

// allow is a one-statement Allow policy.
func allow(actions, resources string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[%s],"Resource":[%s]}]}`, actions, resources)
}

var xmlCodeRe = regexp.MustCompile(`<Code>([^<]*)</Code>`)

// errCode is the S3 error code in a response body, or "".
func errCode(body string) string {
	if m := xmlCodeRe.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// expect fails unless the response has this status and, when code is not
// empty, this S3 error code. Asserting the code is what proves the refusal
// happened for the reason the test is about.
func expect(t *testing.T, what string, r result, status int, code string) {
	t.Helper()
	if r.code != status || (code != "" && errCode(r.body) != code) {
		t.Fatalf("%s: got %d %q, want %d %s; body=%s", what, r.code, errCode(r.body), status, code, r.body)
	}
}

func contentMD5(b string) string {
	s := md5.Sum([]byte(b))
	return base64.StdEncoding.EncodeToString(s[:])
}

// startUpload begins a multipart upload and returns its id.
func (e *fixEnv) startUpload(t *testing.T, bucket, key string) string {
	t.Helper()
	r := e.admin(t, http.MethodPost, "/"+bucket+"/"+key+"?uploads", "")
	expect(t, "create multipart upload", r, http.StatusOK, "")
	var res struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal([]byte(r.body), &res); err != nil || res.UploadID == "" {
		t.Fatalf("no upload id in %s", r.body)
	}
	return res.UploadID
}

// listVersionIDs returns the version ids of key, newest first.
func (e *fixEnv) versionIDs(t *testing.T, bucket, key string) []string {
	t.Helper()
	r := e.admin(t, http.MethodGet, "/"+bucket+"?versions&prefix="+key, "")
	expect(t, "list versions", r, http.StatusOK, "")
	var res struct {
		Versions []struct {
			Key       string `xml:"Key"`
			VersionId string `xml:"VersionId"`
		} `xml:"Version"`
	}
	if err := xml.Unmarshal([]byte(r.body), &res); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range res.Versions {
		if v.Key == key {
			out = append(out, v.VersionId)
		}
	}
	return out
}

func md5Bytes(s string) []byte {
	sum := md5.Sum([]byte(s))
	return sum[:]
}
