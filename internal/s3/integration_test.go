package s3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

const (
	testAccessKey = "testaccesskey"
	testSecretKey = "testsecretkey1234567890"
	testRegion    = "us-east-1"
)

// newIntegrationServer creates a real Handler with filesystem storage and BoltDB
// metadata, wrapped in an httptest.Server. Returns the server and a cleanup func.
func newIntegrationServer(t *testing.T) *httptest.Server {
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

	auth := NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil)
	handler := NewHandler(store, engine, auth, false, "", nil)

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// signV4Request signs an http.Request with AWS SigV4 using the test credentials.
func signV4Request(r *http.Request, accessKey, secretKey string, body []byte) {
	now := time.Now().UTC()
	dateStr := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("Host", r.Host)

	// Ensure body is not nil for canonical request building
	if body == nil {
		body = []byte{}
	}
	// Reset body reader so buildCanonicalRequest can read it
	r.Body = io.NopCloser(bytes.NewReader(body))

	// Compute payload hash
	h := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(h[:])
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)

	// Signed headers
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	// Build canonical request
	canonicalRequest := buildCanonicalRequest(r, signedHeaders)

	// Build string to sign
	stringToSign := buildStringToSign(dateStr, testRegion, "s3", canonicalRequest, r)

	// Derive signing key and compute signature
	signingKey := deriveSigningKey(secretKey, dateStr, testRegion, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	credential := fmt.Sprintf("%s/%s/%s/s3/aws4_request", accessKey, dateStr, testRegion)
	r.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s, SignedHeaders=%s, Signature=%s",
		credential, signedHeaders, signature,
	))
}

// doSigned performs a signed HTTP request and returns the response.
func doSigned(t *testing.T, method, url string, body []byte) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	}
	signV4Request(req, testAccessKey, testSecretKey, body)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// doSignedWithHeaders performs a signed HTTP request with extra headers.
func doSignedWithHeaders(t *testing.T, method, url string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	}
	signV4Request(req, testAccessKey, testSecretKey, body)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(b)
}

// --- Auth Tests ---

func TestIntegrationAuthMissing(t *testing.T) {
	ts := newIntegrationServer(t)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestIntegrationAuthWrongCredentials(t *testing.T) {
	ts := newIntegrationServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	signV4Request(req, "wrongkey", "wrongsecret", nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

func TestIntegrationAuthValidListBuckets(t *testing.T) {
	ts := newIntegrationServer(t)

	resp := doSigned(t, http.MethodGet, ts.URL+"/", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body := readBody(t, resp)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

// --- Bucket Tests ---

func TestIntegrationBucketCRUD(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "test-bucket"

	// Create
	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CreateBucket: expected 200, got %d", resp.StatusCode)
	}

	// Head
	resp = doSigned(t, http.MethodHead, ts.URL+"/"+bucket, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HeadBucket: expected 200, got %d", resp.StatusCode)
	}

	// List (should contain our bucket)
	resp = doSigned(t, http.MethodGet, ts.URL+"/", nil)
	body := readBody(t, resp)
	if !strings.Contains(body, bucket) {
		t.Errorf("ListBuckets should contain %q, got: %s", bucket, body)
	}

	// Delete
	resp = doSigned(t, http.MethodDelete, ts.URL+"/"+bucket, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DeleteBucket: expected 204, got %d", resp.StatusCode)
	}
}

func TestIntegrationBucketDuplicate(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "dup-bucket"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate CreateBucket: expected 409, got %d", resp.StatusCode)
	}
}

func TestIntegrationBucketInvalidName(t *testing.T) {
	ts := newIntegrationServer(t)

	resp := doSigned(t, http.MethodPut, ts.URL+"/AB", nil) // too short + uppercase
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid bucket name: expected 400, got %d", resp.StatusCode)
	}
}

func TestIntegrationBucketDeleteNonEmpty(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "nonempty-bucket"

	// Create bucket + put object
	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/file.txt", []byte("hello"))
	resp.Body.Close()

	// Try to delete non-empty bucket
	resp = doSigned(t, http.MethodDelete, ts.URL+"/"+bucket, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete non-empty bucket: expected 409, got %d", resp.StatusCode)
	}
}

// --- Object Tests ---

func TestIntegrationObjectPutGet(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "obj-bucket"
	key := "hello.txt"
	content := []byte("Hello, VaultS3!")

	// Create bucket
	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// Put object
	resp = doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, content, map[string]string{
		"Content-Type": "text/plain",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PutObject: expected 200, got %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Error("PutObject: expected ETag header")
	}

	// Get object
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	body := readBody(t, resp)
	if body != string(content) {
		t.Errorf("GetObject: got %q, want %q", body, content)
	}
	if resp.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("GetObject Content-Type: got %q, want text/plain", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("ETag") != etag {
		t.Errorf("GetObject ETag mismatch: got %q, want %q", resp.Header.Get("ETag"), etag)
	}
}

func TestIntegrationObjectHeadObject(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "head-bucket"
	key := "file.txt"
	content := []byte("head test content")

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, content)
	resp.Body.Close()
	etag := resp.Header.Get("ETag")

	// HEAD
	resp = doSigned(t, http.MethodHead, ts.URL+"/"+bucket+"/"+key, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HeadObject: expected 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("ETag") != etag {
		t.Errorf("HeadObject ETag: got %q, want %q", resp.Header.Get("ETag"), etag)
	}
	if resp.Header.Get("Content-Length") == "" {
		t.Error("HeadObject: missing Content-Length")
	}
}

func TestIntegrationObjectDelete(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "del-bucket"
	key := "file.txt"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("data"))
	resp.Body.Close()

	// Delete
	resp = doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/"+key, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DeleteObject: expected 204, got %d", resp.StatusCode)
	}

	// Verify 404
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GetObject after delete: expected 404, got %d", resp.StatusCode)
	}
}

