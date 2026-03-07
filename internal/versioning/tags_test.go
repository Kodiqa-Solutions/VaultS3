package versioning

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

func newTestStore(t *testing.T) *metadata.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := metadata.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// setupVersionedObject creates a bucket with versioning and puts an object version.
func setupVersionedObject(t *testing.T, store *metadata.Store, bucket, key, versionID string) {
	t.Helper()
	store.CreateBucket(bucket)
	store.SetBucketVersioning(bucket, "Enabled")
	store.PutObjectVersion(metadata.ObjectMeta{
		Bucket:       bucket,
		Key:          key,
		VersionID:    versionID,
		ContentType:  "text/plain",
		Size:         10,
		LastModified: time.Now().Unix(),
	})
}

func TestTagStore_PutAndGet(t *testing.T) {
	store := newTestStore(t)
	ts := NewTagStore(store)

	setupVersionedObject(t, store, "mybucket", "file.txt", "v1")

	tag := VersionTag{
		Name:      "release-1.0",
		Bucket:    "mybucket",
		Key:       "file.txt",
		VersionID: "v1",
		CreatedBy: "admin",
	}
	if err := ts.PutTag(tag); err != nil {
		t.Fatalf("PutTag: %v", err)
	}

	tags, err := ts.GetTags("mybucket", "file.txt")
	if err != nil {
		t.Fatalf("GetTags: %v", err)
	}
	if len(tags) != 1 {
		t.Fatalf("expected 1 tag, got %d", len(tags))
	}
	if tags[0].Name != "release-1.0" {
		t.Errorf("expected release-1.0, got %s", tags[0].Name)
	}
	if tags[0].CreatedAt == 0 {
		t.Error("expected CreatedAt to be set")
	}
}

func TestTagStore_MultipleTags(t *testing.T) {
	store := newTestStore(t)
	ts := NewTagStore(store)

	setupVersionedObject(t, store, "mybucket", "file.txt", "v1")
	setupVersionedObject(t, store, "mybucket", "file.txt", "v2")

	ts.PutTag(VersionTag{Name: "alpha", Bucket: "mybucket", Key: "file.txt", VersionID: "v1"})
	ts.PutTag(VersionTag{Name: "beta", Bucket: "mybucket", Key: "file.txt", VersionID: "v2"})

	tags, _ := ts.GetTags("mybucket", "file.txt")
	if len(tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(tags))
	}
}

func TestTagStore_DeleteTag(t *testing.T) {
	store := newTestStore(t)
	ts := NewTagStore(store)

	setupVersionedObject(t, store, "mybucket", "file.txt", "v1")
	ts.PutTag(VersionTag{Name: "temp", Bucket: "mybucket", Key: "file.txt", VersionID: "v1"})

	if err := ts.DeleteTag("mybucket", "file.txt", "temp"); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}

	tags, _ := ts.GetTags("mybucket", "file.txt")
	if len(tags) != 0 {
		t.Errorf("expected 0 tags after delete, got %d", len(tags))
	}
}

func TestTagStore_GetVersionByTag(t *testing.T) {
	store := newTestStore(t)
	ts := NewTagStore(store)

	setupVersionedObject(t, store, "mybucket", "file.txt", "v1")
	ts.PutTag(VersionTag{Name: "prod", Bucket: "mybucket", Key: "file.txt", VersionID: "v1"})

	tag, err := ts.GetVersionByTag("mybucket", "file.txt", "prod")
	if err != nil {
		t.Fatalf("GetVersionByTag: %v", err)
	}
	if tag.VersionID != "v1" {
		t.Errorf("expected version v1, got %s", tag.VersionID)
	}
}

func TestTagStore_GetVersionByTag_NotFound(t *testing.T) {
	store := newTestStore(t)
	ts := NewTagStore(store)

	_, err := ts.GetVersionByTag("mybucket", "file.txt", "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent tag")
	}
}

func TestTagStore_PutTag_VersionNotFound(t *testing.T) {
	store := newTestStore(t)
	ts := NewTagStore(store)

	store.CreateBucket("mybucket")

	err := ts.PutTag(VersionTag{
		Name:      "bad",
		Bucket:    "mybucket",
		Key:       "file.txt",
		VersionID: "nonexistent",
	})
	if err == nil {
		t.Error("expected error when version doesn't exist")
	}
}

func TestTagStore_EmptyTags(t *testing.T) {
	store := newTestStore(t)
	ts := NewTagStore(store)

	tags, err := ts.GetTags("mybucket", "file.txt")
	if err != nil {
		t.Fatalf("GetTags: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("expected 0 tags, got %d", len(tags))
	}
}

func TestTagStore_OverwriteTag(t *testing.T) {
	store := newTestStore(t)
	ts := NewTagStore(store)

	setupVersionedObject(t, store, "mybucket", "file.txt", "v1")
	setupVersionedObject(t, store, "mybucket", "file.txt", "v2")

	ts.PutTag(VersionTag{Name: "latest", Bucket: "mybucket", Key: "file.txt", VersionID: "v1"})
	ts.PutTag(VersionTag{Name: "latest", Bucket: "mybucket", Key: "file.txt", VersionID: "v2"})

	tag, _ := ts.GetVersionByTag("mybucket", "file.txt", "latest")
	if tag.VersionID != "v2" {
		t.Errorf("expected overwritten tag to point to v2, got %s", tag.VersionID)
	}

	tags, _ := ts.GetTags("mybucket", "file.txt")
	if len(tags) != 1 {
		t.Errorf("expected 1 tag after overwrite, got %d", len(tags))
	}
}

// --- tagKey/tagPrefix tests ---

func TestTagKey(t *testing.T) {
	key := tagKey("bucket", "key", "tag")
	if key != "bucket\x00key\x00tag" {
		t.Errorf("unexpected tag key: %q", key)
	}
}

func TestTagPrefix(t *testing.T) {
	prefix := tagPrefix("bucket", "key")
	if prefix != "bucket\x00key\x00" {
		t.Errorf("unexpected tag prefix: %q", prefix)
	}
}
