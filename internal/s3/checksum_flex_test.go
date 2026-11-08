package s3

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// The CRC-64/NVME table must be the real one. The other tests compute expected
// values with the same table, so they could not notice a wrong polynomial;
// this pins it to the published check value (and the value AWS's own CRT
// library gives).
func TestCRC64NVMEMatchesThePublishedCheckValue(t *testing.T) {
	if got := crc64.Checksum([]byte("123456789"), crc64NVMETable); got != 0xae8b14860a799888 {
		t.Fatalf("CRC-64/NVME(123456789) = %#x, want 0xae8b14860a799888", got)
	}
}

// checksumOf returns the base64 S3 checksum of body for an algorithm name as
// it appears in the header (crc32, crc32c, sha1, sha256, crc64nvme).
func checksumOf(algo string, body []byte) string {
	var raw []byte
	switch algo {
	case "crc32":
		raw = binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(body))
	case "crc32c":
		raw = binary.BigEndian.AppendUint32(nil, crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli)))
	case "sha1":
		s := sha1.Sum(body)
		raw = s[:]
	case "sha256":
		s := sha256.Sum256(body)
		raw = s[:]
	case "crc64nvme":
		raw = binary.BigEndian.AppendUint64(nil, crc64.Checksum(body, crc64NVMETable))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// wrongChecksumOf is a well-formed value of the right length that does not
// match body.
func wrongChecksumOf(algo string, body []byte) string {
	return checksumOf(algo, append([]byte("not "), body...))
}

var flexAlgos = []string{"crc32", "crc32c", "sha1", "sha256", "crc64nvme"}

// trailerPut sends body as an aws-chunked upload with trailer as the raw
// trailer text, signed the way current SDKs sign it
// (STREAMING-UNSIGNED-PAYLOAD-TRAILER), declaring the given trailer names.
func trailerPut(t *testing.T, ts *httptest.Server, path string, body []byte, declare, trailer string, headers ...string) result {
	t.Helper()
	data := fmt.Sprintf("%x\r\n%s\r\n", len(body), body)
	tail := fmt.Sprintf("0\r\n%s\r\n", trailer)
	framed := data + tail
	var rd io.Reader = strings.NewReader(framed)
	if lateTail {
		// The final chunk and the trailer arrive after the data, as they often
		// do on a real network, so the server cannot see the end of the body
		// in the same read as its last bytes.
		rd = io.MultiReader(strings.NewReader(data), &lateReader{s: tail})
	}
	req, err := http.NewRequest(http.MethodPut, ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(body)))
	if declare != "" {
		req.Header.Set("X-Amz-Trailer", declare)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	signStreamingTrailer(req)
	return send(t, req)
}

// lateTail makes trailerPut send the final chunk and trailer 200ms after the
// data.
var lateTail bool

type lateReader struct {
	s     string
	slept bool
}

func (l *lateReader) Read(p []byte) (int, error) {
	if !l.slept {
		l.slept = true
		time.Sleep(200 * time.Millisecond)
	}
	if l.s == "" {
		return 0, io.EOF
	}
	n := copy(p, l.s)
	l.s = l.s[n:]
	return n, nil
}

// signStreamingTrailer signs r with the literal streaming payload marker, as
// an SDK does for an aws-chunked upload with an unsigned trailer.
func signStreamingTrailer(r *http.Request) {
	now := time.Now().UTC()
	dateStr, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("Host", r.Host)
	r.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	signed := signedHeaderList(r)
	canonical := buildCanonicalRequest(r, signed, "")
	toSign := buildStringToSign(dateStr, testRegion, "s3", canonical, r)
	sig := hex.EncodeToString(hmacSHA256(deriveSigningKey(testSecretKey, dateStr, testRegion, "s3"), []byte(toSign)))
	r.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		testAccessKey, dateStr, testRegion, signed, sig))
}

// headChecksum returns the x-amz-checksum-<algo> header HEAD reports.
func headChecksum(t *testing.T, ts *httptest.Server, path, algo string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodHead, ts.URL+path, nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-Amz-Checksum-" + algo)
}

