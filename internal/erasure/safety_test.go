package erasure

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// writeOldLayout stores an object exactly as the code before generations did:
// shards directly under .ec/{key}/ and a meta.json with no generation or
// checksums. It is how objects already on disk look after an upgrade.
func writeOldLayout(t *testing.T, e *Engine, bucket, key string, data []byte) {
	t.Helper()
	shards, err := e.encoder.Encode(data)
	if err != nil {
		t.Fatal(err)
	}
	meta := &ShardMeta{
		OriginalSize: int64(len(data)), DataShards: e.cfg.DataShards, ParityShards: e.cfg.ParityShards,
		BlockSize: e.cfg.BlockSize, ShardSizes: make([]int64, len(shards)), CreatedAt: time.Now().UTC(),
	}
	for i, sh := range shards {
		meta.ShardSizes[i] = int64(len(sh))
		if _, _, err := e.backendFor(i).PutObject(bucket, shardKey(key, i), bytes.NewReader(sh), int64(len(sh))); err != nil {
			t.Fatal(err)
		}
	}
	mb, _ := meta.Marshal()
	if strings.Contains(string(mb), "generation") || strings.Contains(string(mb), "crc") {
		t.Fatalf("old-layout meta must not carry the new fields: %s", mb)
	}
	if _, _, err := e.backendFor(0).PutObject(bucket, metaKey(key), bytes.NewReader(mb), int64(len(mb))); err != nil {
		t.Fatal(err)
	}
}

// Objects written before generations existed must keep reading, healthy and
// degraded, and an overwrite must retire their shards.
func TestOldLayoutObjectsStillRead(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)
	writeOldLayout(t, r.eng, "b", "old", data)

	if got, err := r.get(t, "old"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("healthy old-layout read failed: %v", err)
	}
	r.wipeDisk(t, 1) // data shard 1 gone (disk1 holds no meta)
	if got, err := r.get(t, "old"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("degraded old-layout read failed: %v", err)
	}

	// Heal understands the old layout and rewrites the shard where it was.
	if err := r.store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "old", Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.disks[1], "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := NewHealer(r.store, r.eng, 3600).Heal("b", "")
	if res.Repaired != 1 {
		t.Fatalf("heal of an old-layout object repaired %d, want 1", res.Repaired)
	}
	if _, err := os.Stat(filepath.Join(r.disks[1], "b", ".ec", "old", shardName(1))); err != nil {
		t.Fatalf("healed old-layout shard not at its old path: %v", err)
	}

	// Overwriting moves the object to a generation and removes the old shards.
	data2 := makeData(9000)
	r.put(t, "old", data2)
	if got, err := r.get(t, "old"); err != nil || !bytes.Equal(got, data2) {
		t.Fatalf("read after overwrite: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := os.Stat(filepath.Join(r.disks[i], "b", ".ec", "old", shardName(i))); !os.IsNotExist(err) {
			t.Fatalf("old-layout shard %d survived the overwrite (err %v)", i, err)
		}
	}
}

// cutReader yields its data and then fails, like a client that drops the
// connection part way through an upload.
type cutReader struct {
	data []byte
	off  int
}

func (c *cutReader) Read(p []byte) (int, error) {
	if c.off >= len(c.data) {
		return 0, errors.New("connection reset")
	}
	n := copy(p, c.data[c.off:])
	c.off += n
	return n, nil
}

// A failed overwrite must leave the previous version whole. The streaming path
// wrote each new data shard over the old shard path, so an upload that broke
// off after shard 0 left a mix of two objects. The buffered path wrote
// meta.json first.
func TestFailedOverwriteKeepsPreviousVersion(t *testing.T) {
	for _, declared := range []int64{8192, -1} {
		t.Run(fmt.Sprintf("declared=%d", declared), func(t *testing.T) {
			r := newECRig(t)
			old := makeData(8192)
			r.put(t, "obj", old)

			newer := bytes.Repeat([]byte{0xEE}, 8192)
			src := &cutReader{data: newer[:6000]}
			if _, _, err := r.eng.PutObject("b", "obj", src, declared); err == nil {
				t.Fatal("PutObject with a failing source returned nil")
			}
			got, err := r.get(t, "obj")
			if err != nil {
				t.Fatalf("previous version unreadable after a failed overwrite: %v", err)
			}
			if !bytes.Equal(got, old) {
				t.Fatal("previous version was corrupted by a failed overwrite")
			}
		})
	}
}

