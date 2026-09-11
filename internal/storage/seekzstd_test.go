package storage

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Compressed objects were the last read path that still expanded the whole
// object into memory to serve a Range request, so twenty concurrent Range
// requests on one object were twenty full plaintext copies (the shape of issue
// #49, left over for compression after #53). New writes use the zstd seekable
// format and a Range read costs one frame; objects written as a single frame,
// or with gzip, still read, and a Range on them now discards rather than
// materialises.

// withFrameSize shrinks the compression frame for a test so multi-frame objects
// cost bytes rather than megabytes.
func withFrameSize(t *testing.T, n int) {
	t.Helper()
	prev := defaultCompressFrame
	defaultCompressFrame = n
	t.Cleanup(func() { defaultCompressFrame = prev })
}

// seekablePayload is compressible but not uniform, so a wrong frame or offset
// shows up as different bytes rather than the same repeated line.
func seekablePayload(n int) []byte {
	out := make([]byte, 0, n+64)
	for i := 0; len(out) < n; i++ {
		out = append(out, []byte("line ")...)
		out = binary.LittleEndian.AppendUint32(out, uint32(i))
		out = append(out, []byte(" of the seekable payload, padded to compress well\n")...)
	}
	return out[:n]
}

// A new object is stored in the seekable format, and a Range read into a later
// frame must return the right bytes without decompressing the object to get
// there.
func TestSeekableRangeReadCostsOneFrame(t *testing.T) {
	withFrameSize(t, 64<<10)
	c, dir := newCompressedFS(t)
	payload := seekablePayload(8 << 20) // 128 frames
	if _, _, err := c.PutObject("b", "big.txt", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "b", "big.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(raw[len(raw)-4:]); got != seekableMagic {
		t.Fatalf("stored blob ends with %#x, want the seekable footer magic %#x", got, seekableMagic)
	}
	if frames := binary.LittleEndian.Uint32(raw[len(raw)-9:]); frames != 128 {
		t.Fatalf("seek table records %d frames, want 128", frames)
	}

	rc, size, err := c.GetObject("b", "big.txt")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer rc.Close()
	if size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}
	if _, ok := rc.(*bytesReadSeekCloser); ok {
		t.Fatal("Range path materialised the whole object into a bytes reader")
	}

	// Cross a frame boundary so the read has to load two frames.
	const off = 100*(64<<10) - 500
	window := make([]byte, 1000)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := rc.Seek(off, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, err := io.ReadFull(rc, window); err != nil {
		t.Fatalf("ReadFull after seek: %v", err)
	}
	runtime.ReadMemStats(&after)

	if !bytes.Equal(window, payload[off:off+1000]) {
		t.Fatal("range read returned the wrong bytes")
	}
	// Two frames plus decoder scratch is well under a megabyte; the object is 8 MiB.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > uint64(len(payload))/4 {
		t.Fatalf("range read allocated %d bytes for an %d byte object; it is decompressing the whole thing", alloc, len(payload))
	}

	// And a backward seek after that still lands on the right frame.
	if _, err := rc.Seek(12345, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rc, window); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(window, payload[12345:12345+1000]) {
		t.Fatal("backward seek returned the wrong bytes")
	}
}

// Objects written before the seekable format are one zstd frame, and older
// ones still are gzip. Both must read, and a Range on them must go through the
// discard path rather than the removed materialisation.
func TestLegacySingleFrameAndGzipStillRangeRead(t *testing.T) {
	c, dir := newCompressedFS(t)
	payload := seekablePayload(300 << 10)

	single := zstdEncoder.EncodeAll(payload, nil)
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	gw.Write(payload)
	gw.Close()
	for name, blob := range map[string][]byte{"single.txt": single, "old.txt": gz.Bytes()} {
		if err := os.WriteFile(filepath.Join(dir, "b", name), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"single.txt", "old.txt"} {
		t.Run(name, func(t *testing.T) {
			rc, size, err := c.GetObject("b", name)
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}
			defer rc.Close()
			if size != int64(len(payload)) {
				t.Fatalf("size = %d, want %d", size, len(payload))
			}
			if _, ok := rc.(*decompressStream); !ok {
				t.Fatalf("reader is %T, want *decompressStream", rc)
			}

			const off = 200<<10 + 17
			window := make([]byte, 4096)
			if _, err := rc.Seek(off, io.SeekStart); err != nil {
				t.Fatalf("Seek: %v", err)
			}
			if _, err := io.ReadFull(rc, window); err != nil {
				t.Fatalf("ReadFull after seek: %v", err)
			}
			if !bytes.Equal(window, payload[off:off+4096]) {
				t.Fatal("range read on a legacy blob returned the wrong bytes")
			}
			// A second, earlier range restarts the decoder.
			if _, err := rc.Seek(1000, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(rc, window); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(window, payload[1000:1000+4096]) {
				t.Fatal("backward range read on a legacy blob returned the wrong bytes")
			}
			// And the whole object still streams from the start.
			if _, err := rc.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("legacy blob did not round trip")
			}
		})
	}
}

// Compression runs under per-bucket encryption, so the seek table is read
// through the VS3S reader's Seek and each compressed frame is fetched from the
// encrypted chunks that hold it. Frames and chunks are shrunk to different sizes
// so a frame straddles chunk boundaries.
func TestSeekableRangeReadThroughPerBucketEncryption(t *testing.T) {
	withFrameSize(t, 4096)
	withChunkSize(t, 1000)
	fs, err := NewFileSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pb, err := NewPerBucketEngine(fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr := newMgr(t)
	pb.SetManager(mgr)
	if err := mgr.EnableBucket("b"); err != nil {
		t.Fatal(err)
	}
	eng := NewCompressedEngine(pb)
	payload := seekablePayload(20*4096 + 77)
	mustPut(t, eng, "b", "layered.txt", payload)

	rc, size, err := eng.GetObject("b", "layered.txt")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer rc.Close()
	if size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}
	if _, ok := rc.(*seekableReader); !ok {
		t.Fatalf("reader is %T, want *seekableReader over the encrypted stream", rc)
	}
	for _, off := range []int64{0, 4095, 4096, 7*4096 + 3, 20 * 4096} {
		if _, err := rc.Seek(off, io.SeekStart); err != nil {
			t.Fatalf("Seek(%d): %v", off, err)
		}
		want := payload[off:]
		if len(want) > 5000 {
			want = want[:5000]
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(rc, got); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("range read at %d through both layers returned the wrong bytes", off)
		}
	}
	// The stored blob must be ciphertext, not a readable seek table.
	raw, err := os.ReadFile(filepath.Join(fs.DataDir(), "b", "layered.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(raw[len(raw)-4:]) == seekableMagic {
		t.Fatal("seek table is visible on disk: compression ran outside encryption")
	}
}