// A checksum sent as an aws-chunked trailer, the default of current AWS SDKs
// over HTTPS, is now compared with the bytes. A matching one is stored and
// returned, a wrong one is refused and stores nothing, and a wrong one on an
// overwrite leaves the object that was there. Before, the trailer was never
// read, so every one of the "wrong" cases was stored.
func TestTrailerChecksumIsVerifiedForEveryAlgorithm(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/trail", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("trailer checked body "), 300)

	for _, algo := range flexAlgos {
		t.Run(algo, func(t *testing.T) {
			name := "x-amz-checksum-" + algo
			good := trailerPut(t, ts, "/trail/good-"+algo, body, name, name+":"+checksumOf(algo, body)+"\r\n")
			good.must(t, "honest trailer")
			if code, got := headChecksum(t, ts, "/trail/good-"+algo, algo); code != 200 || got != checksumOf(algo, body) {
				t.Fatalf("HEAD %d, checksum %q, want 200 and %q", code, got, checksumOf(algo, body))
			}

			bad := trailerPut(t, ts, "/trail/bad-"+algo, body, name, name+":"+wrongChecksumOf(algo, body)+"\r\n")
			if bad.code != http.StatusBadRequest || !strings.Contains(bad.body, "BadDigest") {
				t.Fatalf("wrong trailer: %d %s, want 400 BadDigest", bad.code, bad.body)
			}
			if code, _ := headChecksum(t, ts, "/trail/bad-"+algo, algo); code != http.StatusNotFound {
				t.Fatalf("refused upload is readable: HEAD %d", code)
			}

			over := trailerPut(t, ts, "/trail/good-"+algo, []byte("replacement"), name, name+":"+wrongChecksumOf(algo, []byte("replacement"))+"\r\n")
			if over.code != http.StatusBadRequest {
				t.Fatalf("wrong trailer on overwrite: %d %s", over.code, over.body)
			}
			got := call(t, ts, http.MethodGet, "/trail/good-"+algo, "").must(t, "GET after refused overwrite")
			if got.body != string(body) {
				t.Fatalf("refused overwrite changed the object (%d bytes)", len(got.body))
			}
		})
	}
}

// Malformed trailers are refused with MalformedTrailerError, as on AWS: a
// trailer that was declared but not sent, a line that is not name:value, and
// a trailer longer than any real one.
func TestMalformedTrailersAreRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/mal", "").must(t, "create bucket")
	body := []byte("body for malformed trailers")
	for _, tc := range []struct {
		name, trailer string
	}{
		{"declared, not sent", ""},
		{"not name:value", "this line has no colon\r\n"},
		// Carries a correct checksum, so only the size cap can refuse it.
		{"oversized", "x-amz-checksum-crc32:" + checksumOf("crc32", body) + "\r\nx-amz-padding:" + strings.Repeat("a", maxTrailerBytes) + "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := trailerPut(t, ts, "/mal/obj", body, "x-amz-checksum-crc32", tc.trailer)
			if r.code != http.StatusBadRequest || !strings.Contains(r.body, "MalformedTrailerError") {
				t.Fatalf("%d %s, want 400 MalformedTrailerError", r.code, r.body)
			}
			if code, _ := headChecksum(t, ts, "/mal/obj", "crc32"); code != http.StatusNotFound {
				t.Fatalf("refused upload is readable: HEAD %d", code)
			}
		})
	}
}

// An upload whose checksum is in a header and also, differently, in the
// trailer is checked against the header: a trailer cannot replace a value the
// client committed to up front. A trailer signature line is skipped.
func TestTrailerCannotOverrideAHeaderChecksum(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/hdr", "").must(t, "create bucket")
	body := []byte("header wins")
	trailer := "x-amz-checksum-crc32:" + wrongChecksumOf("crc32", body) + "\r\nx-amz-trailer-signature:abc123\r\n"
	trailerPut(t, ts, "/hdr/ok", body, "x-amz-checksum-crc32", trailer,
		"X-Amz-Checksum-Crc32", checksumOf("crc32", body)).must(t, "honest header, lying trailer")
	r := trailerPut(t, ts, "/hdr/bad", body, "x-amz-checksum-crc32", "x-amz-checksum-crc32:"+checksumOf("crc32", body)+"\r\n",
		"X-Amz-Checksum-Crc32", wrongChecksumOf("crc32", body))
	if r.code != http.StatusBadRequest {
		t.Fatalf("lying header, honest trailer: %d, want 400", r.code)
	}
}

// CRC64NVME, the checksum S3 itself defaults to, works in the header form too:
// checked, stored, and returned on PUT, GET and HEAD. It used to be ignored, so
// a wrong value was stored.
func TestCRC64NVMEHeaderIsCheckedStoredAndReturned(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/nvme", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("nvme "), 1000)
	want := checksumOf("crc64nvme", body)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/nvme/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("X-Amz-Checksum-Crc64nvme", want)
	signV4Request(req, testAccessKey, testSecretKey, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Amz-Checksum-Crc64nvme") != want {
		t.Fatalf("PUT %d, echoed %q, want 200 and %q", resp.StatusCode, resp.Header.Get("X-Amz-Checksum-Crc64nvme"), want)
	}
	if code, got := headChecksum(t, ts, "/nvme/obj", "crc64nvme"); code != 200 || got != want {
		t.Fatalf("HEAD %d %q, want 200 %q", code, got, want)
	}

	bad := call(t, ts, http.MethodPut, "/nvme/bad", string(body), "X-Amz-Checksum-Crc64nvme", wrongChecksumOf("crc64nvme", body))
	if bad.code != http.StatusBadRequest || !strings.Contains(bad.body, "BadDigest") {
		t.Fatalf("wrong CRC64NVME header: %d %s, want 400 BadDigest", bad.code, bad.body)
	}
}

