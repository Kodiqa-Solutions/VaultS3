package erasure

import (
	"bytes"
	"io"
	"testing"
)

// A degraded read must return exactly the same bytes as a healthy one. Streaming
// the recovery a stripe at a time changes when parity math runs, never what it
// produces, so this covers sizes that land on and across stripe and shard
// boundaries, plus one that is not a multiple of either.
func TestDegradedStreamReturnsIdenticalBytes(t *testing.T) {
	sizes := []int{
		8192,                    // small, single stripe
		degradedStripeBytes,     // exactly one stripe per shard
		degradedStripeBytes + 1, // one byte into a second stripe
		5*degradedStripeBytes + 12345,
	}
	for _, size := range sizes {
		r := newECRig(t)
		data := makeData(size)
		if _, _, err := r.eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("size %d: PutObject: %v", size, err)
		}

		// disk1 holds data shard 1 of 2, so losing it forces parity recovery.
		r.wipeDisk(t, 1)

		got, err := r.get(t, "obj")
		if err != nil {
			t.Fatalf("size %d: degraded GetObject: %v", size, err)
		}
		if len(got) != len(data) {
			t.Fatalf("size %d: degraded read returned %d bytes, want %d", size, len(got), len(data))
		}
		if !bytes.Equal(got, data) {
			for i := range got {
				if got[i] != data[i] {
					t.Fatalf("size %d: degraded read differs at byte %d", size, i)
				}
			}
		}
	}
}

// Reading in small chunks must not corrupt anything: each Read is capped at the
// current data shard and the current stripe, so the boundary handling is where a
// mistake would show up as shifted or duplicated bytes.
func TestDegradedStreamSmallReadsAndSeek(t *testing.T) {
	r := newECRig(t)
	size := 3*degradedStripeBytes + 777
	data := makeData(size)
	if _, _, err := r.eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	r.wipeDisk(t, 1)

	rc, n, err := r.eng.GetObject("b", "obj")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer rc.Close()
	if n != int64(size) {
		t.Fatalf("reported size %d, want %d", n, size)
	}

	// Read the whole object 7 bytes at a time.
	var out []byte
	buf := make([]byte, 7)
	for {
		k, err := rc.Read(buf)
		out = append(out, buf[:k]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("chunked read: %v", err)
		}
	}
	if !bytes.Equal(out, data) {
		t.Fatal("chunked degraded read does not match the original")
	}

	// Seek into a later stripe and read across the boundary.
	off := int64(degradedStripeBytes) - 3
	if _, err := rc.Seek(off, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	tail := make([]byte, 64)
	if _, err := io.ReadFull(rc, tail); err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if !bytes.Equal(tail, data[off:off+64]) {
		t.Fatal("bytes after a seek into a later stripe do not match")
	}
}

// The point of the change: a degraded read must not read the whole object before
// emitting byte one. The old path called reconstruct(), which read every shard in
// full and decoded the entire object first, which is issue #38's exact shape.
func TestDegradedFirstByteDoesNotReadWholeObject(t *testing.T) {
	// The claim is not just "less than the whole object", it is that the cost of
	// the first byte no longer grows with the object. Reading the same amount for
	// an 8 MiB and a 32 MiB object is what makes TTFB flat, which is issue #38.
	reads := map[int]int64{}
	for _, size := range []int{8 * degradedStripeBytes, 32 * degradedStripeBytes} {
		eng, counter := newCountingECEngine(t)
		data := makeData(size)
		if _, _, err := eng.PutObject("b", "obj", bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("size %d: PutObject: %v", size, err)
		}
		// Remove one data shard so the read must use parity.
		if err := eng.backendFor(1).DeleteObject("b", shardKey("obj", 1)); err != nil {
			t.Fatalf("size %d: delete shard: %v", size, err)
		}

		counter.reset()
		rc, _, err := eng.GetObject("b", "obj")
		if err != nil {
			t.Fatalf("size %d: GetObject: %v", size, err)
		}
		one := make([]byte, 1)
		if _, err := io.ReadFull(rc, one); err != nil {
			rc.Close()
			t.Fatalf("size %d: read first byte: %v", size, err)
		}
		if one[0] != data[0] {
			rc.Close()
			t.Fatalf("size %d: first byte is %d, want %d", size, one[0], data[0])
		}
		reads[size] = counter.read()
		rc.Close()
		t.Logf("object %d bytes: first byte cost %d bytes of storage reads", size, reads[size])

		if reads[size] >= int64(size) {
			t.Errorf("size %d: read %d bytes to produce byte 1, so the degraded path"+
				" is still materialising the whole object", size, reads[size])
		}
	}

	small, large := reads[8*degradedStripeBytes], reads[32*degradedStripeBytes]
	if large > small*2 {
		t.Errorf("first byte cost %d bytes for an 8 MiB object but %d for a 32 MiB one:"+
			" the cost still scales with object size", small, large)
	}
}