func TestIntegrationObjectNotFound(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "nf-bucket"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/nonexistent", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestIntegrationObjectLarge(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "large-bucket"
	key := "large.bin"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// 1MB object
	content := make([]byte, 1<<20)
	for i := range content {
		content[i] = byte(i % 256)
	}

	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, content)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PutObject large: expected 200, got %d", resp.StatusCode)
	}

	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content) {
		t.Errorf("large object mismatch: got %d bytes, want %d bytes", len(got), len(content))
	}
}

func TestIntegrationObjectSpecialChars(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "special-bucket"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	keys := []string{
		"dir/subdir/file.txt",
		"file with spaces.txt",
		"special-chars_test.2024.log",
	}

	for _, key := range keys {
		content := []byte("content for " + key)
		resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, content)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("PutObject(%q): expected 200, got %d", key, resp.StatusCode)
			continue
		}

		resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
		body := readBody(t, resp)
		if body != string(content) {
			t.Errorf("GetObject(%q): got %q, want %q", key, body, content)
		}
	}
}

// --- User Metadata Tests ---

func TestIntegrationUserMetadata(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "meta-bucket"
	key := "meta.txt"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// Put with user metadata
	resp = doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("metadata test"), map[string]string{
		"X-Amz-Meta-Author": "test-user",
		"X-Amz-Meta-Custom": "value123",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PutObject with metadata: expected 200, got %d", resp.StatusCode)
	}

	// Get and verify metadata
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	resp.Body.Close()
	if resp.Header.Get("X-Amz-Meta-Author") != "test-user" {
		t.Errorf("X-Amz-Meta-Author: got %q, want %q", resp.Header.Get("X-Amz-Meta-Author"), "test-user")
	}
	if resp.Header.Get("X-Amz-Meta-Custom") != "value123" {
		t.Errorf("X-Amz-Meta-Custom: got %q, want %q", resp.Header.Get("X-Amz-Meta-Custom"), "value123")
	}
}

// --- Copy Object Tests ---

func TestIntegrationCopyObject(t *testing.T) {
	ts := newIntegrationServer(t)
	srcBucket := "copy-src"
	dstBucket := "copy-dst"
	srcKey := "original.txt"
	dstKey := "copied.txt"
	content := []byte("copy me!")

	// Create buckets
	resp := doSigned(t, http.MethodPut, ts.URL+"/"+srcBucket, nil)
	resp.Body.Close()
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+dstBucket, nil)
	resp.Body.Close()

	// Put source
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+srcBucket+"/"+srcKey, content)
	resp.Body.Close()

	// Copy across buckets
	resp = doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+dstBucket+"/"+dstKey, nil, map[string]string{
		"X-Amz-Copy-Source": "/" + srcBucket + "/" + srcKey,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CopyObject: expected 200, got %d", resp.StatusCode)
	}

	// Get copied object
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+dstBucket+"/"+dstKey, nil)
	body := readBody(t, resp)
	if body != string(content) {
		t.Errorf("CopyObject content: got %q, want %q", body, content)
	}
}

