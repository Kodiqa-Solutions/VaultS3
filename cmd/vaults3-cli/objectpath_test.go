package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// The object commands pasted the key into the URL as it was, so the first '?'
// ended the path and a '#' began a fragment: "object rm b 'a?b'" deleted the
// object "a" and printed "Deleted b/a?b" (the #62 path bug, in its S3 half).
// These tests run the CLI against the real S3 handler, with real signature
// checks, and always keep a bystander object "a" next to the target "a?b".

// s3CLIServer starts the real S3 handler and points the CLI at it.
func s3CLIServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	engine, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}
	auth := s3.NewAuthenticator("admin", "secret-for-tests", store, nil, nil)
	srv := httptest.NewServer(s3.NewHandler(store, engine, auth, false, "", nil))
	t.Cleanup(srv.Close)

	prev := [3]string{endpoint, accessKey, secretKey}
	endpoint, accessKey, secretKey = srv.URL, "admin", "secret-for-tests"
	t.Cleanup(func() { endpoint, accessKey, secretKey = prev[0], prev[1], prev[2] })

	if _, errOut, failed := runCLI(t, "bucket", "create", "bkt"); failed {
		t.Fatalf("bucket create: %s", errOut)
	}
	return srv.URL
}

// rawObject sends one signed request for a key, escaped independently of the
// code under test, and returns the status and body.
func rawObject(t *testing.T, method, bucket, key, body string) (int, string) {
	t.Helper()
	u := strings.TrimRight(endpoint, "/") + "/" + bucket + "/" + uriEncodePath(key)
	var rdr io.Reader
	if method == "PUT" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		t.Fatal(err)
	}
	signV4(req, accessKey, secretKey, region)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func seed(t *testing.T, key, body string) {
	t.Helper()
	if code, out := rawObject(t, "PUT", "bkt", key, body); code != 200 {
		t.Fatalf("seed %q: HTTP %d %s", key, code, out)
	}
}

func wantObject(t *testing.T, key, body string) {
	t.Helper()
	code, got := rawObject(t, "GET", "bkt", key, "")
	if code != 200 || got != body {
		t.Errorf("object %q: HTTP %d %q, want %q", key, code, got, body)
	}
}

func wantNoObject(t *testing.T, key string) {
	t.Helper()
	if code, _ := rawObject(t, "GET", "bkt", key, ""); code != 404 {
		t.Errorf("object %q: HTTP %d, want 404", key, code)
	}
}

// keysWithReservedCharacters each break a URL built by string formatting in a
// different way: '?' starts a query, '#' a fragment, '%41' decodes to 'A', and a
// space or '+' is changed by a sloppy encoder.
var keysWithReservedCharacters = []string{"a?b", "a#b", "a%41b", "dir/my file+1.txt", "x&y=z", "ünï"}

func TestObjectRmDeletesTheKeyNamedNotABystander(t *testing.T) {
	s3CLIServer(t)
	seed(t, "a", "bystander")
	seed(t, "a?b", "target")

	out, errOut, failed := runCLI(t, "object", "rm", "bkt", "a?b")
	if failed {
		t.Fatalf("rm failed: %s", errOut)
	}
	if !strings.Contains(out, "Deleted bkt/a?b") {
		t.Errorf("unexpected output %q", out)
	}
	wantNoObject(t, "a?b")
	wantObject(t, "a", "bystander")
}

func TestObjectPutAndGetUseTheKeyNamed(t *testing.T) {
	s3CLIServer(t)
	dir := t.TempDir()
	for _, key := range keysWithReservedCharacters {
		t.Run(key, func(t *testing.T) {
			seed(t, "a", "bystander")
			src := filepath.Join(dir, "src")
			if err := os.WriteFile(src, []byte("body of "+key), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, errOut, failed := runCLI(t, "object", "put", "bkt", key, src); failed {
				t.Fatalf("put failed: %s", errOut)
			}
			wantObject(t, key, "body of "+key)
			wantObject(t, "a", "bystander")

			dst := filepath.Join(dir, "dst")
			os.Remove(dst)
			if _, errOut, failed := runCLI(t, "object", "get", "bkt", key, dst); failed {
				t.Fatalf("get failed: %s", errOut)
			}
			if got, _ := os.ReadFile(dst); string(got) != "body of "+key {
				t.Errorf("get wrote %q", got)
			}
		})
	}
}

func TestObjectCopyEncodesSourceAndDestination(t *testing.T) {
	s3CLIServer(t)
	seed(t, "a", "bystander")
	seed(t, "a?b", "source")
	seed(t, "cAd", "other bystander")

	if _, errOut, failed := runCLI(t, "object", "cp", "bkt/a?b", "bkt/c%41d"); failed {
		t.Fatalf("cp failed: %s", errOut)
	}
	wantObject(t, "c%41d", "source")
	wantObject(t, "cAd", "other bystander")
	wantObject(t, "a", "bystander")
}

func TestObjectLsListsUnderAnEscapedPrefix(t *testing.T) {
	s3CLIServer(t)
	seed(t, "a", "bystander")
	seed(t, "a?b", "target")

	out, errOut, failed := runCLI(t, "object", "ls", "bkt", "--prefix=a?")
	if failed {
		t.Fatalf("ls failed: %s", errOut)
	}
	if !strings.Contains(out, "a?b") || !strings.Contains(out, "\n1 object(s)") {
		t.Errorf("ls --prefix=a? = %q, want only a?b", out)
	}
}

// A bucket name was pasted into the path too, so "bucket delete 'a?b'" deleted
// the bucket "abc" when given "abc?x".
func TestBucketDeleteDoesNotActOnABystanderBucket(t *testing.T) {
	s3CLIServer(t)
	if _, errOut, failed := runCLI(t, "bucket", "create", "abc"); failed {
		t.Fatalf("create abc: %s", errOut)
	}
	runCLI(t, "bucket", "delete", "abc?x")
	req, _ := http.NewRequest("HEAD", endpoint+"/abc", nil)
	signV4(req, accessKey, secretKey, region)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("bucket abc answers HTTP %d after deleting 'abc?x', want 200", resp.StatusCode)
	}
}

