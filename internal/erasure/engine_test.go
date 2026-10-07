package erasure

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// ecTestRig is a 4-disk erasure-coded engine where each shard lands on its own
// disk, so wiping a disk dir simulates losing exactly one shard.
//
// Layout for DataShards=2, ParityShards=2 (4 shards total, 4 backends):
//
//	disk0 (inner): shard-00 + meta.json
//	disk1:         shard-01
//	disk2:         shard-02
//	disk3:         shard-03
type ecTestRig struct {
	eng   *Engine
	store *metadata.Store
	disks []string // disks[i] holds shard i (disk0 also holds the meta)
}

func newECRig(t *testing.T) *ecTestRig {
	t.Helper()
	base := t.TempDir()

	disks := []string{
		filepath.Join(base, "disk0"),
		filepath.Join(base, "disk1"),
		filepath.Join(base, "disk2"),
		filepath.Join(base, "disk3"),
	}

	inner, err := storage.NewFileSystem(disks[0])
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}

	cfg := Config{
		DataShards:   2,
		ParityShards: 2,
		BlockSize:    1024, // small so our 8KB test object is erasure-coded
		DataDirs:     disks[1:],
	}
	eng, err := NewEngine(inner, cfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := eng.CreateBucketDir("b"); err != nil {
		t.Fatalf("CreateBucketDir: %v", err)
	}

	store, err := metadata.NewStore(filepath.Join(base, "meta.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.CreateBucket("b"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	return &ecTestRig{eng: eng, store: store, disks: disks}
}

// wipeDisk simulates a lost disk by deleting that backend's bucket directory.
func (r *ecTestRig) wipeDisk(t *testing.T, i int) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(r.disks[i], "b")); err != nil {
		t.Fatalf("wipe disk %d: %v", i, err)
	}
}

// put writes an object through the engine and records it in the metadata
// store, as the S3 handler does. The healer finds objects through the store.
func (r *ecTestRig) put(t *testing.T, key string, data []byte) {
	t.Helper()
	_, etag, err := r.eng.PutObject("b", key, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("PutObject %s: %v", key, err)
	}
	if err := r.store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: key, Size: int64(len(data)), ETag: etag}); err != nil {
		t.Fatalf("PutObjectMeta %s: %v", key, err)
	}
}

// shardFile is where shard i of key's current version lives on its disk.
func (r *ecTestRig) shardFile(t *testing.T, key string, i int) string {
	t.Helper()
	meta, err := r.eng.readShardMeta("b", key)
	if err != nil {
		t.Fatalf("readShardMeta %s: %v", key, err)
	}
	return filepath.Join(r.disks[i], "b", filepath.FromSlash(meta.shardPath(key, i)))
}

func (r *ecTestRig) get(t *testing.T, key string) ([]byte, error) {
	t.Helper()
	rc, _, err := r.eng.GetObject("b", key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// TestEngineErasureCodesLargeObject confirms a >BlockSize object is split into
// shards distributed across disks (not stored whole on the inner disk).
func TestEngineErasureCodesLargeObject(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)

	if _, _, err := r.eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Each of the 4 disks should hold its shard file.
	for i := 0; i < 4; i++ {
		p := r.shardFile(t, "obj", i)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected shard %d at %s: %v", i, p, err)
		}
	}

	got, err := r.get(t, "obj")
	if err != nil {
		t.Fatalf("GetObject (healthy): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("healthy read mismatch")
	}
}

// TestEngineReadsThroughLostDisks is the core fault-injection test: after losing
// `parity` disks, reads must still reconstruct the original bytes transparently.
func TestEngineReadsThroughLostDisks(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)
	if _, _, err := r.eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Lose 2 disks (== parity). Meta + shard0 + shard1 survive on disks 0,1.
	r.wipeDisk(t, 2)
	r.wipeDisk(t, 3)

	got, err := r.get(t, "obj")
	if err != nil {
		t.Fatalf("GetObject through 2 lost disks: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reconstructed data mismatch")
	}
}

// TestEngineFailsBeyondParity confirms losing more disks than parity surfaces an
// error instead of silently returning corrupt data.
func TestEngineFailsBeyondParity(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)
	if _, _, err := r.eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Lose 3 disks (> parity of 2). Only meta + shard0 remain.
	r.wipeDisk(t, 1)
	r.wipeDisk(t, 2)
	r.wipeDisk(t, 3)

	if _, err := r.get(t, "obj"); err == nil {
		t.Fatal("expected error reading object with 3 lost disks, got nil")
	}
}

// TestHealerRepairsDegradedObject drives the full heal path: lose disks, heal,
// and verify the shards are rewritten and the object is no longer degraded.
func TestHealerRepairsDegradedObject(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)
	r.put(t, "obj", data)

	healer := NewHealer(r.store, r.eng, 3600)
	if healer.Status().DegradedObjects != 0 {
		t.Fatal("object should start healthy")
	}

	// Lose 2 disks, then confirm the healer sees it as degraded.
	r.wipeDisk(t, 2)
	r.wipeDisk(t, 3)
	if !healer.isDegraded("b", "obj") {
		t.Fatal("expected object to be degraded after losing 2 disks")
	}

	res := healer.Heal("b", "")
	if res.Scanned != 1 || res.Repaired != 1 {
		t.Fatalf("Heal: got scanned=%d repaired=%d, want 1/1", res.Scanned, res.Repaired)
	}

	// Shard files should be rewritten to the previously-wiped disks.
	for _, i := range []int{2, 3} {
		p := r.shardFile(t, "obj", i)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected repaired shard %d at %s: %v", i, p, err)
		}
	}

	if healer.isDegraded("b", "obj") {
		t.Fatal("object still degraded after heal")
	}
	if st := healer.Status(); st.DegradedObjects != 0 || st.HealthyObjects != 1 {
		t.Fatalf("post-heal status: degraded=%d healthy=%d, want 0/1", st.DegradedObjects, st.HealthyObjects)
	}

	// And the data must still read back correctly.
	got, err := r.get(t, "obj")
	if err != nil {
		t.Fatalf("GetObject after heal: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("data mismatch after heal")
	}
}

// TestHealerLeavesHealthyObjectAlone confirms a heal pass on an intact object
// scans but does not "repair" it.
func TestHealerLeavesHealthyObjectAlone(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)
	r.put(t, "obj", data)

	healer := NewHealer(r.store, r.eng, 3600)
	res := healer.Heal("b", "")
	if res.Scanned != 1 || res.Repaired != 0 {
		t.Fatalf("Heal on healthy object: got scanned=%d repaired=%d, want 1/0", res.Scanned, res.Repaired)
	}
}

// TestHealerPrefixScope confirms the prefix filter narrows the heal scope.
func TestHealerPrefixScope(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)
	for _, key := range []string{"logs/a", "images/b"} {
		r.put(t, key, data)
	}

	healer := NewHealer(r.store, r.eng, 3600)
	res := healer.Heal("b", "logs/")
	if res.Scanned != 1 {
		t.Fatalf("prefix-scoped heal scanned %d objects, want 1", res.Scanned)
	}
}
