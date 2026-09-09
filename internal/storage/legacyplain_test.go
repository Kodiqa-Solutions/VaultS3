package storage

import (
	"bytes"
	"io"
	"testing"
)

// A headerless blob is either a legacy global-key object or the plaintext of a
// bucket that never opted in, and nothing on disk tells them apart. Assuming
// legacy made every plaintext object unreadable the moment a legacy_key was
// configured: the write returned 200 and the read returned 404 (issue #53
// follow-up). The zero-byte case was already special-cased for this reason.
func TestPlaintextReadableWithLegacyKeyConfigured(t *testing.T) {
	legacy := make([]byte, 32)
	for i := range legacy {
		legacy[i] = byte(i + 1)
	}
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"short", []byte("hi")},
		{"normal", bytes.Repeat([]byte("plaintext in an opted-out bucket "), 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, err := NewFileSystem(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			pe, err := NewPerBucketEngine(fs, legacy)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.CreateBucketDir("plain"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := pe.PutObject("plain", "k", bytes.NewReader(tc.body), int64(len(tc.body))); err != nil {
				t.Fatal(err)
			}
			r, _, err := pe.GetObject("plain", "k")
			if err != nil {
				t.Fatalf("a plaintext object is unreadable with legacy_key set: %v", err)
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.body) {
				t.Fatalf("round trip returned %d bytes, want %d", len(got), len(tc.body))
			}
		})
	}
}

// The legacy key must still decrypt what it sealed, or configuring it would be
// pointless and old objects would be served as ciphertext.
func TestLegacyGlobalKeyObjectStillDecrypts(t *testing.T) {
	legacy := make([]byte, 32)
	for i := range legacy {
		legacy[i] = byte(i + 1)
	}
	fs, err := NewFileSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Seal with the plain encrypted engine, which is what an older server used.
	enc, err := NewEncryptedEngine(fs, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.CreateBucketDir("old"); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("sealed with the server-wide key "), 100)
	if _, _, err := enc.PutObject("old", "k", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}

	pe, err := NewPerBucketEngine(fs, legacy)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := pe.GetObject("old", "k")
	if err != nil {
		t.Fatalf("a legacy global-key object no longer decrypts: %v", err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, body) {
		t.Fatal("legacy object did not round trip")
	}
}
