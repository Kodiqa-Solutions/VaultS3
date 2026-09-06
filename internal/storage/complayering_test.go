package storage

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Compression and encryption used to be layered the wrong way round: the
// compressor wrapped the encryptor, so it was handed ciphertext. Ciphertext does
// not compress, so with encryption on the compressor saved nothing at all while
// still costing the CPU to attempt it. The order is now compress-then-encrypt.
//
// Two things have to hold. New writes must actually get smaller, and objects
// written under the old layering must still read, because rewriting every
// encrypted object is not something an upgrade can ask for.

// current is the shipping stack: compression on the outside, so it sees plaintext.
func layerKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func currentStack(t *testing.T, dir string) (Engine, *FileSystem) {
	t.Helper()
	fs, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := NewEncryptedEngine(fs, layerKey())
	if err != nil {
		t.Fatal(err)
	}
	return NewCompressedEngine(enc), fs
}

// legacy is the pre-4.4.70 stack: encryption on the outside, compressor fed
// ciphertext. Used only to produce old-format objects to read back.
func legacyStack(t *testing.T, dir string) (Engine, *FileSystem) {
	t.Helper()
	fs, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := NewEncryptedEngine(NewCompressedEngine(fs), layerKey())
	if err != nil {
		t.Fatal(err)
	}
	return enc, fs
}

func compressiblePayload() []byte {
	return bytes.Repeat([]byte("the same line over and over again, endlessly repeated\n"), 4000)
}

func mustPut(t *testing.T, e Engine, bucket, key string, body []byte) {
	t.Helper()
	if err := e.CreateBucketDir(bucket); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	if _, _, err := e.PutObject(bucket, key, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("put: %v", err)
	}
}

func mustGet(t *testing.T, e Engine, bucket, key string) []byte {
	t.Helper()
	r, _, err := e.GetObject(bucket, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

// The point of the whole change: with encryption on, a compressible object must
// actually shrink on disk.
func TestCompressionActuallyCompressesUnderEncryption(t *testing.T) {
	dir := t.TempDir()
	engine, fs := currentStack(t, dir)
	body := compressiblePayload()
	mustPut(t, engine, "b", "repetitive.txt", body)

	onDisk, err := fs.ObjectSize("b", "repetitive.txt")
	if err != nil {
		t.Fatal(err)
	}
	ratio := float64(len(body)) / float64(onDisk)
	if ratio < 5 {
		t.Fatalf("stored %d bytes for a %d byte payload (%.2fx). Encryption is still wrapping "+
			"compression, so the compressor is seeing ciphertext", onDisk, len(body), ratio)
	}
	if got := mustGet(t, engine, "b", "repetitive.txt"); !bytes.Equal(got, body) {
		t.Fatal("round trip did not return the original bytes")
	}
}

// An object written under the old layering must still read through the new
// stack. This is the compatibility guarantee that makes the swap deployable.
func TestLegacyCompressOutsideEncryptStillReads(t *testing.T) {
	dir := t.TempDir()
	body := compressiblePayload()

	legacy, _ := legacyStack(t, dir)
	mustPut(t, legacy, "b", "old.txt", body)

	// A fresh process, new layering, same data directory.
	current, _ := currentStack(t, dir)
	if got := mustGet(t, current, "b", "old.txt"); !bytes.Equal(got, body) {
		t.Fatalf("an object written under the old layering no longer reads: got %d bytes, want %d",
			len(got), len(body))
	}
}

// Both layouts have to coexist, since an upgraded server keeps writing into
// buckets that already hold old objects.
func TestBothLayoutsCoexistInOneBucket(t *testing.T) {
	dir := t.TempDir()
	body := compressiblePayload()

	legacy, _ := legacyStack(t, dir)
	mustPut(t, legacy, "b", "old.txt", body)

	current, _ := currentStack(t, dir)
	mustPut(t, current, "b", "new.txt", body)

	for _, key := range []string{"old.txt", "new.txt"} {
		if got := mustGet(t, current, "b", key); !bytes.Equal(got, body) {
			t.Fatalf("%s did not round trip", key)
		}
	}
}

// Encryption without compression must be untouched by the change, and so must an
// object whose plaintext is not compressible.
func TestEncryptionWithoutCompressionUnaffected(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := NewEncryptedEngine(fs, layerKey())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(strings.Repeat("x", 100))
	mustPut(t, enc, "b", "plain.txt", body)
	if got := mustGet(t, enc, "b", "plain.txt"); !bytes.Equal(got, body) {
		t.Fatal("encryption-only round trip broke")
	}
}

// A zero-byte object is the shape that has broken this layer before.
func TestEmptyObjectRoundTripsUnderBothLayouts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T, string) (Engine, *FileSystem)
	}{{"current", currentStack}, {"legacy-written", legacyStack}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writer, _ := tc.build(t, dir)
			mustPut(t, writer, "b", "empty.txt", nil)
			reader, _ := currentStack(t, dir)
			if got := mustGet(t, reader, "b", "empty.txt"); len(got) != 0 {
				t.Fatalf("empty object came back as %d bytes", len(got))
			}
		})
	}
}

