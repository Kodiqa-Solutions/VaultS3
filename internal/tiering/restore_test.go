package tiering

import (
	"bytes"
	"context"
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

// Regression (5.0.12): a move to cold removed whatever file was at the hot
// path once its checks passed, so a PUT that landed in that window lost its
// data. The file is moved aside and kept unless it is the one that was copied.
func TestMoveToColdKeepsAPutThatLandedMeanwhile(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "k", []byte("old bytes"), time.Now().Unix()-90*86400)
	newer := []byte("NEW bytes from a PUT during the move")
	r.mgr.beforeColdRemove = func() {
		r.hot.PutObject("b", "k", bytes.NewReader(newer), int64(len(newer)))
	}
	if err := r.mgr.migrateToCol("b", "k"); !errors.Is(err, errChanged) {
		t.Fatalf("move racing a PUT: %v, want errChanged", err)
	}
	if got := r.read(t, r.hot, "k"); !bytes.Equal(got, newer) {
		t.Fatalf("hot bytes %q: the PUT's data was removed by the move", got)
	}
	if r.cold.ObjectExists("b", "k") {
		t.Error("the stale cold copy was kept")
	}
	if got := tierOf(t, r.store, "k"); got == "cold" {
		t.Error("the record says cold although the data is the hot PUT")
	}
}

// Moving "a/b" to cold leaves no empty folder "a" behind, which would stop an
// object named "a" from being written.
func TestMoveToColdRemovesTheFoldersItEmpties(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "a/b", []byte("child"), time.Now().Unix()-90*86400)
	if err := r.mgr.migrateToCol("b", "a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.hot.ObjectPath("b", "a")); err == nil {
		t.Fatal("the empty folder a was left in the hot tier")
	}
}

// Under erasure coding, packing or a cluster an object is not one file at its
// object path, so moves to cold are switched off there; restores still run.
func TestDisabledColdMovesRefuseAndStillRestore(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "was-cold", []byte("cold before"), time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "was-cold"); err != nil {
		t.Fatal(err)
	}
	r.putHot(t, "obj", []byte("data"), 1) // long past migrate_after_days
	r.mgr.DisableColdMoves("test layout")

	if err := r.mgr.ManualMigrate("b", "obj", "cold"); !errors.Is(err, ErrColdMovesUnsupported) {
		t.Fatalf("move to cold with moves off: %v, want ErrColdMovesUnsupported", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.mgr.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run kept scanning with moves off")
	}
	if r.cold.ObjectExists("b", "obj") || tierOf(t, r.store, "obj") == "cold" {
		t.Fatal("an object moved to cold with moves off")
	}
	if err := r.mgr.ManualMigrate("b", "was-cold", "hot"); err != nil {
		t.Fatalf("restore with moves off: %v", err)
	}
	if got := r.read(t, r.hot, "was-cold"); string(got) != "cold before" {
		t.Fatalf("restored %q", got)
	}
}

// Regression (5.0.12 review): two moves of one key overlapped (the scan and a
// manual move). The first to finish removed the hot file; the other then found
// none, "reverted to hot" and dropped the cold copy, the only one left.
func TestOverlappingMovesKeepTheObject(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "obj", []byte("the only copy"), time.Now().Add(-48*time.Hour).Unix())
	cold := &hookedCold{Engine: r.cold}
	inner := make(chan error, 1)
	cold.after = func() {
		go func() { inner <- r.mgr.migrateToCol("b", "obj") }()
		time.Sleep(50 * time.Millisecond) // let it reach the key lock
	}
	r.mgr.coldEngine = cold
	if err := r.mgr.migrateToCol("b", "obj"); err != nil {
		t.Fatalf("outer move: %v", err)
	}
	if err := <-inner; err != nil {
		t.Fatalf("inner move: %v", err)
	}
	if got := r.read(t, r.cold, "obj"); string(got) != "the only copy" || tierOf(t, r.store, "obj") != "cold" {
		t.Fatalf("after two moves: cold %q, tier %q", got, tierOf(t, r.store, "obj"))
	}
}

// With the hot file already gone (removed by something else) and the record
// unchanged, the move keeps the cold copy instead of dropping it.
func TestMoveWhoseHotFileVanishedKeepsTheColdCopy(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "obj", []byte("the only copy"), time.Now().Add(-48*time.Hour).Unix())
	r.mgr.beforeColdRemove = func() { os.Remove(r.hot.ObjectPath("b", "obj")) }
	if err := r.mgr.migrateToCol("b", "obj"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := r.read(t, r.cold, "obj"); string(got) != "the only copy" || tierOf(t, r.store, "obj") != "cold" {
		t.Fatalf("cold %q, tier %q", got, tierOf(t, r.store, "obj"))
	}
}

// Regression (5.0.12 review): a RestoreObject between a move marking the
// record cold and removing the hot file took the hot file as newer data and
// dropped the cold copy; the move then removed the hot file.
func TestRestoreDuringAMoveKeepsTheObject(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "obj", []byte("the only copy"), time.Now().Add(-48*time.Hour).Unix())
	restored := make(chan error, 1)
	r.mgr.beforeColdRemove = func() {
		r.mgr.beforeColdRemove = nil
		go func() { restored <- r.mgr.MigrateToHot("b", "obj") }()
		time.Sleep(50 * time.Millisecond)
	}
	if err := r.mgr.migrateToCol("b", "obj"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if err := <-restored; err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := r.read(t, r.hot, "obj"); string(got) != "the only copy" || tierOf(t, r.store, "obj") == "cold" {
		t.Fatalf("after restore: hot %q, tier %q", got, tierOf(t, r.store, "obj"))
	}
}

// A record left marked cold with its data back in the hot tier (a rewrite of
// a cold object) is marked hot by the scan, so it can move again.
func TestScanRepairsAColdRecordWhoseDataIsHot(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "obj", []byte("rewritten"), time.Now().Unix())
	r.store.SetObjectTier("b", "obj", "cold")
	r.mgr.scan()
	if got := tierOf(t, r.store, "obj"); got != "hot" {
		t.Fatalf("tier %q, want hot", got)
	}
	// A real cold object is left alone.
	r.putHot(t, "c", []byte("cold one"), time.Now().Add(-48*time.Hour).Unix())
	if err := r.mgr.migrateToCol("b", "c"); err != nil {
		t.Fatal(err)
	}
	r.mgr.scan()
	if got := tierOf(t, r.store, "c"); got != "cold" {
		t.Fatalf("a cold object was marked %q", got)
	}
}
