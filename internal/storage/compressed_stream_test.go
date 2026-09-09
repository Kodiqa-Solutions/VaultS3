package storage

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func newCompressedFS(t *testing.T) (*CompressedEngine, string) {
	t.Helper()
	dir := t.TempDir()
	fs, err := NewFileSystem(dir)
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}
	c := NewCompressedEngine(fs)
	if err := c.CreateBucketDir("b"); err != nil {
		t.Fatalf("CreateBucketDir: %v", err)
	}
	return c, dir
}

// Each frame must still record the zstd frame content size. The seekable reader
// takes sizes from the table, but a decoder that ignores the table (and the
// legacy single-frame path) depends on it to stream instead of materialising the
// whole object, so losing it would silently undo the work from issue #38.
func TestStreamCompressRecordsFrameContentSize(t *testing.T) {
	c, dir := newCompressedFS(t)
	payload := bytes.Repeat([]byte("compress me please "), 50000) // ~950 KB

	n, _, err := c.PutObject("b", "obj", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("reported size = %d, want the plaintext length %d", n, len(payload))
	}

	raw, err := os.ReadFile(filepath.Join(dir, "b", "obj"))
	if err != nil {
		t.Fatalf("read stored blob: %v", err)
	}
	var hdr zstd.Header
	if err := hdr.Decode(raw); err != nil {
		t.Fatalf("stored blob is not a zstd frame: %v", err)
	}
	if !hdr.HasFCS {
		t.Fatal("frame content size missing: reads would fall back to buffering the whole object")
	}
	if hdr.FrameContentSize != uint64(len(payload)) {
		t.Errorf("frame content size = %d, want %d", hdr.FrameContentSize, len(payload))
	}
}

func TestStreamCompressRoundTrip(t *testing.T) {
	c, _ := newCompressedFS(t)
	for _, size := range []int{0, 1, 4096, 1 << 20, 5 << 20} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			payload := bytes.Repeat([]byte("abcdefgh"), size/8+1)[:size]
			want := md5.Sum(payload)

			_, etag, err := c.PutObject("b", "rt", bytes.NewReader(payload), int64(len(payload)))
			if err != nil {
				t.Fatalf("PutObject: %v", err)
			}
			if etag != fmt.Sprintf("\"%x\"", want) {
				t.Errorf("etag = %s, want the md5 of the plaintext", etag)
			}

			rc, gotSize, err := c.GetObject("b", "rt")
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}
			defer rc.Close()
			if gotSize != int64(size) {
				t.Errorf("size = %d, want %d", gotSize, size)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("round trip corrupted %d bytes", size)
			}
		})
	}
}

// An upload of unknown length used to buffer so the frame content size could be
// recorded. The seek table now carries the sizes, so it streams like any other
// write and must still produce frames with a content size and read back whole.
func TestUnknownLengthStreamsSeekable(t *testing.T) {
	c, dir := newCompressedFS(t)
	payload := bytes.Repeat([]byte("unknown length "), 10000)

	// -1 is what a chunked upload reports.
	if _, _, err := c.PutObject("b", "chunked", bytes.NewReader(payload), -1); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "b", "chunked"))
	if err != nil {
		t.Fatalf("read stored blob: %v", err)
	}
	var hdr zstd.Header
	if err := hdr.Decode(raw); err != nil {
		t.Fatalf("not a zstd frame: %v", err)
	}
	if !hdr.HasFCS {
		t.Error("unknown-length write dropped the frame content size")
	}

	rc, _, err := c.GetObject("b", "chunked")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, payload) {
		t.Error("unknown-length write corrupted the object")
	}
}

// A failure partway through the body must not leave a readable object: the
// error has to reach the inner engine so it abandons its partial write.
func TestStreamCompressPropagatesReadFailure(t *testing.T) {
	c, _ := newCompressedFS(t)
	src := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("x"), 4096)), failingReader{})

	if _, _, err := c.PutObject("b", "broken", src, 8192); err == nil {
		t.Fatal("a failed body read reported success")
	}
	if c.ObjectExists("b", "broken") {
		t.Error("a failed upload left a readable object behind")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// Versioned writes take the same path and must behave identically.
func TestStreamCompressVersionRoundTrip(t *testing.T) {
	c, _ := newCompressedFS(t)
	payload := bytes.Repeat([]byte("versioned payload "), 20000)

	if _, _, err := c.PutObjectVersion("b", "v", "ver1", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("PutObjectVersion: %v", err)
	}
	rc, size, err := c.GetObjectVersion("b", "v", "ver1")
	if err != nil {
		t.Fatalf("GetObjectVersion: %v", err)
	}
	defer rc.Close()
	if size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", size, len(payload))
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, payload) {
		t.Error("versioned round trip corrupted the object")
	}
}
