package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// stubS3 mimics an S3 source: ListBuckets, paginated ListObjectsV2, and GetObject.
// Objects are an in-memory map keyed by "bucket/key".
func stubS3(t *testing.T, objects map[string][]byte) string {
	t.Helper()

	// Group keys by bucket.
	byBucket := map[string][]string{}
	for path := range objects {
		parts := strings.SplitN(path, "/", 2)
		byBucket[parts[0]] = append(byBucket[parts[0]], parts[1])
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")

		// ListBuckets: GET /
		if r.URL.Path == "/" {
			var b strings.Builder
			b.WriteString(`<ListAllMyBucketsResult><Buckets>`)
			for bucket := range byBucket {
				fmt.Fprintf(&b, `<Bucket><Name>%s</Name></Bucket>`, bucket)
			}
			b.WriteString(`</Buckets></ListAllMyBucketsResult>`)
			io.WriteString(w, b.String())
			return
		}

		trimmed := strings.TrimPrefix(r.URL.Path, "/")

		// ListObjectsV2: GET /{bucket}?list-type=2  (one key per page to exercise paging)
		if r.URL.Query().Get("list-type") == "2" {
			bucket := trimmed
			keys := byBucket[bucket]
			start := 0
			if tok := r.URL.Query().Get("continuation-token"); tok != "" {
				fmt.Sscanf(tok, "%d", &start)
			}
			var b strings.Builder
			b.WriteString(`<ListBucketResult>`)
			if start < len(keys) {
				k := keys[start]
				fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size><ETag>"x"</ETag></Contents>`, k, len(objects[bucket+"/"+k]))
			}
			if start+1 < len(keys) {
				fmt.Fprintf(&b, `<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>`, start+1)
			} else {
				b.WriteString(`<IsTruncated>false</IsTruncated>`)
			}
			b.WriteString(`</ListBucketResult>`)
			io.WriteString(w, b.String())
			return
		}

		// GetObject: GET /{bucket}/{key}
		if data, ok := objects[trimmed]; ok {
			w.Header().Set("Content-Type", "text/plain")
			w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newLocal(t *testing.T) (*metadata.Store, storage.Engine) {
	t.Helper()
	base := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(base, "meta.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	eng, err := storage.NewFileSystem(filepath.Join(base, "data"))
	if err != nil {
		t.Fatalf("fs: %v", err)
	}
	return store, eng
}

func waitDone(t *testing.T, m *Manager, id string) *Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j := m.GetJob(id)
		if j != nil && j.Status != "running" {
			return j
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("migration did not finish in time")
	return nil
}

func TestMigrateCopiesAllObjects(t *testing.T) {
	objects := map[string][]byte{
		"docs/a.txt":     []byte("alpha"),
		"docs/sub/b.txt": []byte("bravo"),
		"media/c.txt":    []byte("charlie"),
	}
	endpoint := stubS3(t, objects)
	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback

	id, err := m.Start(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)

	if job.Status != "completed" {
		t.Fatalf("status=%s err=%s", job.Status, job.Error)
	}
	if job.Copied != 3 || job.Failed != 0 {
		t.Fatalf("copied=%d failed=%d, want 3/0", job.Copied, job.Failed)
	}

	// Every object must exist locally with identical bytes.
	for path, want := range objects {
		parts := strings.SplitN(path, "/", 2)
		bucket, key := parts[0], parts[1]
		if !store.BucketExists(bucket) {
			t.Fatalf("bucket %s not created locally", bucket)
		}
		rc, _, err := eng.GetObject(bucket, key)
		if err != nil {
			t.Fatalf("local GetObject %s/%s: %v", bucket, key, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != string(want) {
			t.Fatalf("object %s mismatch: got %q want %q", path, got, want)
		}
	}
}

func TestMigrateSelectedBucketOnly(t *testing.T) {
	objects := map[string][]byte{
		"keep/a.txt": []byte("a"),
		"skip/b.txt": []byte("b"),
	}
	endpoint := stubS3(t, objects)
	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback

	id, err := m.Start(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s", Buckets: []string{"keep"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)

	if job.Copied != 1 {
		t.Fatalf("copied=%d, want 1 (selected bucket only)", job.Copied)
	}
	if !eng.ObjectExists("keep", "a.txt") {
		t.Fatal("selected bucket object missing")
	}
	if store.BucketExists("skip") {
		t.Fatal("non-selected bucket should not have been created")
	}
}

func TestMigrateTestConnection(t *testing.T) {
	endpoint := stubS3(t, map[string][]byte{"b1/x": []byte("1"), "b2/y": []byte("2")})
	m := NewManager(nil, nil)
	m.AllowPrivateSources(true) // httptest listens on loopback // TestConnection doesn't touch store/engine
	buckets, err := m.TestConnection(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2: %v", len(buckets), buckets)
	}
}

// TestMigrateRetriesTransient: a transient 503 on the first GET is retried and
// the object still copies successfully (issue #6).
func TestMigrateRetriesTransient(t *testing.T) {
	var mu sync.Mutex
	attempts := map[string]int{}
	data := []byte("payload that survives a flaky first fetch")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Path == "/" {
			io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
			return
		}
		if r.URL.Query().Get("list-type") == "2" {
			fmt.Fprintf(w, `<ListBucketResult><Contents><Key>flaky.txt</Key><Size>%d</Size></Contents><IsTruncated>false</IsTruncated></ListBucketResult>`, len(data))
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/b/")
		mu.Lock()
		attempts[key]++
		n := attempts[key]
		mu.Unlock()
		if n == 1 {
			http.Error(w, "slow down", http.StatusServiceUnavailable) // transient
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write(data)
	}))
	defer srv.Close()

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback
	id, err := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)

	if job.Status != "completed" || job.Copied != 1 || job.Failed != 0 {
		t.Fatalf("after retry: status=%s copied=%d failed=%d, want completed/1/0", job.Status, job.Copied, job.Failed)
	}
	mu.Lock()
	n := attempts["flaky.txt"]
	mu.Unlock()
	if n < 2 {
		t.Fatalf("transient 503 should have been retried (>=2 GETs), got %d", n)
	}
	if !eng.ObjectExists("b", "flaky.txt") {
		t.Fatal("object should exist after successful retry")
	}
}

// TestMigratePermanentErrorNotRetried: a 404 is permanent and must NOT be retried.
func TestMigratePermanentErrorNotRetried(t *testing.T) {
	var mu sync.Mutex
	gets := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Path == "/" {
			io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
			return
		}
		if r.URL.Query().Get("list-type") == "2" {
			io.WriteString(w, `<ListBucketResult><Contents><Key>gone.txt</Key><Size>5</Size></Contents><IsTruncated>false</IsTruncated></ListBucketResult>`)
			return
		}
		// Bucket-config probes (?policy, ?tagging) are absent here — return 404
		// without counting them; this test is about OBJECT-GET retry behavior.
		if _, ok := r.URL.Query()["policy"]; ok {
			http.Error(w, "no policy", http.StatusNotFound)
			return
		}
		if _, ok := r.URL.Query()["tagging"]; ok {
			http.Error(w, "no tags", http.StatusNotFound)
			return
		}
		mu.Lock()
		gets++
		mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound) // permanent
	}))
	defer srv.Close()

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback
	id, _ := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s"})
	job := waitDone(t, m, id)
	_ = eng

	if job.Failed != 1 {
		t.Fatalf("expected 1 failed object (404), got failed=%d", job.Failed)
	}
	mu.Lock()
	n := gets
	mu.Unlock()
	if n != 1 {
		t.Fatalf("404 must not be retried — expected 1 GET, got %d", n)
	}
}

func TestMigrateBadEndpoint(t *testing.T) {
	m := NewManager(nil, nil)
	m.AllowPrivateSources(true) // httptest listens on loopback
	if _, err := m.TestConnection(StartConfig{Endpoint: "http://127.0.0.1:1", AccessKey: "k", SecretKey: "s"}); err == nil {
		t.Fatal("expected error connecting to a dead endpoint")
	}
}

// TestGetObjectSpecialCharsSignature is a regression guard for issue #9: object
// keys containing '&', '$', or spaces produced a SigV4 SignatureDoesNotMatch
// because the path was escaped with Go's default rules (which leave sub-delims
// literal) instead of the strict AWS canonical encoding. The stub server here
// recomputes the signature the strict (AWS) way and rejects any mismatch.
func TestGetObjectSpecialCharsSignature(t *testing.T) {
	const access, secret, region = "AKIATEST", "secretkey1234567890", "us-east-1"
	const bucket = "bucket1"
	const key = "1027708 Artik & ASTI feat Artyom $ Kacher/soft-slidertick4.wav"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Signature=" + strictSigV4(r, secret, region)
		if !strings.HasSuffix(r.Header.Get("Authorization"), want) {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>SignatureDoesNotMatch</Code></Error>`)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		io.WriteString(w, "AUDIO")
	}))
	defer srv.Close()

	obj, err := newSourceAllowingPrivate(srv.URL, access, secret, region, 10).GetObject(bucket, key)
	if err != nil {
		t.Fatalf("GetObject with special chars (&, $, space): %v", err)
	}
	defer obj.Body.Close()
	if b, _ := io.ReadAll(obj.Body); string(b) != "AUDIO" {
		t.Fatalf("body = %q, want AUDIO", b)
	}
}

