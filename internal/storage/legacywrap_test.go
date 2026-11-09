package storage

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Regression (5.0.14 security review): with a legacy key configured, a
// tenant's own gzip file in a bucket that never opted in was served
// decompressed: other bytes than were stored.
func TestAPlainGzipIsServedAsStoredWithALegacyKey(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	pe, _ := NewPerBucketEngine(fs, bytes.Repeat([]byte{7}, 32))
	_ = fs.CreateBucketDir("plain")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("inner content"))
	_ = zw.Close()
	up := gz.Bytes()
	_, _, _ = pe.PutObject("plain", "data.bin", bytes.NewReader(up), int64(len(up)))
	r, n, err := pe.GetObject("plain", "data.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, up) || n != int64(len(up)) {
		t.Fatalf("stored %d bytes, served %d (%q)", len(up), n, got)
	}
}

// Regression (5.0.14 security review): a zstd file with no content size in
// its header was decoded in full on every GET to measure it (1 GB for this
// one). It is checked against its stored size instead and served as stored.
func TestAZstdBombIsNotDecodedOnEveryGet(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	pe, _ := NewPerBucketEngine(fs, nil)
	_ = fs.CreateBucketDir("plain")
	var zb bytes.Buffer
	enc, _ := zstd.NewWriter(&zb, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithSingleSegment(false))
	zero := make([]byte, 1<<20)
	for i := 0; i < 1000; i++ {
		_, _ = enc.Write(zero)
	}
	_ = enc.Close()
	up := zb.Bytes()
	_, _, _ = pe.PutObject("plain", "data.bin", bytes.NewReader(up), int64(len(up)))
	start := time.Now()
	r, n, err := pe.GetObject("plain", "data.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	took := time.Since(start)
	if !bytes.Equal(got, up) || n != int64(len(up)) {
		t.Fatalf("served %d bytes, stored %d", n, len(up))
	}
	if took > 400*time.Millisecond {
		t.Errorf("a GET of a %d byte zstd file took %v: it was decoded far past its size", len(up), took)
	}
}
