package erasure

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// countingEngine wraps a storage engine and counts how many bytes readers actually
// pull off it, so a test can prove that producing the first byte of a GET does not
// read the whole object (issue #38).
type countingEngine struct {
	storage.Engine
	mu    sync.Mutex
	bytes int64
}

func (c *countingEngine) GetObject(bucket, key string) (storage.ReadSeekCloser, int64, error) {
	rc, n, err := c.Engine.GetObject(bucket, key)
	if err != nil {
		return nil, 0, err
	}
	return &countingReader{ReadSeekCloser: rc, owner: c}, n, nil
}

func (c *countingEngine) read() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

func (c *countingEngine) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes = 0
}

type countingReader struct {
	storage.ReadSeekCloser
	owner *countingEngine
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadSeekCloser.Read(p)
	r.owner.mu.Lock()
	r.owner.bytes += int64(n)
	r.owner.mu.Unlock()
	return n, err
}

// newCountingECEngine builds a single-backend erasure engine over a counting
// storage engine (all shards land on it, so every read is observed).
func newCountingECEngine(t *testing.T) (*Engine, *countingEngine) {
	t.Helper()
	fs, err := storage.NewFileSystem(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}
	counter := &countingEngine{Engine: fs}
	eng, err := NewEngine(counter, Config{DataShards: 4, ParityShards: 2, BlockSize: 1024})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := eng.CreateBucketDir("b"); err != nil {
		t.Fatalf("CreateBucketDir: %v", err)
	}
	return eng, counter
}

// TestErasureFirstByteDoesNotReadWholeObject is the core issue #38 guard: the bytes
// read from storage to produce byte 1 must stay roughly constant as the object grows,
// instead of scaling with object size (the old read-all-shards-then-reconstruct path
// read 100% of the object before emitting anything).
func TestErasureFirstByteDoesNotReadWholeObject(t *testing.T) {
	for _, size := range []int{256 * 1024, 1024 * 1024, 4 * 1024 * 1024, 32 * 1024 * 1024} {
		eng, counter := newCountingECEngine(t)
		data := makeData(size)
		if _, _, err := eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("PutObject: %v", err)
		}

		counter.reset()
		rc, n, err := eng.GetObject("b", "obj")
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		if n != int64(size) {
			t.Fatalf("reported size %d, want %d", n, size)
		}
		one := make([]byte, 1)
		if _, err := io.ReadFull(rc, one); err != nil {
			t.Fatalf("read first byte: %v", err)
		}
		firstByteCost := counter.read()
		rc.Close()

		if one[0] != data[0] {
			t.Fatalf("first byte mismatch: got %d want %d", one[0], data[0])
		}
		// A materializing read costs >= size. Streaming costs at most one
		// checksum stripe of the first data shard (it is verified before any of
		// it is served) plus the small meta file, whatever the object's size.
		if firstByteCost > crcStripeBytes+16*1024 {
			t.Fatalf("size=%d: reading the first byte pulled %d bytes from storage, more than one stripe: the read is materializing, not streaming",
				size, firstByteCost)
		}
		if size >= 32*1024*1024 && firstByteCost > int64(size)/16 {
			t.Fatalf("size=%d: first byte cost %d does not stay flat as the object grows", size, firstByteCost)
		}
		t.Logf("size=%7d bytes-read-for-first-byte=%6d", size, firstByteCost)
	}
}

// TestErasureStreamRoundTrip checks the streamed bytes are byte-identical to what
// was written, across sizes that do and do not divide evenly into shards (the last
// data shard is zero-padded, which must not leak into the object).
func TestErasureStreamRoundTrip(t *testing.T) {
	for _, size := range []int{4096, 100000, 1024 * 1024, 1024*1024 + 7} {
		eng, _ := newCountingECEngine(t)
		data := makeData(size)
		if _, _, err := eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		rc, n, err := eng.GetObject("b", "obj")
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		if n != int64(size) {
			t.Fatalf("size %d, want %d", n, size)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("size=%d: round-trip mismatch (got %d bytes)", size, len(got))
		}
	}
}