// strictSigV4 recomputes the request's SigV4 signature using strict AWS canonical
// URI encoding — the encoding a real S3 server uses to validate the request.
func strictSigV4(r *http.Request, secret, region string) string {
	amzDate := r.Header.Get("X-Amz-Date")
	dateStr := amzDate[:8]
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", r.Host, payloadHash, amzDate)
	canonReq := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s", r.Method, uriEncodePath(r.URL.Path), "", canonHeaders, signedHeaders, payloadHash)
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStr, region)
	hash := sha256.Sum256([]byte(canonReq))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, scope, hex.EncodeToString(hash[:]))
	return hex.EncodeToString(hmacSHA256(deriveKey(secret, dateStr, region, "s3"), []byte(stringToSign)))
}

// TestMigrateCancel verifies an in-progress migration can be cancelled: the job
// ends in "cancelled" status, stops copying (copied < total), and a second
// cancel on the now-finished job is a no-op. (issue #8)
func TestMigrateCancel(t *testing.T) {
	const n = 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Path == "/" {
			io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
			return
		}
		if r.URL.Query().Get("list-type") == "2" {
			var sb strings.Builder
			sb.WriteString(`<ListBucketResult>`)
			for i := 0; i < n; i++ {
				fmt.Fprintf(&sb, `<Contents><Key>obj-%03d</Key><Size>1</Size></Contents>`, i)
			}
			sb.WriteString(`<IsTruncated>false</IsTruncated></ListBucketResult>`)
			io.WriteString(w, sb.String())
			return
		}
		if bucketMetaProbe(w, r) {
			return
		}
		time.Sleep(5 * time.Millisecond) // slow each GET so cancel can interleave
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback
	id, err := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(40 * time.Millisecond) // let a few objects copy
	if !m.Cancel(id) {
		t.Fatal("Cancel returned false for a running job")
	}
	job := waitDone(t, m, id)

	if job.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", job.Status)
	}
	if job.Copied >= n {
		t.Fatalf("copied %d of %d — cancel did not stop the migration", job.Copied, n)
	}
	if m.Cancel(id) {
		t.Fatal("Cancel on a finished job should return false")
	}
}

