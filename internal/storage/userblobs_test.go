package storage

import (
	"bytes"
	"compress/gzip"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func zstdded(t *testing.T, b []byte) []byte {
	t.Helper()
	enc, _ := zstd.NewWriter(nil)
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

// In per-bucket mode a bucket that has not opted in stores the owner's bytes as
// they are, and the read path unwrapped any blob that began with a zstd or gzip
// magic, meant for objects written before 4.4.70. A user's own .gz or .zst file
// came back decompressed: different bytes, a different length, a checksum the
// client rejects. Every object must come back exactly as it went in, with and
// without the compression layer on top, under a compressed-type name and under
// a neutral one.
func TestPerBucketReturnsCompressedUserFilesUnchanged(t *testing.T) {
	text := []byte("hello world, this is the original text\n")
	bodies := map[string][]byte{
		"archive.gz":  gzipped(t, text),
		"archive.zst": zstdded(t, text),
		"gz-data.bin": gzipped(t, text),
		"zs-data.bin": zstdded(t, text),
		"notes.txt":   text,
	}
	for _, compress := range []bool{false, true} {
		fs, _ := NewFileSystem(t.TempDir())
		pe, _ := NewPerBucketEngine(fs, nil)
		mgr := newMgr(t)
		pe.SetManager(mgr)
		var e Engine = pe
		if compress {
			e = NewCompressedEngine(pe)
		}
		for _, b := range []string{"plain", "sealed"} {
			fs.CreateBucketDir(b)
		}
		mgr.EnableBucket("sealed")
		for _, b := range []string{"plain", "sealed"} {
			for key, body := range bodies {
				if _, _, err := e.PutObject(b, key, bytes.NewReader(body), int64(len(body))); err != nil {
					t.Fatal(err)
				}
				if got := getPlain(t, e, b, key); !bytes.Equal(got, body) {
					t.Errorf("compression=%v %s/%s: stored %d bytes, read back %d different bytes", compress, b, key, len(body), len(got))
				}
			}
		}
	}
}

// Before 4.4.70 the compressor wrapped the per-bucket engine, so an object in a
// bucket that opted in is compress(seal(plaintext)) on disk, and one in a bucket
// that did not is compress(plaintext). Narrowing the unwrap must not lose either.
func TestPerBucketStillReadsTheOldCompressOutsideLayering(t *testing.T) {
	dir := t.TempDir()
	fs, _ := NewFileSystem(dir)
	mgr := newMgr(t)
	old, _ := NewPerBucketEngine(NewCompressedEngine(fs), nil)
	old.SetManager(mgr)
	fs.CreateBucketDir("plain")
	fs.CreateBucketDir("sealed")
	mgr.EnableBucket("sealed")
	body := bytes.Repeat([]byte("compressible line of text\n"), 2000)
	for _, b := range []string{"plain", "sealed"} {
		if _, _, err := old.PutObject(b, "f.txt", bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
	// Today's stack: the compressor sits outside the per-bucket engine.
	cur, _ := NewPerBucketEngine(fs, nil)
	cur.SetManager(mgr)
	stack := NewCompressedEngine(cur)
	for _, b := range []string{"plain", "sealed"} {
		if got := getPlain(t, stack, b, "f.txt"); !bytes.Equal(got, body) {
			t.Errorf("%s: an object written under the old layering no longer reads (%d bytes)", b, len(got))
		}
	}
}

// A key reached another bucket's object: the engine checked only that the path
// stayed inside the data directory. It must stay inside its own bucket.
func TestObjectKeysCannotLeaveTheirBucket(t *testing.T) {
	dir := t.TempDir()
	fs, _ := NewFileSystem(dir)
	fs.CreateBucketDir("victim")
	fs.CreateBucketDir("src")
	orig := []byte("original")
	fs.PutObject("victim", "a.txt", bytes.NewReader(orig), int64(len(orig)))

	evil := []byte("PWNED")
	for _, key := range []string{"../victim/a.txt", "x/../../victim/a.txt", ".."} {
		fs.PutObject("src", key, bytes.NewReader(evil), int64(len(evil)))
	}
	fs.PutObjectVersion("src", "../../victim/a.txt", "v", bytes.NewReader(evil), int64(len(evil)))
	fs.PutObjectVersion("src", "k", "../../../victim/a.txt", bytes.NewReader(evil), int64(len(evil)))

	got, err := os.ReadFile(fs.ObjectPath("victim", "a.txt"))
	if err != nil || !bytes.Equal(got, orig) {
		t.Fatalf("another bucket's object was overwritten: %q %v", got, err)
	}
}
