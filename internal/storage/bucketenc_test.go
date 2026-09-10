package storage

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
)

func newMgr(t *testing.T) *bucketcrypto.Manager {
	t.Helper()
	mk := make([]byte, 32)
	rand.Read(mk)
	kek, err := bucketcrypto.NewKEK(mk)
	if err != nil {
		t.Fatal(err)
	}
	return bucketcrypto.NewManager(kek, bucketcrypto.NewMemKeyStore())
}

func getPlain(t *testing.T, e Engine, bucket, key string) []byte {
	t.Helper()
	r, _, err := e.GetObject(bucket, key)
	if err != nil {
		t.Fatalf("GetObject %s/%s: %v", bucket, key, err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return b
}

func TestPerBucketEngine_EncryptedVsOptOut(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	mgr := newMgr(t)
	pe, _ := NewPerBucketEngine(fs, nil)
	pe.SetManager(mgr)

	fs.CreateBucketDir("tenant-a")
	fs.CreateBucketDir("tenant-b")
	mgr.EnableBucket("tenant-a") // a opts in; b stays plaintext

	secret := []byte("super secret tenant A records")
	if _, _, err := pe.PutObject("tenant-a", "f.txt", bytes.NewReader(secret), int64(len(secret))); err != nil {
		t.Fatal(err)
	}
	if got := getPlain(t, pe, "tenant-a", "f.txt"); !bytes.Equal(got, secret) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
	// On disk it is encrypted (streaming-format header, plaintext absent).
	raw, _ := os.ReadFile(fs.ObjectPath("tenant-a", "f.txt"))
	if !bytes.HasPrefix(raw, []byte(streamMagic)) {
		t.Fatalf("encrypted bucket object should carry the streaming header on disk, got %q", raw[:min(4, len(raw))])
	}
	if bytes.Contains(raw, secret) {
		t.Fatal("plaintext leaked to disk")
	}

	// Opt-out bucket: stored as plaintext, read back unchanged.
	pub := []byte("public tenant B data")
	if _, _, err := pe.PutObject("tenant-b", "p.txt", bytes.NewReader(pub), int64(len(pub))); err != nil {
		t.Fatal(err)
	}
	rawB, _ := os.ReadFile(fs.ObjectPath("tenant-b", "p.txt"))
	if !bytes.Equal(rawB, pub) {
		t.Fatal("opt-out bucket should store plaintext on disk")
	}
	if got := getPlain(t, pe, "tenant-b", "p.txt"); !bytes.Equal(got, pub) {
		t.Fatalf("opt-out round-trip mismatch: %q", got)
	}
}

func TestPerBucketEngine_LegacyGlobalKeyRead(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	fs.CreateBucketDir("legacy")

	// Write an object the old way (server-wide global key, no per-bucket header).
	legacyKey := make([]byte, 32)
	rand.Read(legacyKey)
	leg, err := NewEncryptedEngine(fs, legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("written under the old global key")
	if _, _, err := leg.PutObject("legacy", "old.txt", bytes.NewReader(plain), int64(len(plain))); err != nil {
		t.Fatal(err)
	}

	// A per-bucket engine configured with the legacy key still reads it.
	pe, _ := NewPerBucketEngine(fs, legacyKey)
	pe.SetManager(newMgr(t)) // manager present, but "legacy" bucket never opted in
	if got := getPlain(t, pe, "legacy", "old.txt"); !bytes.Equal(got, plain) {
		t.Fatalf("legacy object should decrypt via the legacy key: %q", got)
	}
}

func TestPerBucketEngine_NoManagerIsPlaintext(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	fs.CreateBucketDir("b")
	pe, _ := NewPerBucketEngine(fs, nil) // no manager set
	data := []byte("no manager -> passthrough")
	pe.PutObject("b", "k", bytes.NewReader(data), int64(len(data)))
	raw, _ := os.ReadFile(fs.ObjectPath("b", "k"))
	if !bytes.Equal(raw, data) {
		t.Fatal("without a manager, objects must be stored as plaintext")
	}
}

// An opted-out bucket's objects are plaintext, so a GET must hand back the
// underlying reader rather than a copy of the object: no buffering, and Seek
// reaches the stored bytes directly (issue #53).
func TestPerBucketEngine_OptOutReadPassesThrough(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	mgr := newMgr(t)
	pe, _ := NewPerBucketEngine(fs, nil)
	pe.SetManager(mgr)
	fs.CreateBucketDir("plain")

	data := bytes.Repeat([]byte("0123456789"), 1000)
	if _, _, err := pe.PutObject("plain", "k", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	r, size, err := pe.GetObject("plain", "k")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", size, len(data))
	}
	if _, buffered := r.(*bytesReadSeekCloser); buffered {
		t.Fatal("plaintext object was read into memory instead of passed through")
	}
	if _, err := r.Seek(5000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 10)
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data[5000:5010]) {
		t.Fatalf("range read = %q, want %q", got, data[5000:5010])
	}
}

// VS3X blobs (the pre-streaming per-bucket format) still decrypt through the
// whole-object path.
func TestPerBucketEngine_WholeObjectFormatStillReads(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	mgr := newMgr(t)
	pe, _ := NewPerBucketEngine(fs, nil)
	pe.SetManager(mgr)
	fs.CreateBucketDir("enc")
	mgr.EnableBucket("enc")

	plain := []byte("sealed as a single message before 4.4.53")
	blob, encrypted, err := mgr.Encrypt("enc", plain)
	if err != nil || !encrypted {
		t.Fatalf("Encrypt: %v (encrypted=%v)", err, encrypted)
	}
	if !bucketcrypto.HasHeader(blob) {
		t.Fatal("test expects a VS3X blob")
	}
	if _, _, err := fs.PutObject("enc", "old", bytes.NewReader(blob), int64(len(blob))); err != nil {
		t.Fatal(err)
	}
	if got := getPlain(t, pe, "enc", "old"); !bytes.Equal(got, plain) {
		t.Fatalf("VS3X round-trip mismatch: %q", got)
	}
}

// An SSE-C object is sealed by the S3 handler with the customer's key and lands
// in the engine as a VS3S blob whose key version is CustomerKeyVersion. In an
// opted-out bucket the per-bucket engine sees that blob directly, and must hand
// it back untouched rather than treat it as a version-0 server-wide object and
// try (and fail) to open it with the legacy key.
func TestPerBucketEngine_PassesThroughCustomerSealedBlob(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	fs.CreateBucketDir("optout")
	legacyKey := make([]byte, 32)
	rand.Read(legacyKey)
	pe, _ := NewPerBucketEngine(fs, legacyKey)
	pe.SetManager(newMgr(t)) // manager present, "optout" never opted in

	custKey := make([]byte, 32)
	rand.Read(custKey)
	plain := []byte("sealed above the engine with a key it does not hold")
	if _, _, err := SealStreamWithKey(custKey, bytes.NewReader(plain), int64(len(plain)),
		func(sealed io.Reader, storedSize int64) (int64, string, error) {
			return pe.PutObject("optout", "c.bin", sealed, storedSize)
		}); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(fs.ObjectPath("optout", "c.bin"))
	r, stored, err := pe.GetObject("optout", "c.bin")
	if err != nil {
		t.Fatalf("customer-sealed blob should pass through, got: %v", err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, raw) || stored != int64(len(raw)) {
		t.Fatal("engine altered a blob it does not hold the key for")
	}

	// And the handler-side opener streams it with the customer key.
	src, stored, _ := pe.GetObject("optout", "c.bin")
	out, size, ok, err := OpenStreamWithKey(custKey, src, stored)
	if err != nil || !ok {
		t.Fatalf("OpenStreamWithKey: ok=%v err=%v", ok, err)
	}
	defer out.Close()
	if _, buffered := out.(*bytesReadSeekCloser); buffered {
		t.Fatal("customer stream was buffered instead of streamed")
	}
	if b, _ := io.ReadAll(out); !bytes.Equal(b, plain) || size != int64(len(plain)) {
		t.Fatalf("round-trip mismatch: %q (size %d)", b, size)
	}
}