// Range reads must survive the extra layer, since that is what a video player or
// a parquet reader does on every request.
func TestRangeReadThroughTheNewLayering(t *testing.T) {
	dir := t.TempDir()
	engine, _ := currentStack(t, dir)
	body := compressiblePayload()
	mustPut(t, engine, "b", "seek.txt", body)

	r, _, err := engine.GetObject("b", "seek.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Seek(1000, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	got := make([]byte, 64)
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if !bytes.Equal(got, body[1000:1064]) {
		t.Fatalf("range read returned the wrong bytes")
	}
}

func TestVersionedObjectRoundTripsUnderTheNewLayering(t *testing.T) {
	dir := t.TempDir()
	engine, _ := currentStack(t, dir)
	body := compressiblePayload()
	if err := engine.CreateBucketDir("b"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.PutObjectVersion("b", "v.txt", "ver1", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("put version: %v", err)
	}
	r, _, err := engine.GetObjectVersion("b", "v.txt", "ver1")
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, body) {
		t.Fatal("versioned round trip broke")
	}
	_ = filepath.Join
}

// The layering change touched all three encryption engines, but the tests above
// only exercise SSE-S3. SSE-KMS and per-bucket encryption got the same call to
// openSealed with no coverage at all, which is how a legacy object becomes
// permanently unreadable for exactly the operators who encrypted it.

func localKMS(t *testing.T) *KMS {
	t.Helper()
	return NewKMS(KMSConfig{
		Provider: "local",
		KeyName:  "test-key",
		LocalKey: "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
	})
}

func TestLegacyLayeringStillReadsUnderSSEKMS(t *testing.T) {
	dir := t.TempDir()
	body := compressiblePayload()

	// Old layering: the compressor sat under the encryptor.
	fsOld, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	oldEng, err := NewKMSEncryptedEngine(NewCompressedEngine(fsOld), localKMS(t), "test-key")
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, oldEng, "b", "old.txt", body)

	// New layering, same data directory.
	fsNew, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	newInner, err := NewKMSEncryptedEngine(fsNew, localKMS(t), "test-key")
	if err != nil {
		t.Fatal(err)
	}
	newEng := NewCompressedEngine(newInner)

	if got := mustGet(t, newEng, "b", "old.txt"); !bytes.Equal(got, body) {
		t.Fatalf("an SSE-KMS object written under the old layering no longer reads: got %d bytes, want %d",
			len(got), len(body))
	}
	// And a new write into the same bucket must compress and read back.
	mustPut(t, newEng, "b", "new.txt", body)
	if got := mustGet(t, newEng, "b", "new.txt"); !bytes.Equal(got, body) {
		t.Fatal("new SSE-KMS object did not round trip")
	}
	onDisk, err := fsNew.ObjectSize("b", "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if float64(len(body))/float64(onDisk) < 5 {
		t.Fatalf("SSE-KMS write stored %d bytes for %d, so compression is still seeing ciphertext", onDisk, len(body))
	}
}

func TestLegacyLayeringStillReadsUnderPerBucketEncryption(t *testing.T) {
	dir := t.TempDir()
	body := compressiblePayload()

	// A server-wide key with no per-bucket manager is the "legacy key" path,
	// which is what an object sealed before per-bucket mode reads through.
	fsOld, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := NewEncryptedEngine(NewCompressedEngine(fsOld), layerKey())
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, inner, "b", "old.txt", body)

	fsNew, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := NewPerBucketEngine(fsNew, layerKey())
	if err != nil {
		t.Fatal(err)
	}
	newEng := NewCompressedEngine(pb)
	if got := mustGet(t, newEng, "b", "old.txt"); !bytes.Equal(got, body) {
		t.Fatalf("a per-bucket-engine read of an object written under the old layering broke: got %d bytes, want %d",
			len(got), len(body))
	}
}

// A bucket that never opted into per-bucket encryption stores plaintext, so the
// compressor is the only layer. That must keep working too.
func TestPerBucketOptedOutBucketStillCompresses(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := NewPerBucketEngine(fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewCompressedEngine(pb)
	body := compressiblePayload()
	mustPut(t, eng, "b", "plain.txt", body)
	if got := mustGet(t, eng, "b", "plain.txt"); !bytes.Equal(got, body) {
		t.Fatal("opted-out bucket did not round trip")
	}
	onDisk, err := fs.ObjectSize("b", "plain.txt")
	if err != nil {
		t.Fatal(err)
	}
	if float64(len(body))/float64(onDisk) < 5 {
		t.Fatalf("an unencrypted bucket stored %d bytes for %d, compression is not running", onDisk, len(body))
	}
}