// A part of a multipart upload is checked the same way, in both forms, and its
// CRC64NVME is returned so the client can list it in CompleteMultipartUpload.
func TestUploadPartChecksTrailerAndReturnsCRC64NVME(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/parts", "").must(t, "create bucket")
	init := call(t, ts, http.MethodPost, "/parts/big?uploads", "").must(t, "create upload")
	start := strings.Index(init.body, "<UploadId>") + len("<UploadId>")
	uploadID := init.body[start:strings.Index(init.body, "</UploadId>")]
	part := bytes.Repeat([]byte("p"), 64*1024)
	name := "x-amz-checksum-crc64nvme"

	bad := trailerPut(t, ts, "/parts/big?partNumber=1&uploadId="+uploadID, part, name, name+":"+wrongChecksumOf("crc64nvme", part)+"\r\n")
	if bad.code != http.StatusBadRequest || !strings.Contains(bad.body, "BadDigest") {
		t.Fatalf("wrong part trailer: %d %s, want 400 BadDigest", bad.code, bad.body)
	}

	framed := fmt.Sprintf("%x\r\n%s\r\n0\r\n%s:%s\r\n\r\n", len(part), part, name, checksumOf("crc64nvme", part))
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/parts/big?partNumber=1&uploadId="+uploadID, strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(part)))
	req.Header.Set("X-Amz-Trailer", name)
	signStreamingTrailer(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Amz-Checksum-Crc64nvme") != checksumOf("crc64nvme", part) {
		t.Fatalf("part %d, echoed %q, want 200 and %q", resp.StatusCode, resp.Header.Get("X-Amz-Checksum-Crc64nvme"), checksumOf("crc64nvme", part))
	}
}

// stopsAtSize is an engine that reads exactly the declared size and never asks
// the body for more, as the erasure-coding engine does (one LimitReader per
// data shard). The body then never reaches EOF inside the engine.
type stopsAtSize struct{ storage.Engine }

func (e stopsAtSize) PutObject(bucket, key string, r io.Reader, size int64) (int64, string, error) {
	if size >= 0 {
		r = io.LimitReader(r, size)
	}
	return e.Engine.PutObject(bucket, key, r, size)
}

// With such an engine the trailer, which follows the data, is only reached
// when the checks finish by reading on to the end. Without that, every honest
// trailer upload on an erasure-coded server was refused as "declared, not
// sent", and a lying one could not be told apart.
func TestTrailerIsVerifiedWhenTheEngineStopsAtTheDeclaredSize(t *testing.T) {
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fs, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, stopsAtSize{fs}, NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil), false, "", nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	call(t, ts, http.MethodPut, "/exact", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("exact size "), 500)
	name := "x-amz-checksum-crc32"
	lateTail = true
	t.Cleanup(func() { lateTail = false })
	trailerPut(t, ts, "/exact/good", body, name, name+":"+checksumOf("crc32", body)+"\r\n").must(t, "honest trailer")
	if code, got := headChecksum(t, ts, "/exact/good", "crc32"); code != 200 || got != checksumOf("crc32", body) {
		t.Fatalf("HEAD %d %q, want 200 %q", code, got, checksumOf("crc32", body))
	}
	bad := trailerPut(t, ts, "/exact/bad", body, name, name+":"+wrongChecksumOf("crc32", body)+"\r\n")
	if bad.code != http.StatusBadRequest || !strings.Contains(bad.body, "BadDigest") {
		t.Fatalf("wrong trailer: %d %s, want 400 BadDigest", bad.code, bad.body)
	}
	if code, _ := headChecksum(t, ts, "/exact/bad", "crc32"); code != http.StatusNotFound {
		t.Fatalf("refused upload is readable: HEAD %d", code)
	}
}

// Regression (seventh review): an aws-chunked upload without
// X-Amz-Decoded-Content-Length was held to its framed Content-Length, so the
// shorter decoded body was refused as cut short (5.0.8 stored it). Its
// decoded length is unknown, on either engine.
func TestAwsChunkedUploadWithoutDecodedLengthIsStored(t *testing.T) {
	plain, _ := fullServer(t)
	exact, _ := exactSizeServer(t)
	for name, ts := range map[string]*httptest.Server{"plain": plain, "exact-size engine": exact} {
		t.Run(name, func(t *testing.T) {
			call(t, ts, http.MethodPut, "/nodl", "").must(t, "create bucket")
			body := bytes.Repeat([]byte("hello world "), 300)
			framed := fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(body), body)
			req, _ := http.NewRequest(http.MethodPut, ts.URL+"/nodl/k", strings.NewReader(framed))
			req.ContentLength = int64(len(framed))
			req.Header.Set("Content-Encoding", "aws-chunked")
			signStreamingTrailer(req)
			if r := send(t, req); r.code != http.StatusOK {
				t.Fatalf("aws-chunked PUT without a decoded length: %d %s, want 200", r.code, r.body)
			}
			if got := call(t, ts, http.MethodGet, "/nodl/k", ""); got.body != string(body) {
				t.Fatalf("stored %d bytes, want %d", len(got.body), len(body))
			}
		})
	}
}
