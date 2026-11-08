package tiering

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Regression (5.0.11 round 14): a PUT that landed while a restore copied the
// cold bytes back had its data replaced by the old object. The restore puts
// the data in place only where no hot file exists.
func TestRestoreDoesNotOverwriteAPutThatLandedMeanwhile(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "obj", []byte("old bytes"), time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "obj"); err != nil {
		t.Fatal(err)
	}
	newer := []byte("NEW bytes from a PUT during restore")
	r.mgr.beforeRestoreLink = func() {
		r.hot.PutObject("b", "obj", bytes.NewReader(newer), int64(len(newer)))
		r.store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "obj", ETag: "new", Size: int64(len(newer)), LastModified: time.Now().Unix() + 5})
	}
	if err := r.mgr.MigrateToHot("b", "obj"); err != nil {
		t.Fatal(err)
	}
	if got := r.read(t, r.hot, "obj"); !bytes.Equal(got, newer) {
		t.Fatalf("hot bytes %q: the PUT's data was overwritten by the restore", got)
	}
	if r.cold.ObjectExists("b", "obj") {
		t.Error("the stale cold copy was kept")
	}
}

// 5.0.10's RestoreObject marked a record hot and left its data in the cold
// tier; a restore now brings that data back. With nothing in the cold tier it
// reports ErrNothingToRestore instead of claiming a move.
func TestRestoreRecoversDataLeftColdByAnEarlierRestore(t *testing.T) {
	r := newTierRig(t, 30)
	data := []byte("stranded by 5.0.10")
	r.putHot(t, "obj", data, time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "obj"); err != nil {
		t.Fatal(err)
	}
	r.store.SetObjectTier("b", "obj", "hot") // what 5.0.10's restore did
	if _, err := os.Stat(r.hot.ObjectPath("b", "obj")); err == nil {
		t.Fatal("setup: the hot file should be gone")
	}
	if err := r.mgr.MigrateToHot("b", "obj"); err != nil {
		t.Fatalf("restore of stranded data: %v", err)
	}
	if got := r.read(t, r.hot, "obj"); !bytes.Equal(got, data) {
		t.Fatalf("restored %q", got)
	}
	if err := r.mgr.MigrateToHot("b", "obj"); !errors.Is(err, ErrNothingToRestore) {
		t.Errorf("a second restore: %v, want ErrNothingToRestore", err)
	}
}

// Regression (5.0.11 round 15): with the hot path now a directory (another
// key, "a/b", made one at "a"), a restore took it for newer data and dropped
// the only, cold, copy. It refuses and keeps the cold copy.
func TestRestoreKeepsTheColdCopyWhenTheHotPathIsADirectory(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "a", []byte("only copy"), time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "a"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.hot.ObjectPath("b", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.mgr.MigrateToHot("b", "a"); !errors.Is(err, ErrHotPathBlocked) {
		t.Fatalf("restore over a directory: %v, want ErrHotPathBlocked", err)
	}
	if !r.cold.ObjectExists("b", "a") {
		t.Fatal("the only copy, in the cold tier, was dropped")
	}
}

// An object deleted on purpose, whose cold file an earlier version left
// behind, is not brought back when the bucket's current version is another
// object (round 15).
func TestRestoreDoesNotBringBackADeletedObject(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "x", []byte("deleted on purpose"), time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "x"); err != nil {
		t.Fatal(err)
	}
	// Deleted, then versioning on and a new current version.
	r.store.DeleteObjectMeta("b", "x")
	cur := metadata.ObjectMeta{Bucket: "b", Key: "x", VersionID: "0000000000000009aaaaaaaa", IsLatest: true, Size: 3}
	r.store.PutObjectVersion(cur)
	r.store.PutObjectMeta(cur)
	if err := r.mgr.MigrateToHot("b", "x"); !errors.Is(err, ErrNothingToRestore) {
		t.Fatalf("restore of a deleted object below a new version: %v, want ErrNothingToRestore", err)
	}
	if _, err := os.Stat(r.hot.ObjectPath("b", "x")); err == nil {
		t.Fatal("the deleted object's bytes came back to the hot tier")
	}
}

// A DELETE that lands while a restore copies leaves no orphan behind
// (round 15).
func TestRestoreRacingADeleteLeavesNothingBehind(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "d", []byte("to be deleted"), time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "d"); err != nil {
		t.Fatal(err)
	}
	r.mgr.beforeRestoreLink = func() { r.store.DeleteObjectMeta("b", "d") }
	if err := r.mgr.MigrateToHot("b", "d"); !errors.Is(err, ErrNothingToRestore) {
		t.Fatalf("restore racing a delete: %v, want ErrNothingToRestore", err)
	}
	if _, err := os.Stat(r.hot.ObjectPath("b", "d")); err == nil {
		t.Error("the deleted object's bytes were left in the hot tier")
	}
	if r.cold.ObjectExists("b", "d") {
		t.Error("the deleted object's cold copy was left behind")
	}
}

// A cold folder marker ("m/") is a directory: a restore makes it again
// instead of answering an error (round 15).
func TestRestoreOfAFolderMarker(t *testing.T) {
	r := newTierRig(t, 30)
	r.store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "m/", Tier: "cold", LastModified: time.Now().Unix()})
	if err := os.MkdirAll(r.cold.ObjectPath("b", "m/"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.mgr.MigrateToHot("b", "m/"); err != nil {
		t.Fatalf("restore of a folder marker: %v", err)
	}
	if fi, err := os.Stat(r.hot.ObjectPath("b", "m/")); err != nil || !fi.IsDir() {
		t.Fatalf("the folder marker's directory was not made: %v", err)
	}
	if got := tierOf(t, r.store, "m/"); got != "hot" {
		t.Errorf("tier %q, want hot", got)
	}
}