// TestMigrateRejectsDuplicate verifies a second migration of the same source +
// buckets is rejected while the first is still running (issue #8 — double-click
// guard).
func TestMigrateRejectsDuplicate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Path == "/" {
			io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
			return
		}
		if r.URL.Query().Get("list-type") == "2" {
			var sb strings.Builder
			sb.WriteString(`<ListBucketResult>`)
			for i := 0; i < 200; i++ {
				fmt.Fprintf(&sb, `<Contents><Key>obj-%03d</Key><Size>1</Size></Contents>`, i)
			}
			sb.WriteString(`<IsTruncated>false</IsTruncated></ListBucketResult>`)
			io.WriteString(w, sb.String())
			return
		}
		if bucketMetaProbe(w, r) {
			return
		}
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback
	cfg := StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s", Buckets: []string{"b"}}

	id1, err := m.Start(cfg)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := m.Start(cfg); err == nil {
		t.Fatal("expected duplicate migration to be rejected while the first is running")
	}
	// after cancelling the first, a new one is allowed again
	m.Cancel(id1)
	waitDone(t, m, id1)
	id3, err := m.Start(cfg)
	if err != nil {
		t.Fatalf("Start after first finished should be allowed: %v", err)
	}
	// Drain the third migration before the test returns; otherwise its background
	// goroutine keeps writing to the engine's t.TempDir() while RemoveAll runs,
	// which races as "directory not empty" cleanup failures in CI.
	m.Cancel(id3)
	waitDone(t, m, id3)
}

