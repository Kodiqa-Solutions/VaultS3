package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// withChunkSize shrinks the write chunk size for a test so multi-chunk objects
// cost bytes rather than megabytes.
func withChunkSize(t *testing.T, n int) {
	t.Helper()
	prev := defaultStreamChunk
	defaultStreamChunk = n
	t.Cleanup(func() { defaultStreamChunk = prev })
}

// sizesAroundChunk covers the boundaries the format's chunking can get wrong:
// empty, a single byte, either side of a chunk edge, and exact multiples (which
// are the case that needs the empty final chunk).
func sizesAroundChunk(chunk int) []int {
	return []int{0, 1, chunk - 1, chunk, chunk + 1, 2 * chunk, 2*chunk + 7, 5*chunk + 3}
}

func TestStreamRoundTripAtChunkBoundaries(t *testing.T) {
	const chunk = 64
	key := testKey(t)

	for _, size := range sizesAroundChunk(chunk) {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			plain := make([]byte, size)
			rand.Read(plain)

			var sealed bytes.Buffer
			n, err := sealStream(&sealed, bytes.NewReader(plain), key, 0, chunk)
			if err != nil {
				t.Fatalf("seal: %v", err)
			}
			if n != int64(size) {
				t.Fatalf("sealed %d plaintext bytes, want %d", n, size)
			}
			if got := int64(sealed.Len()); got != streamCipherSize(int64(size), chunk) {
				t.Fatalf("stored size %d, streamCipherSize predicted %d", got, streamCipherSize(int64(size), chunk))
			}

			r := openTestStream(t, sealed.Bytes(), key)
			if r.Size() != int64(size) {
				t.Fatalf("plaintext size %d, want %d", r.Size(), size)
			}
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("round-trip mismatch at size %d", size)
			}
		})
	}
}

// openTestStream builds a reader over an in-memory sealed blob.
func openTestStream(t *testing.T, sealed, key []byte) *streamReader {
	t.Helper()
	src := &bytesReadSeekCloser{Reader: bytes.NewReader(sealed)}
	h, ok := peekStreamHeader(src)
	if !ok {
		t.Fatal("sealed blob did not present a stream header")
	}
	r, err := newStreamReader(src, int64(len(sealed)), h, key)
	if err != nil {
		t.Fatalf("newStreamReader: %v", err)
	}
	return r
}

func TestStreamSeekMatchesPlaintext(t *testing.T) {
	const chunk = 64
	key := testKey(t)
	plain := make([]byte, 5*chunk+11)
	rand.Read(plain)

	var sealed bytes.Buffer
	if _, err := sealStream(&sealed, bytes.NewReader(plain), key, 0, chunk); err != nil {
		t.Fatal(err)
	}
	r := openTestStream(t, sealed.Bytes(), key)

	// Ranges chosen to cross chunk edges, sit inside one chunk, and end exactly on
	// a boundary — the three cases serveRange produces.
	for _, rng := range [][2]int{{0, 10}, {chunk - 5, chunk + 5}, {chunk, 2 * chunk}, {3*chunk + 1, len(plain)}, {len(plain) - 1, len(plain)}} {
		start, end := rng[0], rng[1]
		if _, err := r.Seek(int64(start), io.SeekStart); err != nil {
			t.Fatalf("seek %d: %v", start, err)
		}
		got := make([]byte, end-start)
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatalf("read [%d,%d): %v", start, end, err)
		}
		if !bytes.Equal(got, plain[start:end]) {
			t.Fatalf("range [%d,%d) mismatch", start, end)
		}
	}

	// Seeking past the end yields EOF, not garbage.
	if _, err := r.Seek(int64(len(plain)), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("read past end: got %v, want EOF", err)
	}
}

func TestStreamRejectsTamperedChunk(t *testing.T) {
	const chunk = 64
	key := testKey(t)
	plain := make([]byte, 3*chunk)
	rand.Read(plain)

	var sealed bytes.Buffer
	if _, err := sealStream(&sealed, bytes.NewReader(plain), key, 0, chunk); err != nil {
		t.Fatal(err)
	}
	blob := sealed.Bytes()
	// Flip a bit inside the second chunk's ciphertext.
	blob[streamHeaderLen+chunk+streamTagLen+3] ^= 0x40

	r := openTestStream(t, blob, key)
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("a tampered chunk must fail authentication, not decode")
	}
}

