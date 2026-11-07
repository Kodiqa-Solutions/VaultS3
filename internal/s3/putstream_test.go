package s3

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func crcB64(v uint32) string {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return base64.StdEncoding.EncodeToString(b)
}

// drain consumes the body the way the storage engine would, which is what makes
// the digests final.
func drain(t *testing.T, d *putDigests) {
	t.Helper()
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func TestPutDigestsComputesOnlyRequestedHashes(t *testing.T) {
	body := []byte("some object payload")

	// Nothing requested: no hashing work at all, which is the point of the
	// selective wiring — most uploads ask for nothing.
	r := httptest.NewRequest(http.MethodPut, "/b/k", bytes.NewReader(body))
	d := newPutDigests(r, r.Body)
	if d.sum != nil {
		t.Error("no checksum headers, yet hashes were wired up")
	}
	drain(t, d)
	if d.size() != int64(len(body)) {
		t.Errorf("size = %d, want %d", d.size(), len(body))
	}

	r = httptest.NewRequest(http.MethodPut, "/b/k", bytes.NewReader(body))
	r.Header.Set("X-Amz-Checksum-Crc32", crcB64(crc32.ChecksumIEEE(body)))
	d = newPutDigests(r, r.Body)
	if d.crc32 == nil || d.sha256 != nil || d.sha1 != nil || d.crc32c != nil || d.md5 != nil {
		t.Error("only CRC32 was requested, but other hashes were wired up")
	}
}

func TestPutDigestsAcceptsEveryMatchingChecksum(t *testing.T) {
	body := []byte("payload that will be hashed every which way")
	sum256 := sha256.Sum256(body)
	sum1 := sha1.Sum(body)
	sumMD5 := md5.Sum(body)

	r := httptest.NewRequest(http.MethodPut, "/b/k", bytes.NewReader(body))
	r.Header.Set("Content-MD5", b64(sumMD5[:]))
	r.Header.Set("X-Amz-Checksum-Sha256", b64(sum256[:]))
	r.Header.Set("X-Amz-Checksum-Sha1", b64(sum1[:]))
	r.Header.Set("X-Amz-Checksum-Crc32", crcB64(crc32.ChecksumIEEE(body)))
	r.Header.Set("X-Amz-Checksum-Crc32c", crcB64(crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli))))

	d := newPutDigests(r, r.Body)
	drain(t, d)
	sums, code, msg, ok := d.verify(r)
	if !ok {
		t.Fatalf("valid checksums rejected: %s %s", code, msg)
	}
	if sums.SHA256 != b64(sum256[:]) || sums.SHA1 != b64(sum1[:]) {
		t.Errorf("recorded checksums wrong: %+v", sums)
	}
	if sums.CRC32 == "" || sums.CRC32C == "" {
		t.Errorf("CRC values not recorded: %+v", sums)
	}
}