// TestMigratePreservesMetadata verifies a migration carries over the source's
// original modified time, user metadata, and content headers instead of stamping
// today's date — so it's a faithful copy, not a same-day re-upload (issue #13).
func TestMigratePreservesMetadata(t *testing.T) {
	orig := time.Date(2020, 1, 15, 8, 30, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<ListBucketResult><Contents><Key>report.pdf</Key><Size>5</Size>`+
				`<ETag>"x"</ETag><LastModified>%s</LastModified></Contents>`+
				`<IsTruncated>false</IsTruncated></ListBucketResult>`, orig.Format(time.RFC3339))
			return
		}
		// GetObject — return the metadata-rich headers a real S3 source would.
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Last-Modified", orig.Format(http.TimeFormat))
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Language", "en")
		w.Header().Set("X-Amz-Meta-Author", "matt")
		w.Header().Set("X-Amz-Meta-Project", "archive")
		w.Write([]byte("HELLO"))
	}))
	defer srv.Close()

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback
	id, err := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s", Buckets: []string{"old"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job := waitDone(t, m, id); job.Status != "completed" || job.Copied != 1 {
		t.Fatalf("status=%s copied=%d err=%s", job.Status, job.Copied, job.Error)
	}

	meta, err := store.GetObjectMeta("old", "report.pdf")
	if err != nil {
		t.Fatalf("GetObjectMeta: %v", err)
	}
	if meta.LastModified != orig.Unix() {
		t.Errorf("LastModified = %d (%s), want preserved %d (%s)",
			meta.LastModified, time.Unix(meta.LastModified, 0).UTC(), orig.Unix(), orig)
	}
	if meta.UserMetadata["author"] != "matt" || meta.UserMetadata["project"] != "archive" {
		t.Errorf("UserMetadata = %v, want author=matt project=archive", meta.UserMetadata)
	}
	if meta.ContentType != "application/pdf" || meta.ContentEncoding != "gzip" ||
		meta.CacheControl != "max-age=3600" || meta.ContentLanguage != "en" {
		t.Errorf("content headers not preserved: type=%q enc=%q cache=%q lang=%q",
			meta.ContentType, meta.ContentEncoding, meta.CacheControl, meta.ContentLanguage)
	}
}

// bucketMetaProbe answers a ?policy / ?tagging request with 404 (absent) so
// stubs that don't model bucket config aren't mistaken for serving one. Returns
// true if it handled the request.
func bucketMetaProbe(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if _, ok := q["policy"]; ok {
		http.Error(w, "no policy", http.StatusNotFound)
		return true
	}
	if _, ok := q["tagging"]; ok {
		http.Error(w, "no tags", http.StatusNotFound)
		return true
	}
	return false
}

// TestMigrateCopiesBucketPolicyAndTags verifies the IAM/policies half of a
// migration: the source bucket's policy and tags are carried over (issue:
// migrate IAM/policies). User/access-key migration is intentionally out of scope.
func TestMigrateCopiesBucketPolicyAndTags(t *testing.T) {
	const policyJSON = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::secured/*"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if _, ok := q["policy"]; ok {
			io.WriteString(w, policyJSON)
			return
		}
		if _, ok := q["tagging"]; ok {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<Tagging><TagSet><Tag><Key>env</Key><Value>prod</Value></Tag><Tag><Key>team</Key><Value>data</Value></Tag></TagSet></Tagging>`)
			return
		}
		if q.Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<ListBucketResult><Contents><Key>file.txt</Key><Size>3</Size></Contents><IsTruncated>false</IsTruncated></ListBucketResult>`)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "abc")
	}))
	defer srv.Close()

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback
	id, err := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s", Buckets: []string{"secured"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)
	if job.Status != "completed" || job.Copied != 1 {
		t.Fatalf("status=%s copied=%d err=%s", job.Status, job.Copied, job.Error)
	}
	if job.Policies != 1 {
		t.Fatalf("Policies = %d, want 1", job.Policies)
	}

	gotPolicy, err := store.GetBucketPolicy("secured")
	if err != nil || string(gotPolicy) != policyJSON {
		t.Fatalf("bucket policy not migrated: err=%v got=%s", err, gotPolicy)
	}
	tags, err := store.GetBucketTags("secured")
	if err != nil {
		t.Fatalf("GetBucketTags: %v", err)
	}
	if tags["env"] != "prod" || tags["team"] != "data" {
		t.Fatalf("bucket tags not migrated, got %v", tags)
	}
}

// TestMigrateResumeSkipsExisting verifies issue #24: a migration re-run (after a
// restart or crash) skips objects already present at the destination instead of
// re-copying the whole bucket.
func TestMigrateResumeSkipsExisting(t *testing.T) {
	objects := map[string][]byte{
		"docs/a.txt": []byte("alpha"),
		"docs/b.txt": []byte("bravo"),
		"docs/c.txt": []byte("charlie"),
	}
	endpoint := stubS3(t, objects)
	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback

	// First run copies everything.
	id, err := m.Start(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j1 := waitDone(t, m, id)
	if j1.Status != "completed" || j1.Copied != 3 || j1.Skipped != 0 {
		t.Fatalf("first run: status=%s copied=%d skipped=%d", j1.Status, j1.Copied, j1.Skipped)
	}

	// Second run (a restart) must skip all three and copy nothing.
	id2, err := m.Start(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	j2 := waitDone(t, m, id2)
	if j2.Status != "completed" {
		t.Fatalf("second run status=%s err=%s", j2.Status, j2.Error)
	}
	if j2.Copied != 0 || j2.Skipped != 3 || j2.Failed != 0 {
		t.Fatalf("resume: copied=%d skipped=%d failed=%d, want 0/3/0", j2.Copied, j2.Skipped, j2.Failed)
	}

	// Data is still intact after the resumed (no-op) run.
	rc, _, err := eng.GetObject("docs", "a.txt")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "alpha" {
		t.Fatalf("object changed on resume: %q", got)
	}
}

// TestMigrateCopiesConcurrently verifies the copy loop runs objects in parallel
// (issue #24, kesavkolla's comment) and respects the concurrency bound.
func TestMigrateCopiesConcurrently(t *testing.T) {
	const n = 12
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%02d.txt", i)
	}

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case r.URL.Path == "/":
			io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
		case r.URL.Query().Get("list-type") == "2":
			// Return all keys in a single page so the worker pool has real work.
			var b strings.Builder
			b.WriteString(`<ListBucketResult>`)
			for _, k := range keys {
				fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>3</Size><ETag>"x"</ETag></Contents>`, k)
			}
			b.WriteString(`<IsTruncated>false</IsTruncated></ListBucketResult>`)
			io.WriteString(w, b.String())
		default: // GetObject — measure how many run at once.
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			time.Sleep(40 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("abc"))
		}
	}))
	t.Cleanup(srv.Close)

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback
	id, err := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s", Concurrency: 4})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitDone(t, m, id)
	if j.Status != "completed" || j.Copied != n {
		t.Fatalf("status=%s copied=%d, want completed/%d", j.Status, j.Copied, n)
	}

	mu.Lock()
	mx := maxInFlight
	mu.Unlock()
	if mx < 2 {
		t.Fatalf("expected concurrent copies (>1), max in-flight was %d", mx)
	}
	if mx > 4 {
		t.Fatalf("concurrency exceeded the configured limit of 4: max in-flight %d", mx)
	}
}

