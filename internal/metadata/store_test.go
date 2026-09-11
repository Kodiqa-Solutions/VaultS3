package metadata

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A callback returning false ends the walk with ErrStopIteration, which callers
// distinguish from a real error with errors.Is.
func TestStore_IterateAllObjectsStops(t *testing.T) {
	s := newTestStore(t)
	s.CreateBucket("bucket")
	for _, k := range []string{"a", "b", "c"} {
		if err := s.PutObjectMeta(ObjectMeta{Bucket: "bucket", Key: k, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	err := s.IterateAllObjects(func(bucket, key string, meta ObjectMeta) bool {
		seen++
		return seen < 2
	})
	if !errors.Is(err, ErrStopIteration) {
		t.Fatalf("err = %v, want ErrStopIteration", err)
	}
	if seen != 2 {
		t.Fatalf("callback ran %d times, want 2", seen)
	}
	if err := s.IterateAllObjects(func(string, string, ObjectMeta) bool { return true }); err != nil {
		t.Fatalf("a full walk must return nil, got %v", err)
	}
}

// Prewarm is a plain sequential read of the whole database file — it must read
// exactly the file and leave the store usable.
func TestStore_Prewarm(t *testing.T) {
	s := newTestStore(t)
	s.CreateBucket("bucket")
	if err := s.PutObjectMeta(ObjectMeta{Bucket: "bucket", Key: "k", Size: 1}); err != nil {
		t.Fatal(err)
	}

	n, err := s.Prewarm()
	if err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	info, err := os.Stat(s.db.Path())
	if err != nil {
		t.Fatal(err)
	}
	if n != info.Size() {
		t.Fatalf("Prewarm read %d bytes, file is %d", n, info.Size())
	}
	if _, err := s.GetObjectMeta("bucket", "k"); err != nil {
		t.Fatalf("store unusable after Prewarm: %v", err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_BucketCRUD(t *testing.T) {
	s := newTestStore(t)

	// Create
	if err := s.CreateBucket("test-bucket"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// List
	buckets, err := s.ListBuckets()
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Name != "test-bucket" {
		t.Errorf("expected [test-bucket], got %v", buckets)
	}

	// Duplicate create
	if err := s.CreateBucket("test-bucket"); err == nil {
		t.Error("expected error on duplicate bucket")
	}

	// Delete
	if err := s.DeleteBucket("test-bucket"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	buckets, _ = s.ListBuckets()
	if len(buckets) != 0 {
		t.Errorf("expected 0 buckets after delete, got %d", len(buckets))
	}
}

func TestStore_ObjectMeta(t *testing.T) {
	s := newTestStore(t)
	s.CreateBucket("bucket")

	meta := ObjectMeta{
		Bucket:      "bucket",
		Key:         "file.txt",
		Size:        42,
		ContentType: "text/plain",
		ETag:        "abc123",
	}

	if err := s.PutObjectMeta(meta); err != nil {
		t.Fatalf("PutObjectMeta: %v", err)
	}

	got, err := s.GetObjectMeta("bucket", "file.txt")
	if err != nil {
		t.Fatalf("GetObjectMeta: %v", err)
	}
	if got.Size != 42 || got.ContentType != "text/plain" {
		t.Errorf("got %+v", got)
	}

	if err := s.DeleteObjectMeta("bucket", "file.txt"); err != nil {
		t.Fatalf("DeleteObjectMeta: %v", err)
	}
	if _, err := s.GetObjectMeta("bucket", "file.txt"); err == nil {
		t.Error("expected error after delete")
	}
}

func TestStore_BucketTags(t *testing.T) {
	s := newTestStore(t)
	s.CreateBucket("tagged")

	tags := map[string]string{"env": "prod", "team": "backend"}
	if err := s.PutBucketTags("tagged", tags); err != nil {
		t.Fatalf("PutBucketTags: %v", err)
	}

	got, err := s.GetBucketTags("tagged")
	if err != nil {
		t.Fatalf("GetBucketTags: %v", err)
	}
	if got["env"] != "prod" || got["team"] != "backend" {
		t.Errorf("expected env=prod,team=backend, got %v", got)
	}

	if err := s.DeleteBucketTags("tagged"); err != nil {
		t.Fatalf("DeleteBucketTags: %v", err)
	}
	got, err = s.GetBucketTags("tagged")
	if err != nil {
		t.Fatalf("GetBucketTags after delete: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty tags after delete, got %v", got)
	}
}

func TestStore_AccessKeys(t *testing.T) {
	s := newTestStore(t)

	key := AccessKey{
		AccessKey: "AKIA123",
		SecretKey: "secret123",
		UserID:    "admin",
	}
	if err := s.CreateAccessKey(key); err != nil {
		t.Fatalf("CreateAccessKey: %v", err)
	}

	got, err := s.GetAccessKey("AKIA123")
	if err != nil {
		t.Fatalf("GetAccessKey: %v", err)
	}
	if got.SecretKey != "secret123" || got.UserID != "admin" {
		t.Errorf("got %+v", got)
	}

	keys, err := s.ListAccessKeys()
	if err != nil {
		t.Fatalf("ListAccessKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("expected 1 key, got %d", len(keys))
	}

	if err := s.DeleteAccessKey("AKIA123"); err != nil {
		t.Fatalf("DeleteAccessKey: %v", err)
	}
	if _, err := s.GetAccessKey("AKIA123"); err == nil {
		t.Error("expected error after delete")
	}
}

func TestStore_IAMPolicy(t *testing.T) {
	s := newTestStore(t)

	policy := IAMPolicy{
		Name:     "TestPolicy",
		Document: `{"Version":"2012-10-17","Statement":[]}`,
	}
	if err := s.CreateIAMPolicy(policy); err != nil {
		t.Fatalf("CreateIAMPolicy: %v", err)
	}

	got, err := s.GetIAMPolicy("TestPolicy")
	if err != nil {
		t.Fatalf("GetIAMPolicy: %v", err)
	}
	if got.Name != "TestPolicy" {
		t.Errorf("expected TestPolicy, got %s", got.Name)
	}

	policies, err := s.ListIAMPolicies()
	if err != nil {
		t.Fatalf("ListIAMPolicies: %v", err)
	}
	if len(policies) != 1 {
		t.Errorf("expected 1 policy, got %d", len(policies))
	}
}

func TestStore_MultipartUploads(t *testing.T) {
	s := newTestStore(t)

	upload := MultipartUpload{
		UploadID: "upload-123",
		Bucket:   "mybucket",
		Key:      "bigfile.bin",
	}
	if err := s.CreateMultipartUpload(upload); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	got, err := s.GetMultipartUpload("upload-123")
	if err != nil {
		t.Fatalf("GetMultipartUpload: %v", err)
	}
	if got.Bucket != "mybucket" || got.Key != "bigfile.bin" {
		t.Errorf("got %+v", got)
	}

	uploads, err := s.ListMultipartUploads("mybucket")
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if len(uploads) != 1 {
		t.Errorf("expected 1 upload, got %d", len(uploads))
	}
}

func TestStore_AuditEntries(t *testing.T) {
	s := newTestStore(t)

	entry := AuditEntry{
		Time:      1700000000,
		Principal: "admin",
		Action:    "s3:GetObject",
		Resource:  "mybucket/file.txt",
		Effect:    "Allow",
	}
	if err := s.PutAuditEntry(entry); err != nil {
		t.Fatalf("PutAuditEntry: %v", err)
	}

	entries, err := s.ListAuditEntries(10, 0, 0, "", "")
	if err != nil {
		t.Fatalf("QueryAuditEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Principal != "admin" {
		t.Errorf("expected 1 entry by admin, got %v", entries)
	}
}

func TestNewStore_InvalidPath(t *testing.T) {
	_, err := NewStore(filepath.Join(os.DevNull, "nonexistent", "test.db"))
	if err == nil {
		t.Error("expected error for invalid path")
	}
}
