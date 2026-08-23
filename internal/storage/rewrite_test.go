package storage

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

// Rewriting live data has one absolute requirement: the bytes must survive it.
// Everything else about this migration is secondary to that.
func TestRewriteLegacyObjectPreservesBytesExactly(t *testing.T) {
	for _, size := range []int{0, 1, 4096, 3*1024*1024 + 517} {
		fs, _ := NewFileSystem(t.TempDir())
		fs.CreateBucketDir("b")

		key := make([]byte, 32)
		rand.Read(key)
		pe, err := NewPerBucketEngine(fs, key)
		if err != nil {
			t.Fatal(err)
		}
		mgr := newMgr(t)
		pe.SetManager(mgr)
		if err := mgr.EnableBucket("b"); err != nil {
			t.Fatalf("size %d: opt the bucket into encryption: %v", size, err)
		}

		plain := make([]byte, size)
		rand.Read(plain)

		// Seal it the old way, whole-object, exactly as a pre-4.4.53 write did.
		sealed, err := pe.seal("b", plain)
		if err != nil {
			t.Fatalf("size %d: seal: %v", size, err)
		}
		if _, _, err := fs.PutObject("b", "old.bin", bytes.NewReader(sealed), int64(len(sealed))); err != nil {
			t.Fatalf("size %d: store legacy blob: %v", size, err)
		}

		legacy, err := pe.IsLegacyObject("b", "old.bin")
		if err != nil {
			t.Fatalf("size %d: IsLegacyObject: %v", size, err)
		}
		if !legacy {
			t.Fatalf("size %d: a whole-object blob was not detected as legacy", size)
		}

		if err := pe.RewriteObject("b", "old.bin"); err != nil {
			t.Fatalf("size %d: RewriteObject: %v", size, err)
		}

		// It must now be current, and still be the same bytes.
		legacy, err = pe.IsLegacyObject("b", "old.bin")
		if err != nil {
			t.Fatalf("size %d: IsLegacyObject after rewrite: %v", size, err)
		}
		if legacy {
			t.Errorf("size %d: still legacy after a rewrite", size)
		}
		if got := getPlain(t, pe, "b", "old.bin"); !bytes.Equal(got, plain) {
			t.Fatalf("size %d: REWRITE CORRUPTED THE OBJECT (%d bytes back, wanted %d)",
				size, len(got), len(plain))
		}
	}
}

// Rewriting something already in the current format must be a no-op that still
// returns the same bytes, because a migration will be run more than once.
func TestRewriteIsIdempotent(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	fs.CreateBucketDir("b")
	key := make([]byte, 32)
	rand.Read(key)
	pe, _ := NewPerBucketEngine(fs, key)
	mgr := newMgr(t)
	pe.SetManager(mgr)
	if err := mgr.EnableBucket("b"); err != nil {
		t.Fatalf("opt the bucket into encryption: %v", err)
	}

	plain := []byte("already in the current format")
	if _, _, err := pe.PutObject("b", "k.bin", bytes.NewReader(plain), int64(len(plain))); err != nil {
		t.Fatal(err)
	}
	if legacy, _ := pe.IsLegacyObject("b", "k.bin"); legacy {
		t.Fatal("a freshly written object was reported as legacy")
	}
	for i := 0; i < 3; i++ {
		if err := pe.RewriteObject("b", "k.bin"); err != nil {
			t.Fatalf("rewrite %d: %v", i, err)
		}
		if got := getPlain(t, pe, "b", "k.bin"); !bytes.Equal(got, plain) {
			t.Fatalf("rewrite %d changed the object: %q", i, got)
		}
	}
}

// The single-key engine has the same legacy problem and the same fix.
func TestRewriteWorksOnTheSingleKeyEngine(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	fs.CreateBucketDir("b")
	key := make([]byte, 32)
	rand.Read(key)
	e, err := NewEncryptedEngine(fs, key)
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, 2*1024*1024)
	rand.Read(plain)
	if _, _, err := e.PutObject("b", "k.bin", bytes.NewReader(plain), int64(len(plain))); err != nil {
		t.Fatal(err)
	}
	if err := e.RewriteObject("b", "k.bin"); err != nil {
		t.Fatalf("RewriteObject: %v", err)
	}
	rc, _, err := e.GetObject("b", "k.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, plain) {
		t.Fatal("rewrite on the single-key engine changed the object")
	}
}

// A zero-byte object carries no header and no ciphertext. It used to fall
// through to the whole-object decrypt path, which rejected it as "encrypted data
// too short", so every empty object in a bucket that had not opted in became
// unreadable as soon as a legacy key was configured. Empty objects are ordinary
// in S3, so this made them a silent read failure rather than an edge case.
func TestZeroByteObjectRoundTripsUnderPerBucketEncryption(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	fs.CreateBucketDir("b")
	key := make([]byte, 32)
	rand.Read(key)
	pe, err := NewPerBucketEngine(fs, key)
	if err != nil {
		t.Fatal(err)
	}
	pe.SetManager(newMgr(t))

	if _, _, err := pe.PutObject("b", "empty.bin", bytes.NewReader(nil), 0); err != nil {
		t.Fatalf("writing a 0-byte object: %v", err)
	}
	rc, n, err := pe.GetObject("b", "empty.bin")
	if err != nil {
		t.Fatalf("a 0-byte object could not be read back: %v", err)
	}
	defer rc.Close()
	if n != 0 {
		t.Errorf("size %d, want 0", n)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("read %d bytes from an empty object", len(got))
	}
}

// A bucket that never opted into encryption stores plaintext, and plaintext has
// no streaming magic either. Detecting "legacy" from the missing magic alone
// would flag every plaintext object in the deployment and rewrite it for
// nothing, which on a large bucket is a lot of pointless I/O against live data.
func TestPlaintextObjectIsNotReportedAsLegacy(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	fs.CreateBucketDir("plain")
	key := make([]byte, 32)
	rand.Read(key)
	pe, err := NewPerBucketEngine(fs, key)
	if err != nil {
		t.Fatal(err)
	}
	pe.SetManager(newMgr(t)) // manager present, but "plain" never opts in

	body := []byte("stored as plaintext because this bucket opted out")
	if _, _, err := pe.PutObject("plain", "k.txt", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	legacy, err := pe.IsLegacyObject("plain", "k.txt")
	if err != nil {
		t.Fatalf("IsLegacyObject: %v", err)
	}
	if legacy {
		t.Error("a plaintext object in a bucket that never opted in was reported as legacy," +
			" so a migration would rewrite every unencrypted object for nothing")
	}
}
