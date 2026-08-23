package erasure

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"testing"
)

// The streaming write must be byte-identical to the buffering one it replaces,
// including the ETag, or existing objects and new ones stop agreeing.
func TestStreamingWriteMatchesBufferedWrite(t *testing.T) {
	sizes := []int{
		8192,
		parityStripeBytes,
		parityStripeBytes + 1,
		3*parityStripeBytes + 4321, // not a multiple of the stripe or the shard count
	}
	for _, size := range sizes {
		data := makeData(size)
		want := md5.Sum(data)

		r := newECRig(t)
		_, etag, err := r.eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("size %d: PutObject: %v", size, err)
		}
		if etag != hex.EncodeToString(want[:]) {
			t.Errorf("size %d: etag %s, want %s", size, etag, hex.EncodeToString(want[:]))
		}
		got, err := r.get(t, "obj")
		if err != nil {
			t.Fatalf("size %d: read back: %v", size, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("size %d: read back does not match what was written", size)
		}
	}
}

// The decisive one. A wrong parity shard is invisible while the data shards are
// intact, because the read path never touches parity on the healthy path. It only
// surfaces when recovery is needed, which is the moment it matters most.
func TestStreamingWriteProducesUsableParity(t *testing.T) {
	for _, size := range []int{8192, 2*parityStripeBytes + 77} {
		r := newECRig(t)
		data := makeData(size)
		if _, _, err := r.eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("size %d: PutObject: %v", size, err)
		}

		// disk1 holds data shard 1 of 2, so the read must rebuild it from parity
		// that the streaming writer generated.
		r.wipeDisk(t, 1)

		got, err := r.get(t, "obj")
		if err != nil {
			t.Fatalf("size %d: degraded read after a streaming write: %v", size, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("size %d: recovery from streaming-written parity returned wrong bytes,"+
				" so the parity shards do not match the data", size)
		}
	}
}

// A short body must fail rather than silently store a truncated object padded
// with zeros, which is what writing shard-by-shard would otherwise do.
func TestStreamingWriteRejectsAShortBody(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)
	// Declare more than the reader will produce.
	_, _, err := r.eng.PutObject("b", "short", bytes.NewReader(data), int64(len(data))+4096)
	if err == nil {
		t.Fatal("a body shorter than its declared size was accepted, so the object" +
			" would be stored zero-padded and silently corrupt")
	}
}

// An unknown length (a chunked upload arrives with -1) has no shard geometry to
// stream into, so it must fall back to the buffering path rather than fail.
func TestUnknownLengthStillStores(t *testing.T) {
	r := newECRig(t)
	data := makeData(8192)
	_, _, err := r.eng.PutObject("b", "chunked", bytes.NewReader(data), -1)
	if err != nil {
		t.Fatalf("unknown-length PutObject: %v", err)
	}
	got, err := r.get(t, "chunked")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("unknown-length write did not round-trip")
	}
}

// The point of the change: what a write allocates must not grow with the object.
// The old path called io.ReadAll and then encoded on top of that, so allocation
// tracked object size and concurrent large PUTs OOMed the container. Streaming
// costs a fixed number of stripe buffers instead, so quadrupling the object must
// not meaningfully change what it allocates.
func TestStreamingWriteAllocationDoesNotGrowWithObjectSize(t *testing.T) {
	alloc := map[int]uint64{}
	for _, size := range []int{4 * parityStripeBytes, 16 * parityStripeBytes} {
		r := newECRig(t)
		data := makeData(size)
		var before, after testingMemStats
		readMem(&before)
		if _, _, err := r.eng.PutObject("b", fmt.Sprintf("obj%d", size), bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		readMem(&after)
		alloc[size] = after.TotalAlloc - before.TotalAlloc
		t.Logf("object %d bytes: write allocated %d bytes", size, alloc[size])
	}

	small, large := alloc[4*parityStripeBytes], alloc[16*parityStripeBytes]
	// A 4x larger object on the old path allocated roughly 4x more. Allow a
	// generous margin and still catch anything that scales.
	if large > small*2 {
		t.Errorf("a 4x larger object allocated %d bytes versus %d: the write path still"+
			" scales with object size", large, small)
	}
}