// A read in progress when the object is overwritten must return one version,
// never a mix: the old code opened data shards lazily and the overwrite
// replaced them in place underneath it.
func TestReadDuringOverwriteSeesOneVersion(t *testing.T) {
	r := newECRig(t)
	old := makeData(8192)
	r.put(t, "obj", old)

	rc, _, err := r.eng.GetObject("b", "obj")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	first := make([]byte, 10)
	if _, err := io.ReadFull(rc, first); err != nil {
		t.Fatal(err)
	}

	r.put(t, "obj", bytes.Repeat([]byte{0xEE}, 8192))

	rest, err := io.ReadAll(rc)
	got := append(first, rest...)
	if err == nil && !bytes.Equal(got, old) {
		t.Fatal("a read that started before an overwrite returned a mix of two versions")
	}
}

// Replacing a coded object with a plain one (small, or in a bucket that opted
// out) must serve the new bytes. meta.json used to stay behind and GetObject,
// which checks it first, kept returning the old object.
func TestPlainOverwriteRetiresCodedVersion(t *testing.T) {
	r := newECRig(t)
	r.put(t, "obj", makeData(8192))
	small := []byte("small replacement")
	r.put(t, "obj", small)
	if got, err := r.get(t, "obj"); err != nil || !bytes.Equal(got, small) {
		t.Fatalf("after a small overwrite got %d bytes (err %v), want the %d new bytes", len(got), err, len(small))
	}
	if r.eng.backendFor(0).ObjectExists("b", metaKey("obj")) {
		t.Fatal("meta.json of the coded version survived the overwrite")
	}

	// And the other way: a coded write removes the stale plain file, so a
	// later delete does not bring the small object back.
	big := makeData(8192)
	r.put(t, "obj", big)
	if r.eng.inner.ObjectExists("b", "obj") {
		t.Fatal("plain file survived a coded overwrite")
	}
	if err := r.eng.DeleteObject("b", "obj"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.get(t, "obj"); err == nil {
		t.Fatal("deleted object still reads")
	}
}

func TestOptedOutOverwriteRetiresCodedVersion(t *testing.T) {
	coded := map[string]bool{"coded": true}
	e, _ := newPerBucketEngine(t, coded)
	put(t, e, "coded", "k", 8192)
	coded["coded"] = false // the bucket opts out, then the key is rewritten
	want := put(t, e, "coded", "k", 8192)
	if got := readBack(t, e, "coded", "k"); !bytes.Equal(got, want) {
		t.Fatal("bucket that opted out still serves the old erasure-coded version")
	}
}

// failingDeletes refuses to delete one path, so a test can see whether the
// engine reports it.
type failingDeletes struct {
	storage.Engine
	fail string
}

func (f *failingDeletes) DeleteObject(bucket, key string) error {
	if strings.HasSuffix(key, f.fail) {
		return errors.New("disk refused the delete")
	}
	return f.Engine.DeleteObject(bucket, key)
}

func TestDeleteObjectReportsShardFailures(t *testing.T) {
	fs, err := storage.NewFileSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inner := &failingDeletes{Engine: fs}
	e, err := NewEngine(inner, config.ErasureConfig{DataShards: 2, ParityShards: 1, BlockSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	e.CreateBucketDir("b")
	if _, _, err := e.PutObject("b", "k", bytes.NewReader(makeData(4096)), 4096); err != nil {
		t.Fatal(err)
	}

	inner.fail = shardName(1)
	if err := e.DeleteObject("b", "k"); err == nil {
		t.Fatal("DeleteObject returned nil although a shard could not be deleted")
	}

	// A meta.json that cannot be deleted leaves the object readable, so that
	// must be reported too.
	if _, _, err := e.PutObject("b", "k2", bytes.NewReader(makeData(4096)), 4096); err != nil {
		t.Fatal(err)
	}
	inner.fail = "meta.json"
	if err := e.DeleteObject("b", "k2"); err == nil {
		t.Fatal("DeleteObject returned nil although meta.json could not be deleted")
	}

	// Deleting something that is not there is not an error.
	inner.fail = "nothing"
	if err := e.DeleteObject("b", "never-written"); err != nil {
		t.Fatalf("delete of a missing key: %v", err)
	}
}

// Listing with maxKeys 0 means everything. maxKeys+100 turned it into 100 and
// then the "re-apply maxKeys" step returned nothing, so a backup over an
// erasure engine copied zero objects and reported success.
func TestListObjectsUnlimitedAndPastShardFiles(t *testing.T) {
	r := newECRig(t)
	for i := 0; i < 30; i++ { // each coded object adds five files under .ec/
		r.put(t, fmt.Sprintf("coded-%02d", i), makeData(2048))
	}
	for i := 0; i < 5; i++ {
		r.put(t, fmt.Sprintf("plain-%02d", i), []byte("x"))
	}
	all, truncated, err := r.eng.ListObjects("b", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 || truncated {
		t.Fatalf("unlimited listing returned %d objects (truncated %v), want the 5 plain ones", len(all), truncated)
	}
	// A page of 3 must not end the listing just because the first inner page
	// was all shard files.
	var keys []string
	after := ""
	for {
		page, more, err := r.eng.ListObjects("b", "", after, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page {
			keys = append(keys, o.Key)
		}
		if !more {
			break
		}
		after = page[len(page)-1].Key
	}
	if len(keys) != 5 {
		t.Fatalf("paged listing returned %v, want 5 keys", keys)
	}
}

// Heal must cover every coded object, not just the first 10,000 files under
// .ec/. The page size is lowered so several pages are needed.
func TestHealerPagesThroughEveryObject(t *testing.T) {
	orig := healPageSize
	healPageSize = 2
	t.Cleanup(func() { healPageSize = orig })

	r := newECRig(t)
	for i := 0; i < 7; i++ {
		r.put(t, fmt.Sprintf("k%d", i), makeData(2048))
	}
	r.put(t, "plain", []byte("not coded"))
	res := NewHealer(r.store, r.eng, 3600).Heal("b", "")
	if res.Scanned != 7 {
		t.Fatalf("heal scanned %d objects, want 7", res.Scanned)
	}
}

// A shard that is present but corrupt must not be served. With checksums in
// meta.json the read rebuilds the stripe from parity and heal rewrites it.
func TestCorruptShardIsDetectedAndHealed(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)
	r.put(t, "obj", data)

	p := r.shardFile(t, "obj", 0)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw[17] ^= 0xFF // same length, wrong content
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := r.get(t, "obj")
	if err != nil {
		t.Fatalf("read with a corrupt shard: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("a corrupt data shard was served to the client")
	}
	// A Range read inside the corrupt stripe is recovered too.
	rc, _, err := r.eng.GetObject("b", "obj")
	if err != nil {
		t.Fatal(err)
	}
	rc.Seek(10, io.SeekStart)
	part := make([]byte, 20)
	io.ReadFull(rc, part)
	rc.Close()
	if !bytes.Equal(part, data[10:30]) {
		t.Fatal("range read over a corrupt shard returned bad bytes")
	}

	res := NewHealer(r.store, r.eng, 3600).Heal("b", "")
	if res.Repaired != 1 {
		t.Fatalf("heal repaired %d objects, want the corrupt one", res.Repaired)
	}
	fixed, _ := os.ReadFile(p)
	if bytes.Equal(fixed, raw) {
		t.Fatal("heal left the corrupt shard in place")
	}
}