func TestIntegrationCopyObjectSameBucket(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "copy-same"
	content := []byte("same bucket copy")

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/src.txt", content)
	resp.Body.Close()

	// Copy within same bucket
	resp = doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/dst.txt", nil, map[string]string{
		"X-Amz-Copy-Source": "/" + bucket + "/src.txt",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CopyObject same bucket: expected 200, got %d", resp.StatusCode)
	}

	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/dst.txt", nil)
	body := readBody(t, resp)
	if body != string(content) {
		t.Errorf("got %q, want %q", body, content)
	}
}

// --- Conditional Request Tests ---

func TestIntegrationConditionalIfNoneMatch(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "cond-bucket"
	key := "cond.txt"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("conditional"))
	resp.Body.Close()
	etag := resp.Header.Get("ETag")

	// GET with matching ETag should return 304
	resp = doSignedWithHeaders(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil, map[string]string{
		"If-None-Match": etag,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: expected 304, got %d", resp.StatusCode)
	}
}

func TestIntegrationConditionalIfMatch(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "ifmatch-bucket"
	key := "file.txt"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("data"))
	resp.Body.Close()

	// GET with non-matching ETag should return 412
	resp = doSignedWithHeaders(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil, map[string]string{
		"If-Match": "\"wrongetag\"",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("If-Match mismatch: expected 412, got %d", resp.StatusCode)
	}
}

// --- Multipart Upload Tests ---

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

func TestIntegrationMultipartUpload(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "mp-bucket"
	key := "multipart.bin"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// Initiate multipart upload
	resp = doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"/"+key+"?uploads", nil)
	if resp.StatusCode != http.StatusOK {
		body := readBody(t, resp)
		t.Fatalf("InitiateMultipartUpload: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var initResult initiateResult
	if err := xml.NewDecoder(resp.Body).Decode(&initResult); err != nil {
		t.Fatalf("decode initiate result: %v", err)
	}
	resp.Body.Close()
	uploadID := initResult.UploadID
	if uploadID == "" {
		t.Fatal("empty uploadId")
	}

	// Upload 2 parts (5MB minimum for real S3, but VaultS3 doesn't enforce this)
	part1 := []byte("PART1DATA" + strings.Repeat("x", 1000))
	part2 := []byte("PART2DATA" + strings.Repeat("y", 1000))

	resp = doSigned(t, http.MethodPut,
		fmt.Sprintf("%s/%s/%s?uploadId=%s&partNumber=1", ts.URL, bucket, key, uploadID), part1)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("UploadPart 1: expected 200, got %d", resp.StatusCode)
	}
	etag1 := resp.Header.Get("ETag")

	resp = doSigned(t, http.MethodPut,
		fmt.Sprintf("%s/%s/%s?uploadId=%s&partNumber=2", ts.URL, bucket, key, uploadID), part2)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("UploadPart 2: expected 200, got %d", resp.StatusCode)
	}
	etag2 := resp.Header.Get("ETag")

	// Complete multipart upload
	completeXML := fmt.Sprintf(`<CompleteMultipartUpload>
		<Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part>
		<Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part>
	</CompleteMultipartUpload>`, etag1, etag2)

	resp = doSigned(t, http.MethodPost,
		fmt.Sprintf("%s/%s/%s?uploadId=%s", ts.URL, bucket, key, uploadID),
		[]byte(completeXML))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CompleteMultipartUpload: expected 200, got %d", resp.StatusCode)
	}

	// Verify the assembled object
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	body := readBody(t, resp)
	expected := string(part1) + string(part2)
	if body != expected {
		t.Errorf("multipart object mismatch: got %d bytes, want %d bytes", len(body), len(expected))
	}
}

func TestIntegrationMultipartAbort(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "mp-abort-bucket"
	key := "abort.bin"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// Initiate
	resp = doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"/"+key+"?uploads", nil)
	var initResult initiateResult
	xml.NewDecoder(resp.Body).Decode(&initResult)
	resp.Body.Close()
	uploadID := initResult.UploadID

	// Abort
	resp = doSigned(t, http.MethodDelete,
		fmt.Sprintf("%s/%s/%s?uploadId=%s", ts.URL, bucket, key, uploadID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("AbortMultipartUpload: expected 204, got %d", resp.StatusCode)
	}
}

// --- Tagging Tests ---

func TestIntegrationObjectTagging(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "tag-bucket"
	key := "tagged.txt"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("tagged"))
	resp.Body.Close()

	// Put tagging
	taggingXML := `<Tagging><TagSet><Tag><Key>env</Key><Value>test</Value></Tag><Tag><Key>team</Key><Value>dev</Value></Tag></TagSet></Tagging>`
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key+"?tagging", []byte(taggingXML))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PutObjectTagging: expected 200, got %d", resp.StatusCode)
	}

	// Get tagging
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key+"?tagging", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GetObjectTagging: expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "env") || !strings.Contains(body, "test") {
		t.Errorf("GetObjectTagging: expected tags in response, got: %s", body)
	}

	// Delete tagging
	resp = doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/"+key+"?tagging", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DeleteObjectTagging: expected 204, got %d", resp.StatusCode)
	}
}

