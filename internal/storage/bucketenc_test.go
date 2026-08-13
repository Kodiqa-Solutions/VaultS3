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
