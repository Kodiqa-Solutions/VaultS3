package storage

import (
	"bytes"
	"encoding/hex"
	"io"
	"testing"
)

// v1Fixtures were produced by the format 1 sealStream (before format 2 existed)
// with key 0x5a repeated, key version 7 and 32-byte chunks. They are kept as
// bytes, not regenerated, so the test proves that objects already on disk keep
// reading after the upgrade.
var v1Fixtures = []struct {
	name  string
	hex   string
	plain []byte
}{
	{"three chunks", "5653335301000000070000002095444ad7f5d75fbb5072f459b1af534d810041b2f65ff4d977ad085e974487045e65e75f7c7b8c6ca564e30ac897a6ada0d18230f07a1de4bc3886d23b7ad2ce37844b4780eecbd09fab5ecc6b27f8a4a7f3062a03e418ee2ea0afc275a8b535fd2e6d7b08997f601c27eccaed574cbc59243fc686ff9ae58a641d0c82", []byte("VaultS3 v1 stream fixture: written by the pre-v2 sealStream, 3 chunks.")},
	{"chunk boundary", "56533353010000000700000020b1828f83bae9088099f4863178bbcf36d9ecec84840fdfcd271c7ce63c54f20002a60a1c695116064a4146c861e616dea62641fa85d1ee3b69477db119161f041c082eda5a12e105139b8d25d0b8d851327c91b6713d0ef11ad24796397084cb80adc4f992b306e4e9a6f3b2afa3bc648b7520b0d336a2", bytes.Repeat([]byte("b"), 64)},
}

func openStreamBlob(t *testing.T, blob, key []byte) *streamReader {
	t.Helper()
	src := &bytesReadSeekCloser{Reader: bytes.NewReader(blob)}
	h, ok := peekStreamHeader(src)
	if !ok {
		t.Fatal("blob not recognised as a VS3S stream")
	}
	sr, err := newStreamReader(src, int64(len(blob)), h, key)
	if err != nil {
		t.Fatalf("newStreamReader: %v", err)
	}
	return sr
}

// readRange reads [off, off+n) through Seek, the way a Range GET does.
func readRange(t *testing.T, r io.ReadSeeker, off, n int64) []byte {
	t.Helper()
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read range %d+%d: %v", off, n, err)
	}
	return buf
}

func TestStreamFormatV1StillReads(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	for _, f := range v1Fixtures {
		t.Run(f.name, func(t *testing.T) {
			blob, err := hex.DecodeString(f.hex)
			if err != nil {
				t.Fatal(err)
			}
			if blob[4] != streamFormatV1 {
				t.Fatalf("fixture format byte %d, want 1", blob[4])
			}
			sr := openStreamBlob(t, blob, key)
			if sr.Size() != int64(len(f.plain)) {
				t.Fatalf("Size %d, want %d", sr.Size(), len(f.plain))
			}
			got, err := io.ReadAll(sr)
			if err != nil || !bytes.Equal(got, f.plain) {
				t.Fatalf("read %q (err %v), want %q", got, err, f.plain)
			}
			// A Range read that crosses a chunk boundary.
			if got := readRange(t, sr, 30, 10); !bytes.Equal(got, f.plain[30:40]) {
				t.Fatalf("range read %q, want %q", got, f.plain[30:40])
			}
			// Truncation is still caught for format 1.
			short := blob[:len(blob)-streamTagLen]
			src := &bytesReadSeekCloser{Reader: bytes.NewReader(short)}
			h, _ := peekStreamHeader(src)
			if r, err := newStreamReader(src, int64(len(short)), h, key); err == nil {
				if _, err := io.ReadAll(r); err == nil {
					t.Fatal("a truncated format 1 blob read without error")
				}
			}
		})
	}
}

// New writes are format 2: they carry a salt and are sealed under a key derived
// per object, so the same plaintext under the same bucket key yields unrelated
// ciphertext and the stored key alone does not open the chunks.
func TestStreamFormatV2RoundTripAndRange(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	const chunk = 32
	for _, size := range []int{0, 1, chunk, 3*chunk + 5, 4 * chunk} {
		plain := make([]byte, size)
		for i := range plain {
			plain[i] = byte(i*7 + 3)
		}
		var sealed bytes.Buffer
		if _, err := sealStream(&sealed, bytes.NewReader(plain), key, 7, chunk); err != nil {
			t.Fatal(err)
		}
		blob := sealed.Bytes()
		if blob[4] != streamFormatV2 {
			t.Fatalf("new write has format byte %d, want 2", blob[4])
		}
		if int64(len(blob)) != streamCipherSize(int64(size), chunk) {
			t.Fatalf("stored %d bytes, streamCipherSize says %d", len(blob), streamCipherSize(int64(size), chunk))
		}
		sr := openStreamBlob(t, blob, key)
		got, err := io.ReadAll(sr)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("size %d: round trip failed (err %v)", size, err)
		}
		if size > chunk+10 {
			if got := readRange(t, sr, chunk-5, 10); !bytes.Equal(got, plain[chunk-5:chunk+5]) {
				t.Fatalf("size %d: range read mismatch", size)
			}
		}
		// The chunks must not open under the stored key directly.
		gcm, _ := newAEAD(key)
		var prefix [streamNoncePrefix]byte
		copy(prefix[:], blob[13:20])
		first := blob[streamHeaderLen:]
		if size >= chunk {
			first = first[:chunk+streamTagLen]
		}
		if _, err := gcm.Open(nil, streamNonce(prefix, 0, size < chunk), first, nil); err == nil {
			t.Fatalf("size %d: chunk 0 opened under the stored key, so no per-object key was used", size)
		}
		// A wrong key fails cleanly. An empty object has no bytes to read, so
		// there is nothing to authenticate on the way.
		if size > 0 {
			if _, err := io.ReadAll(openStreamBlob(t, blob, bytes.Repeat([]byte{1}, 32))); err == nil {
				t.Fatalf("size %d: wrong key read without error", size)
			}
		}
	}

	// Two writes of the same plaintext do not share a per-object key.
	var a, b bytes.Buffer
	sealStream(&a, bytes.NewReader([]byte("same")), key, 7, chunk)
	sealStream(&b, bytes.NewReader([]byte("same")), key, 7, chunk)
	if bytes.Equal(a.Bytes()[20:streamHeaderLen], b.Bytes()[20:streamHeaderLen]) {
		t.Fatal("two objects drew the same salt")
	}
}

// A format 2 header cut off inside its salt is an error, not a short object.
func TestStreamFormatV2TruncatedHeader(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	var sealed bytes.Buffer
	sealStream(&sealed, bytes.NewReader([]byte("x")), key, 7, 32)
	cut := sealed.Bytes()[:30]
	if _, err := parseStreamHeader(bytes.NewReader(cut)); err == nil {
		t.Fatal("a header truncated inside the salt parsed")
	}
}