// TestStreamRejectsTruncation is the reason the final chunk is flagged in its
// nonce: without it, dropping the tail of an object would read back as a shorter
// but perfectly valid object.
func TestStreamRejectsTruncation(t *testing.T) {
	const chunk = 64
	key := testKey(t)
	plain := make([]byte, 4*chunk)
	rand.Read(plain)

	var sealed bytes.Buffer
	if _, err := sealStream(&sealed, bytes.NewReader(plain), key, 0, chunk); err != nil {
		t.Fatal(err)
	}
	blob := sealed.Bytes()

	// Drop the trailing empty final chunk, and then a whole data chunk as well.
	for _, cut := range []int{streamTagLen, streamTagLen + chunk + streamTagLen} {
		truncated := blob[:len(blob)-cut]
		src := &bytesReadSeekCloser{Reader: bytes.NewReader(truncated)}
		h, ok := peekStreamHeader(src)
		if !ok {
			t.Fatal("header should still parse")
		}
		r, err := newStreamReader(src, int64(len(truncated)), h, key)
		if err != nil {
			continue // rejected outright, also acceptable
		}
		if _, err := io.ReadAll(r); err == nil {
			t.Fatalf("truncation by %d bytes was accepted as a valid object", cut)
		}
	}
}

func TestStreamRejectsWrongKey(t *testing.T) {
	const chunk = 64
	plain := []byte("per-bucket keys must not open each other's objects")

	var sealed bytes.Buffer
	if _, err := sealStream(&sealed, bytes.NewReader(plain), testKey(t), 0, chunk); err != nil {
		t.Fatal(err)
	}
	r := openTestStream(t, sealed.Bytes(), testKey(t))
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("decrypting with a different key must fail")
	}
}

// TestStreamReaderHoldsOneChunk is the property the whole format exists for: a
// reader's footprint is set by the chunk size, not by the object size.
func TestStreamReaderHoldsOneChunk(t *testing.T) {
	const chunk = 1024
	key := testKey(t)
	plain := make([]byte, 400*chunk) // 400 KiB through a 1 KiB chunk
	rand.Read(plain)

	var sealed bytes.Buffer
	if _, err := sealStream(&sealed, bytes.NewReader(plain), key, 0, chunk); err != nil {
		t.Fatal(err)
	}
	r := openTestStream(t, sealed.Bytes(), key)
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}

	held := cap(r.buf) + cap(r.ct)
	if max := 4 * (chunk + streamTagLen); held > max {
		t.Fatalf("reader retained %d bytes after streaming %d; a chunk-bounded reader should hold under %d",
			held, len(plain), max)
	}
}

// seekCountingSource counts Seeks so a test can assert none happen.
type seekCountingSource struct {
	*bytes.Reader
	seeks int
}

func (s *seekCountingSource) Seek(off int64, whence int) (int64, error) {
	s.seeks++
	return s.Reader.Seek(off, whence)
}
func (s *seekCountingSource) Close() error { return nil }

// TestStreamReadDoesNotSeekSource guards a regression that a correctness test
// cannot see. Not every inner reader seeks cheaply: the decompressor has to
// materialise the whole object to satisfy one, because the codecs are not
// seekable. A straight read that seeks even once therefore costs a full copy of
// the object per concurrent reader on a compressed+encrypted bucket, which is
// the exact failure this format was written to remove. Measured at 2257 MiB for
// a 128 MiB object at 8 readers before this was fixed.
func TestStreamReadDoesNotSeekSource(t *testing.T) {
	const chunk = 64
	key := testKey(t)
	plain := make([]byte, 10*chunk+5)
	rand.Read(plain)

	var sealed bytes.Buffer
	if _, err := sealStream(&sealed, bytes.NewReader(plain), key, 0, chunk); err != nil {
		t.Fatal(err)
	}

	src := &seekCountingSource{Reader: bytes.NewReader(sealed.Bytes())}
	h, ok := peekStreamHeader(src)
	if !ok {
		t.Fatal("expected a stream header")
	}
	r, err := newStreamReader(src, int64(sealed.Len()), h, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("round-trip mismatch")
	}
	if src.seeks != 0 {
		t.Fatalf("a sequential read seeked the source %d times; it must not seek at all", src.seeks)
	}
}

