package metadata

import (
	"fmt"
	"path/filepath"
	"testing"
)

// Deleting a bucket or aborting an upload must remove every record under its
// prefix. A delete inside a bbolt cursor walk shifts the next key into the
// cursor's position, so a Next() after it can skip a record and leave ghost
// metadata that a recreated bucket of the same name would inherit.
func TestPrefixDeletesRemoveEveryRecord(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	const n = 1000
	for i := 0; i < n; i++ {
		if err := s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: fmt.Sprintf("k%05d", i), Size: 1}); err != nil {
			t.Fatalf("PutObjectMeta: %v", err)
		}
	}
	// A bystander bucket that shares the name as a prefix must survive.
	if err := s.PutObjectMeta(ObjectMeta{Bucket: "bb", Key: "keep", Size: 1}); err != nil {
		t.Fatalf("PutObjectMeta: %v", err)
	}
	if err := s.DeleteBucketObjectMeta("b"); err != nil {
		t.Fatalf("DeleteBucketObjectMeta: %v", err)
	}
	left := 0
	s.IterateAllObjects(func(bucket, key string, _ ObjectMeta) bool {
		if bucket == "b" {
			left++
		}
		return true
	})
	if left != 0 {
		t.Fatalf("DeleteBucketObjectMeta left %d of %d records behind", left, n)
	}
	if _, err := s.GetObjectMeta("bb", "keep"); err != nil {
		t.Fatalf("bystander bucket lost its object: %v", err)
	}

	if err := s.CreateMultipartUpload(MultipartUpload{UploadID: "u1", Bucket: "b", Key: "k"}); err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	for i := 1; i <= n; i++ {
		if err := s.PutPart("u1", PartInfo{PartNumber: i, Size: 1}); err != nil {
			t.Fatalf("PutPart: %v", err)
		}
	}
	if err := s.DeleteMultipartUpload("u1"); err != nil {
		t.Fatalf("DeleteMultipartUpload: %v", err)
	}
	parts, err := s.ListParts("u1")
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("DeleteMultipartUpload left %d of %d parts behind", len(parts), n)
	}
}