// TestMigrateResumePartial mirrors the reporter's exact case in issue #24: a
// prior migration copied some objects before it stopped; the re-run copies only
// the missing ones and skips what is already there.
func TestMigrateResumePartial(t *testing.T) {
	objects := map[string][]byte{
		"docs/a.txt": []byte("alpha"),
		"docs/b.txt": []byte("bravo"),
		"docs/c.txt": []byte("charlie"),
	}
	endpoint := stubS3(t, objects)
	store, eng := newLocal(t)
	m := NewManager(store, eng)
	m.AllowPrivateSources(true) // httptest listens on loopback

	// Simulate a prior run that copied only docs/a.txt before it died.
	if err := store.CreateBucket("docs"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	eng.CreateBucketDir("docs")
	w, etag, err := eng.PutObject("docs", "a.txt", strings.NewReader("alpha"), 5)
	if err != nil {
		t.Fatalf("seed PutObject: %v", err)
	}
	if err := store.PutObjectMeta(metadata.ObjectMeta{
		Bucket: "docs", Key: "a.txt", Size: w, ETag: etag, LastModified: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("seed PutObjectMeta: %v", err)
	}

	id, err := m.Start(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitDone(t, m, id)
	if j.Status != "completed" {
		t.Fatalf("status=%s err=%s", j.Status, j.Error)
	}
	if j.Copied != 2 || j.Skipped != 1 || j.Failed != 0 {
		t.Fatalf("partial resume: copied=%d skipped=%d failed=%d, want 2/1/0", j.Copied, j.Skipped, j.Failed)
	}

	// The two missing objects are now present with correct content.
	for _, tc := range []struct{ key, want string }{{"b.txt", "bravo"}, {"c.txt", "charlie"}} {
		rc, _, err := eng.GetObject("docs", tc.key)
		if err != nil {
			t.Fatalf("GetObject %s: %v", tc.key, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != tc.want {
			t.Fatalf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// TestSourceSlowBodyNotTimedOut verifies issue #26: the source client no longer
// caps the whole request (including reading the body) with a total timeout, so a
// large object whose body takes longer than the (former) timeout to stream is
// not killed with "context deadline exceeded ... while reading body". The client
// only bounds time-to-first-byte now.
func TestSourceSlowBodyNotTimedOut(t *testing.T) {
	const chunks = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Headers arrive immediately; the body then streams slowly for ~1.5s.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("responsewriter is not a flusher")
			return
		}
		fl.Flush()
		for i := 0; i < chunks; i++ {
			w.Write([]byte("chunkdata"))
			fl.Flush()
			time.Sleep(250 * time.Millisecond)
		}
	}))
	defer srv.Close()

	// timeoutSecs=1: the total body read (~1.5s) exceeds it. With the old
	// http.Client.Timeout this would fail; now it only bounds the response header.
	src := newSourceAllowingPrivate(srv.URL, "k", "s", "us-east-1", 1)
	obj, err := src.GetObject("bucket", "key")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer obj.Body.Close()
	data, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("read slow body: %v", err)
	}
	if want := chunks * len("chunkdata"); len(data) != want {
		t.Fatalf("read %d bytes, want %d", len(data), want)
	}
}