// --- Versioning Tests ---

func TestIntegrationVersioning(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "ver-bucket"
	key := "versioned.txt"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// Enable versioning
	versioningXML := `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"?versioning", []byte(versioningXML))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PutBucketVersioning: expected 200, got %d", resp.StatusCode)
	}

	// Verify versioning is enabled
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"?versioning", nil)
	body := readBody(t, resp)
	if !strings.Contains(body, "Enabled") {
		t.Errorf("GetBucketVersioning: expected Enabled, got: %s", body)
	}

	// Put v1
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("version1"))
	resp.Body.Close()
	v1 := resp.Header.Get("X-Amz-Version-Id")
	if v1 == "" {
		t.Error("expected version ID for v1")
	}

	// Put v2
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("version2"))
	resp.Body.Close()
	v2 := resp.Header.Get("X-Amz-Version-Id")
	if v2 == "" {
		t.Error("expected version ID for v2")
	}

	if v1 == v2 {
		t.Errorf("version IDs should differ: v1=%s, v2=%s", v1, v2)
	}

	// GET latest should return v2
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	body = readBody(t, resp)
	if body != "version2" {
		t.Errorf("latest version: got %q, want %q", body, "version2")
	}

	// GET specific version v1
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key+"?versionId="+v1, nil)
	body = readBody(t, resp)
	if body != "version1" {
		t.Errorf("version1: got %q, want %q", body, "version1")
	}
}

// --- Batch Delete Tests ---

func TestIntegrationBatchDelete(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "batch-bucket"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// Create some objects
	for _, key := range []string{"a.txt", "b.txt", "c.txt"} {
		resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte(key))
		resp.Body.Close()
	}

	// Batch delete a.txt and b.txt
	deleteXML := `<Delete><Object><Key>a.txt</Key></Object><Object><Key>b.txt</Key></Object></Delete>`
	resp = doSignedWithHeaders(t, http.MethodPost, ts.URL+"/"+bucket+"?delete", []byte(deleteXML), map[string]string{
		"Content-Type": "application/xml",
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("BatchDelete: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// a.txt should be gone
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/a.txt", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a.txt after batch delete: expected 404, got %d", resp.StatusCode)
	}

	// c.txt should still exist
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/c.txt", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("c.txt should still exist: expected 200, got %d", resp.StatusCode)
	}
}

// --- List Objects Tests ---

func TestIntegrationListObjects(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "list-bucket"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// Put some objects
	for _, key := range []string{"a.txt", "b.txt", "dir/c.txt"} {
		resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte(key))
		resp.Body.Close()
	}

	// ListObjectsV2
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"?list-type=2", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ListObjectsV2: expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "a.txt") || !strings.Contains(body, "b.txt") {
		t.Errorf("ListObjectsV2 should contain a.txt and b.txt: %s", body)
	}

	// ListObjectsV2 with prefix
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"?list-type=2&prefix=dir/", nil)
	body = readBody(t, resp)
	if !strings.Contains(body, "dir/c.txt") {
		t.Errorf("ListObjectsV2 with prefix: expected dir/c.txt, got: %s", body)
	}
	if strings.Contains(body, "a.txt") {
		t.Errorf("ListObjectsV2 with prefix: should not contain a.txt")
	}
}

// --- Path Traversal Security Tests ---

func TestIntegrationPathTraversal(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "sec-bucket"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// Key with .. should be rejected
	resp = doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/../etc/passwd", []byte("hack"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("path traversal in key: expected 400, got %d", resp.StatusCode)
	}
}

// --- Inline Tagging on PUT ---

func TestIntegrationInlineTagging(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "inline-tag-bucket"
	key := "tagged-inline.txt"

	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil)
	resp.Body.Close()

	// Put with x-amz-tagging header
	resp = doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("tagged inline"), map[string]string{
		"X-Amz-Tagging": "env=prod&team=backend",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PutObject with tagging: expected 200, got %d", resp.StatusCode)
	}

	// Get tagging
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key+"?tagging", nil)
	body := readBody(t, resp)
	if !strings.Contains(body, "env") || !strings.Contains(body, "prod") {
		t.Errorf("inline tags not saved: %s", body)
	}
}
