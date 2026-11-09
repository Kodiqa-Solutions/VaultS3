package tiering

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

type tierRig struct {
	mgr   *Manager
	store *metadata.Store
	hot   storage.Engine
	cold  storage.Engine
}

func newTierRig(t *testing.T, migrateAfterDays int) *tierRig {
	t.Helper()
	base := t.TempDir()

	hot, err := storage.NewFileSystem(filepath.Join(base, "hot"))
	if err != nil {
		t.Fatalf("hot fs: %v", err)
	}
	cold, err := storage.NewFileSystem(filepath.Join(base, "cold"))
	if err != nil {
		t.Fatalf("cold fs: %v", err)
	}
	store, err := metadata.NewStore(filepath.Join(base, "meta.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if err := hot.CreateBucketDir("b"); err != nil {
		t.Fatalf("create hot bucket: %v", err)
	}
	if err := store.CreateBucket("b"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	return &tierRig{
		mgr:   NewManager(store, hot, cold, migrateAfterDays, 3600),
		store: store,
		hot:   hot,
		cold:  cold,
	}
}

// putHot writes an object to the hot engine and records its metadata with the
// given last-modified time (unix seconds).
func (r *tierRig) putHot(t *testing.T, key string, data []byte, lastModified int64) {
	t.Helper()
	if _, _, err := r.hot.PutObject("b", key, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("put hot %s: %v", key, err)
	}
	if err := r.store.PutObjectMeta(metadata.ObjectMeta{
		Bucket:       "b",
		Key:          key,
		Size:         int64(len(data)),
		LastModified: lastModified,
		Tier:         "hot",
	}); err != nil {
		t.Fatalf("put meta %s: %v", key, err)
	}
}

// read returns the full contents of an object from the given engine.
func (r *tierRig) read(t *testing.T, eng storage.Engine, key string) []byte {
	t.Helper()
	rc, _, err := eng.GetObject("b", key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return b
}

func tierOf(t *testing.T, store *metadata.Store, key string) string {
	t.Helper()
	m, err := store.GetObjectMeta("b", key)
	if err != nil {
		t.Fatalf("get meta %s: %v", key, err)
	}
	return m.Tier
}

// TestManualMigrateToCold moves an object hot→cold: it must land in cold,
// disappear from hot, and have its tier flipped to "cold".
func TestManualMigrateToCold(t *testing.T) {
	r := newTierRig(t, 30)
	data := []byte("cold-me please, I am old data")
	r.putHot(t, "obj", data, time.Now().Unix())

	if err := r.mgr.migrateToCol("b", "obj"); err != nil {
		t.Fatalf("migrate to cold: %v", err)
	}

	if r.hot.ObjectExists("b", "obj") {
		t.Fatal("object still present on hot tier after cold migration")
	}
	if !r.cold.ObjectExists("b", "obj") {
		t.Fatal("object missing from cold tier after migration")
	}
	if got := tierOf(t, r.store, "obj"); got != "cold" {
		t.Fatalf("tier = %q, want cold", got)
	}
	if got := r.read(t, r.cold, "obj"); !bytes.Equal(got, data) {
		t.Fatal("cold copy is not byte-identical to the original")
	}
}

// TestScanMigratesStaleObjects: the background scan migrates objects whose last
// access is older than the threshold, and leaves fresh ones on hot.
func TestScanMigratesStaleObjects(t *testing.T) {
	r := newTierRig(t, 30) // migrate after 30 days

	stale := []byte("stale object")
	fresh := []byte("fresh object")
	r.putHot(t, "stale", stale, time.Now().Add(-60*24*time.Hour).Unix()) // 60 days old
	r.putHot(t, "fresh", fresh, time.Now().Unix())                       // brand new

	r.mgr.scan()

	if got := tierOf(t, r.store, "stale"); got != "cold" {
		t.Fatalf("stale object tier = %q, want cold", got)
	}
	if !r.cold.ObjectExists("b", "stale") || r.hot.ObjectExists("b", "stale") {
		t.Fatal("stale object not moved to cold by scan")
	}

	if got := tierOf(t, r.store, "fresh"); got != "hot" {
		t.Fatalf("fresh object tier = %q, want hot", got)
	}
	if !r.hot.ObjectExists("b", "fresh") {
		t.Fatal("fresh object should remain on hot")
	}
}

// TestTransparentReadAndPromotion: reading a cold object returns the right bytes
// and (per the README claim) promotes it back to hot, re-checking tier first.
func TestTransparentReadAndPromotion(t *testing.T) {
	r := newTierRig(t, 30)
	data := []byte("promote me on access")
	r.putHot(t, "obj", data, time.Now().Unix())

	// Push it to cold first.
	if err := r.mgr.migrateToCol("b", "obj"); err != nil {
		t.Fatalf("migrate to cold: %v", err)
	}
	if got := tierOf(t, r.store, "obj"); got != "cold" {
		t.Fatalf("precondition: tier = %q, want cold", got)
	}

	// Transparent read must return the correct bytes regardless of tier.
	rc, _, err := r.mgr.GetObject("b", "obj")
	if err != nil {
		t.Fatalf("transparent GetObject: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("transparent read returned wrong bytes")
	}

	// The read triggers an async promotion back to hot.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tierOf(t, r.store, "obj") == "hot" && r.hot.ObjectExists("b", "obj") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := tierOf(t, r.store, "obj"); got != "hot" {
		t.Fatalf("object not promoted back to hot after access: tier = %q", got)
	}
	if !r.hot.ObjectExists("b", "obj") {
		t.Fatal("promoted object missing from hot tier")
	}
	if got := r.read(t, r.hot, "obj"); !bytes.Equal(got, data) {
		t.Fatal("promoted hot copy is not byte-identical")
	}
}

// TestMigrateToHotRoundTrip: explicit cold→hot migration deletes the cold copy
// and preserves the bytes exactly.
func TestMigrateToHotRoundTrip(t *testing.T) {
	r := newTierRig(t, 30)
	data := []byte("round trip data")
	r.putHot(t, "obj", data, time.Now().Unix())

	if err := r.mgr.ManualMigrate("b", "obj", "cold"); err != nil {
		t.Fatalf("to cold: %v", err)
	}
	if err := r.mgr.MigrateToHot("b", "obj"); err != nil {
		t.Fatalf("to hot: %v", err)
	}

	if r.cold.ObjectExists("b", "obj") {
		t.Fatal("cold copy not removed after promotion")
	}
	if got := tierOf(t, r.store, "obj"); got != "hot" {
		t.Fatalf("tier = %q, want hot", got)
	}
	if got := r.read(t, r.hot, "obj"); !bytes.Equal(got, data) {
		t.Fatal("data corrupted across cold→hot round trip")
	}
}
