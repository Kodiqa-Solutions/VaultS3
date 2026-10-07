package storage

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"
)

// The seekable writer streams, so an object larger than the legacy decode cap
// must store and read back instead of failing with a 500. A real 1 GiB write is
// impractical here, so the cap is lowered to show the write path ignores it.
func TestCompressedWriteIgnoresLegacySizeCap(t *testing.T) {
	orig := maxCompressedSize
	maxCompressedSize = 64 << 10
	t.Cleanup(func() { maxCompressedSize = orig })
	origFrame := defaultCompressFrame
	defaultCompressFrame = 16 << 10
	t.Cleanup(func() { defaultCompressFrame = origFrame })

	fs, err := NewFileSystem(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	c := NewCompressedEngine(fs)
	c.CreateBucketDir("b")
	plain := bytes.Repeat([]byte("compressible line of text\n"), 20000) // about 520 KiB
	for _, size := range []int64{int64(len(plain)), -1} {
		n, _, err := c.PutObject("b", "big.txt", bytes.NewReader(plain), size)
		if err != nil {
			t.Fatalf("PutObject size=%d over the cap: %v", size, err)
		}
		if n != int64(len(plain)) {
			t.Fatalf("PutObject wrote %d, want %d", n, len(plain))
		}
		rc, got, err := c.GetObject("b", "big.txt")
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		if got != int64(len(plain)) || !bytes.Equal(body, plain) {
			t.Fatalf("read back %d bytes (size %d), want %d", len(body), got, len(plain))
		}
	}
}