// TestErasureStreamSeek covers Range and partNumber reads, which Seek into the
// object — including offsets that land exactly on and across shard boundaries.
func TestErasureStreamSeek(t *testing.T) {
	eng, _ := newCountingECEngine(t)
	const size = 1024 * 1024 // 4 data shards => 256 KiB per shard
	data := makeData(size)
	if _, _, err := eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	rc, _, err := eng.GetObject("b", "obj")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer rc.Close()

	perShard := size / 4
	offsets := []int{
		0,
		1,
		perShard - 1,   // last byte of shard 0
		perShard,       // first byte of shard 1
		perShard + 1,   // across the boundary
		2*perShard - 5, // spans shards 1 -> 2
		3 * perShard,
		size - 10,
	}
	for _, off := range offsets {
		if _, err := rc.Seek(int64(off), io.SeekStart); err != nil {
			t.Fatalf("Seek(%d): %v", off, err)
		}
		length := 4096
		if off+length > size {
			length = size - off
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("ReadFull at %d: %v", off, err)
		}
		if !bytes.Equal(buf, data[off:off+length]) {
			t.Fatalf("range read at offset %d mismatch", off)
		}
	}

	// Seek relative to the end, like a suffix Range request.
	if _, err := rc.Seek(-100, io.SeekEnd); err != nil {
		t.Fatalf("Seek(end): %v", err)
	}
	tail, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if !bytes.Equal(tail, data[size-100:]) {
		t.Fatalf("suffix read mismatch: got %d bytes", len(tail))
	}

	// Rewind and re-read the whole object: it must still be byte-identical.
	if _, err := rc.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek(0): %v", err)
	}
	full, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("full re-read: %v", err)
	}
	if !bytes.Equal(full, data) {
		t.Fatal("full re-read mismatch")
	}
}

// TestErasureStreamFallsBackWhenDataShardMissing verifies the degraded path still
// returns correct bytes when a data shard is gone before the read starts (parity
// reconstruction), matching the pre-streaming behaviour.
func TestErasureStreamFallsBackWhenDataShardMissing(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)
	if _, _, err := r.eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// disk1 holds data shard 1 (DataShards=2), so losing it forces parity recovery.
	r.wipeDisk(t, 1)

	got, err := r.get(t, "obj")
	if err != nil {
		t.Fatalf("GetObject with a missing data shard: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("degraded read mismatch")
	}
}

// TestErasureStreamRecoversMidStream is the harder degraded case: the stream starts
// healthy, then a data shard disappears before it is reached. The reader must switch
// to parity reconstruction and continue from the same offset without corrupting or
// truncating the object.
func TestErasureStreamRecoversMidStream(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192) // DataShards=2 => 4096 bytes per shard
	if _, _, err := r.eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	rc, size, err := r.eng.GetObject("b", "obj")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer rc.Close()
	if size != int64(len(data)) {
		t.Fatalf("size %d, want %d", size, len(data))
	}

	// Read the first 100 bytes from shard 0 while everything is healthy.
	head := make([]byte, 100)
	if _, err := io.ReadFull(rc, head); err != nil {
		t.Fatalf("read head: %v", err)
	}
	if !bytes.Equal(head, data[:100]) {
		t.Fatal("head mismatch")
	}

	// Now lose the disk holding data shard 1, which the stream has not reached yet.
	r.wipeDisk(t, 1)

	rest, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read rest after losing a data shard: %v", err)
	}
	if !bytes.Equal(append(head, rest...), data) {
		t.Fatalf("mid-stream recovery mismatch: got %d bytes total, want %d", len(head)+len(rest), len(data))
	}
}

// TestErasureSmallObjectUnaffected confirms objects below BlockSize (stored whole,
// not erasure-coded) still read back correctly through the new code path.
func TestErasureSmallObjectUnaffected(t *testing.T) {
	eng, _ := newCountingECEngine(t)
	data := []byte("small object stored without erasure coding")
	if _, _, err := eng.PutObject("b", "small", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	rc, n, err := eng.GetObject("b", "small")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer rc.Close()
	if n != int64(len(data)) {
		t.Fatalf("size %d, want %d", n, len(data))
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Fatalf("small object mismatch: %q", got)
	}
}

// TestErasureStreamConcurrentReads runs many simultaneous readers over one object,
// which is the real access pattern under load, to catch shared-state mistakes.
func TestErasureStreamConcurrentReads(t *testing.T) {
	eng, _ := newCountingECEngine(t)
	const size = 512 * 1024
	data := makeData(size)
	if _, _, err := eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rc, _, err := eng.GetObject("b", "obj")
			if err != nil {
				errs <- err
				return
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, data) {
				errs <- fmt.Errorf("reader %d: mismatch (%d bytes)", i, len(got))
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read: %v", err)
	}
}