// A presigned URL is only worth anything if the server accepts it. The URL
// used to carry the key unescaped and sign it unescaped, so any key with a
// space or reserved character produced a URL that failed verification or
// fetched a different object.
func TestObjectPresignVerifiesForReservedCharacters(t *testing.T) {
	s3CLIServer(t)
	seed(t, "a", "bystander")
	for _, key := range keysWithReservedCharacters {
		seed(t, key, "presigned "+key)
		out, errOut, failed := runCLI(t, "object", "presign", "bkt", key, "--expires=60")
		if failed {
			t.Fatalf("%q: presign failed: %s", key, errOut)
		}
		resp, err := http.Get(strings.TrimSpace(out))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "presigned "+key {
			t.Errorf("%q: presigned GET = HTTP %d %q", key, resp.StatusCode, body)
		}
	}
}

func TestObjectPresignBoundsExpiry(t *testing.T) {
	s3CLIServer(t)
	for _, bad := range []string{"0", "-5", "604801", "abc", ""} {
		_, errOut, failed := runCLI(t, "object", "presign", "bkt", "k", "--expires="+bad)
		if !failed || !strings.Contains(errOut, "604800") {
			t.Errorf("--expires=%s: want a refusal naming the bound, got failed=%v %q", bad, failed, errOut)
		}
	}
	for _, good := range []string{"1", "604800"} {
		out, errOut, failed := runCLI(t, "object", "presign", "bkt", "k", "--expires="+good)
		if failed || !strings.Contains(out, "X-Amz-Expires="+good) {
			t.Errorf("--expires=%s: failed=%v %q %q", good, failed, out, errOut)
		}
	}
}

// object put held the whole file in memory three times over. It now streams
// the file with its length declared and the payload signed as
// UNSIGNED-PAYLOAD, so nothing reads the body before it goes on the wire.
func TestObjectPutStreamsWithADeclaredLength(t *testing.T) {
	var mu sync.Mutex
	var gotHash, gotTE string
	var gotLen int64
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotHash = r.Header.Get("X-Amz-Content-Sha256")
		gotTE = strings.Join(r.TransferEncoding, ",")
		gotLen = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	prev := [3]string{endpoint, accessKey, secretKey}
	endpoint, accessKey, secretKey = srv.URL, "admin", "secret"
	defer func() { endpoint, accessKey, secretKey = prev[0], prev[1], prev[2] }()

	src := filepath.Join(t.TempDir(), "big")
	payload := strings.Repeat("0123456789", 100000)
	if err := os.WriteFile(src, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, failed := runCLI(t, "object", "put", "bkt", "k", src); failed {
		t.Fatalf("put failed: %s", errOut)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotHash != unsignedPayload {
		t.Errorf("payload hash %q, want %s", gotHash, unsignedPayload)
	}
	if gotLen != int64(len(payload)) || gotTE != "" {
		t.Errorf("Content-Length %d, Transfer-Encoding %q, want %d and none", gotLen, gotTE, len(payload))
	}
	if string(gotBody) != payload {
		t.Error("the body that arrived is not the file")
	}
}

// object get replaced an existing local file without asking.
func TestObjectGetRefusesToOverwriteWithoutForce(t *testing.T) {
	s3CLIServer(t)
	seed(t, "k", "new contents")
	dst := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(dst, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, errOut, failed := runCLI(t, "object", "get", "bkt", "k", dst)
	if !failed || !strings.Contains(errOut, "--force") {
		t.Errorf("want a refusal naming --force, got failed=%v %q", failed, errOut)
	}
	if got, _ := os.ReadFile(dst); string(got) != "precious" {
		t.Errorf("refused get still changed the file: %q", got)
	}

	if _, errOut, failed := runCLI(t, "object", "get", "bkt", "k", dst, "--force"); failed {
		t.Fatalf("get --force failed: %s", errOut)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new contents" {
		t.Errorf("get --force wrote %q", got)
	}

	out, errOut, failed := runCLI(t, "object", "get", "bkt", "k", "-")
	if failed || out != "new contents" {
		t.Errorf("get to stdout: failed=%v out=%q err=%q", failed, out, errOut)
	}

	// A failed download leaves no partial file behind under any name.
	fresh := filepath.Join(t.TempDir(), "fresh.txt")
	if _, _, failed := runCLI(t, "object", "get", "bkt", "missing", fresh); !failed {
		t.Error("get of a missing key succeeded")
	}
	if entries, _ := os.ReadDir(filepath.Dir(fresh)); len(entries) != 0 {
		t.Errorf("a failed get left %d file(s) behind", len(entries))
	}
}