// TestEncryptedEngineReadsLegacyWholeObject pins backward compatibility: objects
// sealed by the pre-4.4.53 whole-object format must still be readable, because
// upgrading a server must not strand the data already on its disks.
func TestEncryptedEngineReadsLegacyWholeObject(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.CreateBucketDir("b"); err != nil {
		t.Fatal(err)
	}
	key := testKey(t)
	plain := []byte("written by the whole-object format that shipped before streaming")

	// Write the blob exactly as the old engine did: nonce || GCM seal of everything.
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	legacy := gcm.Seal(nonce, nonce, plain, nil)
	if _, _, err := fs.PutObject("b", "old.bin", bytes.NewReader(legacy), int64(len(legacy))); err != nil {
		t.Fatal(err)
	}

	enc, err := NewEncryptedEngine(fs, key)
	if err != nil {
		t.Fatal(err)
	}
	r, size, err := enc.GetObject("b", "old.bin")
	if err != nil {
		t.Fatalf("legacy object should still decrypt: %v", err)
	}
	defer r.Close()
	if size != int64(len(plain)) {
		t.Fatalf("size %d, want %d", size, len(plain))
	}
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, plain) {
		t.Fatalf("legacy round-trip mismatch: %q", got)
	}
}

// TestEncryptedEngineStreamRoundTrip exercises the engine end to end, including
// that the object on disk is neither plaintext nor the old format.
func TestEncryptedEngineStreamRoundTrip(t *testing.T) {
	withChunkSize(t, 128)

	dir := t.TempDir()
	fs, err := NewFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	fs.CreateBucketDir("b")
	key := testKey(t)
	enc, err := NewEncryptedEngine(fs, key)
	if err != nil {
		t.Fatal(err)
	}

	for _, size := range sizesAroundChunk(128) {
		plain := make([]byte, size)
		rand.Read(plain)
		name := fmt.Sprintf("obj-%d", size)

		n, _, err := enc.PutObject("b", name, bytes.NewReader(plain), int64(size))
		if err != nil {
			t.Fatalf("put %d: %v", size, err)
		}
		if n != int64(size) {
			t.Fatalf("put reported %d plaintext bytes, want %d", n, size)
		}

		raw, err := os.ReadFile(fs.ObjectPath("b", name))
		if err != nil {
			t.Fatal(err)
		}
		if !isStreamBlob(raw) {
			t.Fatalf("object %s was not written in the streaming format", name)
		}
		// Only meaningful once the plaintext is long enough that finding it inside
		// random ciphertext by chance is not a coin flip: a single random byte
		// turns up in a 60-byte blob about a fifth of the time.
		if size >= 16 && bytes.Contains(raw, plain) {
			t.Fatalf("plaintext leaked to disk for %s", name)
		}

		r, gotSize, err := enc.GetObject("b", name)
		if err != nil {
			t.Fatalf("get %d: %v", size, err)
		}
		if gotSize != int64(size) {
			r.Close()
			t.Fatalf("get reported size %d, want %d", gotSize, size)
		}
		got, _ := io.ReadAll(r)
		r.Close()
		if !bytes.Equal(got, plain) {
			t.Fatalf("engine round-trip mismatch at size %d", size)
		}
	}
}

// TestPerBucketEngineStreamsWithRotatedKey covers the header's key version doing
// its job: an object stays readable after the bucket's key is rotated, because
// the version it was sealed under travels with it.
func TestPerBucketEngineStreamsWithRotatedKey(t *testing.T) {
	withChunkSize(t, 128)

	fs, _ := NewFileSystem(t.TempDir())
	mgr := newMgr(t)
	pe, _ := NewPerBucketEngine(fs, nil)
	pe.SetManager(mgr)
	fs.CreateBucketDir("t")
	if err := mgr.EnableBucket("t"); err != nil {
		t.Fatal(err)
	}

	first := make([]byte, 500)
	rand.Read(first)
	if _, _, err := pe.PutObject("t", "v1.bin", bytes.NewReader(first), int64(len(first))); err != nil {
		t.Fatal(err)
	}

	if err := mgr.Rotate("t"); err != nil {
		t.Fatal(err)
	}
	second := make([]byte, 500)
	rand.Read(second)
	if _, _, err := pe.PutObject("t", "v2.bin", bytes.NewReader(second), int64(len(second))); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string][]byte{"v1.bin": first, "v2.bin": second} {
		r, size, err := pe.GetObject("t", name)
		if err != nil {
			t.Fatalf("get %s after rotation: %v", name, err)
		}
		if size != int64(len(want)) {
			r.Close()
			t.Fatalf("%s size %d, want %d", name, size, len(want))
		}
		got, _ := io.ReadAll(r)
		r.Close()
		if !bytes.Equal(got, want) {
			t.Fatalf("%s mismatch after rotation", name)
		}
	}
}