// Each algorithm must be rejected with the same code the buffered path used, so
// a client sees no behaviour change from streaming.
func TestPutDigestsRejectsEachBadChecksum(t *testing.T) {
	body := []byte("the real payload")
	wrong := []byte("not the payload at all")
	wrongMD5 := md5.Sum(wrong)
	wrong256 := sha256.Sum256(wrong)
	wrong1 := sha1.Sum(wrong)

	cases := []struct {
		name, header, value, wantCode string
	}{
		{"content-md5", "Content-MD5", b64(wrongMD5[:]), "BadDigest"},
		{"sha256", "X-Amz-Checksum-Sha256", b64(wrong256[:]), "BadDigest"},
		{"sha1", "X-Amz-Checksum-Sha1", b64(wrong1[:]), "BadDigest"},
		{"crc32", "X-Amz-Checksum-Crc32", crcB64(crc32.ChecksumIEEE(wrong)), "BadDigest"},
		{"crc32c", "X-Amz-Checksum-Crc32c", crcB64(crc32.Checksum(wrong, crc32.MakeTable(crc32.Castagnoli))), "BadDigest"},
		{"malformed md5", "Content-MD5", "!!!not-base64!!!", "InvalidDigest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/b/k", bytes.NewReader(body))
			r.Header.Set(tc.header, tc.value)
			d := newPutDigests(r, r.Body)
			drain(t, d)
			_, code, _, ok := d.verify(r)
			if ok {
				t.Fatalf("%s: bad checksum accepted", tc.name)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// A trailer checksum arrives after an aws-chunked body. maybeDecodeAwsChunked
// reads it, the digest is checked against it, and the value is recorded. A
// trailer that was declared but never sent is refused: before, the trailer was
// never read, so whatever it said was accepted.
func TestPutDigestsChecksTrailerChecksum(t *testing.T) {
	body := []byte("trailer payload")
	sum := sha256.Sum256(body)
	framed := func(trailer string) []byte {
		return []byte(fmt.Sprintf("%x\r\n%s\r\n0\r\n%s\r\n", len(body), body, trailer))
	}
	for _, tc := range []struct {
		name     string
		trailer  string
		wantOK   bool
		wantCode string
	}{
		{"matching value", "x-amz-checksum-sha256:" + b64(sum[:]) + "\r\n", true, ""},
		{"wrong value", "x-amz-checksum-sha256:" + b64(make([]byte, 32)) + "\r\n", false, "BadDigest"},
		{"declared, not sent", "", false, "MalformedTrailerError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/b/k", bytes.NewReader(framed(tc.trailer)))
			r.Header.Set("Content-Encoding", "aws-chunked")
			r.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
			r.Header.Set("X-Amz-Trailer", "x-amz-checksum-sha256")
			maybeDecodeAwsChunked(r)
			d := newPutDigests(r, r.Body)
			drain(t, d)
			sums, code, _, ok := d.verify(r)
			if ok != tc.wantOK || code != tc.wantCode {
				t.Fatalf("ok=%v code=%q, want ok=%v code=%q", ok, code, tc.wantOK, tc.wantCode)
			}
			if ok && sums.SHA256 != b64(sum[:]) {
				t.Errorf("recorded sha256 %q, want %q", sums.SHA256, b64(sum[:]))
			}
		})
	}
}

// The digests must be identical to what the buffered helper produced, since the
// values are stored on the object and returned to clients on GET.
func TestStreamedDigestsMatchBufferedOnes(t *testing.T) {
	body := bytes.Repeat([]byte("abcdefghij"), 5000) // spans many Read calls
	sum256 := sha256.Sum256(body)

	r := httptest.NewRequest(http.MethodPut, "/b/k", bytes.NewReader(body))
	r.Header.Set("X-Amz-Checksum-Sha256", b64(sum256[:]))
	r.Header.Set("X-Amz-Checksum-Crc32", crcB64(crc32.ChecksumIEEE(body)))
	d := newPutDigests(r, r.Body)
	drain(t, d)
	streamed, _, _, ok := d.verify(r)
	if !ok {
		t.Fatal("streamed verification failed on a valid body")
	}

	// The buffered helper is still used by the multipart path, so its answer on
	// the same bytes is the reference the streamed values must reproduce.
	bufSHA, bufCRC, _, _, err := checksumFromRequest(r, body)
	if err != nil {
		t.Fatalf("buffered helper rejected a valid body: %v", err)
	}
	if streamed.SHA256 != bufSHA || streamed.CRC32 != bufCRC {
		t.Errorf("streamed (%q,%q) != buffered (%q,%q)",
			streamed.SHA256, streamed.CRC32, bufSHA, bufCRC)
	}
	if streamed.SHA256 != b64(sum256[:]) {
		t.Errorf("sha256 = %q, want %q", streamed.SHA256, b64(sum256[:]))
	}
	if streamed.CRC32 != crcB64(crc32.ChecksumIEEE(body)) {
		t.Errorf("crc32 = %q", streamed.CRC32)
	}
}

// A short read must not be silently treated as a complete object.
func TestPutDigestsPropagatesReadError(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/b/k", nil)
	d := newPutDigests(r, io.MultiReader(strings.NewReader("half"), errReader{}))
	_, err := io.Copy(io.Discard, d)
	if err == nil {
		t.Fatal("read error swallowed")
	}
	if d.size() != 4 {
		t.Errorf("size = %d, want the 4 bytes that did arrive", d.size())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
