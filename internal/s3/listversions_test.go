package s3

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// S3 reports an object written before its bucket was versioned as a version
// whose id is the literal "null". VaultS3 returned nothing for those, because it
// read only the version index and such objects have no entry there.
//
// This is how a bucket became undeletable: ListObjectVersions is what tools use
// to enumerate a bucket before emptying it, so a caller saw an empty bucket,
// deleted nothing, and then got BucketNotEmpty from DeleteBucket. Found by the
// ceph/s3-tests fixture, which failed the setup of every test after the first.
func TestSortVersionsForListingOrdersByKeyThenLatestFirst(t *testing.T) {
	versions := []metadata.ObjectMeta{
		{Key: "b", VersionID: "v1", LastModified: 100},
		{Key: "a", VersionID: "v2", LastModified: 200},
		{Key: "a", VersionID: "v3", IsLatest: true, LastModified: 300},
		{Key: "a", VersionID: "v1", LastModified: 100},
	}
	sortVersionsForListing(versions)

	if versions[0].Key != "a" || !versions[0].IsLatest {
		t.Fatalf("first entry = %+v, want key a and IsLatest", versions[0])
	}
	for i := 1; i < len(versions); i++ {
		if versions[i-1].Key > versions[i].Key {
			t.Fatalf("keys out of order at %d: %q then %q", i, versions[i-1].Key, versions[i].Key)
		}
	}
	// Within key "a", newest first after the latest.
	if versions[1].Key == "a" && versions[2].Key == "a" &&
		versions[1].LastModified < versions[2].LastModified {
		t.Errorf("within a key, newer versions must come first: %d then %d",
			versions[1].LastModified, versions[2].LastModified)
	}
}

// The constant is what callers compare against; pin it so it cannot drift into
// something like "NULL" that no S3 client would recognise.
func TestNullVersionIDIsTheLiteralNull(t *testing.T) {
	if nullVersionID != "null" {
		t.Fatalf("nullVersionID = %q, S3 requires the literal \"null\"", nullVersionID)
	}
}

// Deleting version "null" of an object that predates versioning must delete the
// object's bytes, not just its metadata. It used to remove the index entry and
// leave the file on disk: an orphan nothing referenced, and a bucket DeleteBucket
// would never accept as empty, because it asks the storage engine rather than
// the index.
//
// This uses a REAL filesystem engine, not the package mock: the mock answers
// ObjectExists with a constant false, so it cannot tell a removed file from an
// orphaned one, which is the only thing this test is about.
func TestDeleteNullVersionRemovesTheObjectItself(t *testing.T) {
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	engine, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}
	h := &ObjectHandler{store: store, mpStore: store, engine: engine}

	const bucket, key = "nullver", "asdf"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if err := engine.CreateBucketDir(bucket); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.PutObject(bucket, key, strings.NewReader("abc"), 3); err != nil {
		t.Fatal(err)
	}
	// No VersionID: this is what an object written before versioning looks like.
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: bucket, Key: key, Size: 3}); err != nil {
		t.Fatal(err)
	}
	if !engine.ObjectExists(bucket, key) {
		t.Fatal("setup: the object should exist on disk")
	}

	if _, err := h.deleteObjectVersion(bucket, key, nullVersionID, false); err != nil {
		t.Fatalf("delete null version: %v", err)
	}

	if engine.ObjectExists(bucket, key) {
		t.Error("the bytes are still on disk: this orphan is what made the bucket undeletable")
	}
	if meta, err := store.GetObjectMeta(bucket, key); err == nil && meta != nil {
		t.Error("metadata still present after deleting the null version")
	}
}
