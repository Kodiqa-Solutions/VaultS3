package s3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// The chunk signature formula must be AWS's, not merely self-consistent: the
// tests below sign chunks with the same code they check. This is the worked
// example from AWS's SigV4 streaming documentation (64 KiB of 'a', then 1 KiB,
// then the final chunk, chained from a fixed seed signature).
func TestChunkSignerMatchesAWSExample(t *testing.T) {
	c := &chunkSigner{
		key:     deriveSigningKey("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "20130524", "us-east-1", "s3"),
		amzDate: "20130524T000000Z",
		scope:   "20130524/us-east-1/s3/aws4_request",
		prev:    "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9",
	}
	for _, tc := range []struct {
		n   int
		sig string
	}{
		{65536, "ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648"},
		{1024, "0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497"},
		{0, "b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9"},
	} {
		s := sha256.Sum256(bytes.Repeat([]byte("a"), tc.n))
		if err := c.chunk(s[:], tc.sig); err != nil {
			t.Fatalf("%d-byte chunk: %v", tc.n, err)
		}
	}
}

// streamOpts shapes a signed streaming upload, honest unless told otherwise.
type streamOpts struct {
	chunk          int    // chunk size
	tamperChunk    int    // 1-based chunk whose data is altered after signing, 0 for none
	swapChunks     bool   // send the first two chunks in the wrong order
	dropSigOnChunk int    // 1-based chunk sent without its chunk-signature
	trailerName    string // e.g. x-amz-checksum-crc32; empty for no trailer
	trailerValue   string
	trailerSig     string // "" signs honestly, "-" omits it, anything else is sent as is
	minioLayout    bool   // lay the trailer out as minio-go does
}

// signedStreamPut sends body as STREAMING-AWS4-HMAC-SHA256-PAYLOAD (or the
// -TRAILER form), with a real chain of chunk signatures, like minio-go does
// over plain HTTP.
func signedStreamPut(t *testing.T, ts *httptest.Server, path string, body []byte, o streamOpts) result {
	t.Helper()
	marker := signedStreaming
	if o.trailerName != "" {
		marker = signedStreamingTrailer
	}
	req, _ := http.NewRequest(http.MethodPut, ts.URL+path, nil)
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(body)))
	if o.trailerName != "" {
		req.Header.Set("X-Amz-Trailer", o.trailerName)
	}
	now := time.Now().UTC()
	dateStr, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Host", req.Host)
	req.Header.Set("X-Amz-Content-Sha256", marker)
	signed := "host;x-amz-content-sha256;x-amz-date"
	key := deriveSigningKey(testSecretKey, dateStr, testRegion, "s3")
	seed := hex.EncodeToString(hmacSHA256(key, []byte(buildStringToSignAt(amzDate, dateStr, testRegion, "s3",
		buildCanonicalRequest(req, signed, "")))))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		testAccessKey, dateStr, testRegion, signed, seed))

	c := &chunkSigner{key: key, amzDate: amzDate, scope: dateStr + "/" + testRegion + "/s3/aws4_request", prev: seed}
	sign := func(data []byte) string {
		s := sha256.Sum256(data)
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.amzDate + "\n" + c.scope + "\n" + c.prev + "\n" + emptyPayloadSHA256 + "\n" + hex.EncodeToString(s[:])
		c.prev = hex.EncodeToString(hmacSHA256(c.key, []byte(sts)))
		return c.prev
	}
	var chunks []string
	for i, n := 0, 1; i < len(body); i, n = i+o.chunk, n+1 {
		data := body[i:min(i+o.chunk, len(body))]
		sig := sign(data)
		if n == o.tamperChunk {
			data = bytes.ToUpper(data)
		}
		ext := ";chunk-signature=" + sig
		if n == o.dropSigOnChunk {
			ext = ""
		}
		chunks = append(chunks, fmt.Sprintf("%x%s\r\n%s\r\n", len(data), ext, data))
	}
	if o.swapChunks && len(chunks) > 1 {
		chunks[0], chunks[1] = chunks[1], chunks[0]
	}
	final := "0;chunk-signature=" + sign(nil) + "\r\n"
	var tail string
	if o.trailerName != "" {
		line := o.trailerName + ":" + o.trailerValue
		sum := sha256.Sum256([]byte(line + "\n"))
		sts := "AWS4-HMAC-SHA256-TRAILER\n" + c.amzDate + "\n" + c.scope + "\n" + c.prev + "\n" + hex.EncodeToString(sum[:])
		sig := hex.EncodeToString(hmacSHA256(c.key, []byte(sts)))
		switch o.trailerSig {
		case "":
		case "-":
			sig = ""
		default:
			sig = o.trailerSig
		}
		tail = line + "\r\n"
		if o.minioLayout {
			// minio-go: the checksum line ends in a bare LF, then a blank
			// line, then the signature line.
			tail = line + "\n\r\n"
		}
		if sig != "" {
			tail += "x-amz-trailer-signature:" + sig + "\r\n"
		}
	}
	framed := strings.Join(chunks, "") + final + tail + "\r\n"
	req.Body = io.NopCloser(strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	return send(t, req)
}

// A signed streaming upload is checked chunk by chunk. An honest one is
// stored. Data altered after signing, chunks in the wrong order, or a chunk
// without its signature are refused with SignatureDoesNotMatch and store
// nothing, and an object already at the key is left as it was. Before, chunk
// signatures were never looked at, so every one of these was stored.
func TestSignedStreamingChunksAreVerified(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/sig", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("signed chunk data "), 2000) // 36 KB, 5 chunks of 8 KiB

	signedStreamPut(t, ts, "/sig/good", body, streamOpts{chunk: 8192}).must(t, "honest signed streaming")
	if got := call(t, ts, http.MethodGet, "/sig/good", "").must(t, "GET"); got.body != string(body) {
		t.Fatalf("stored %d bytes, want %d", len(got.body), len(body))
	}

	for _, tc := range []struct {
		name string
		o    streamOpts
	}{
		{"data altered after signing", streamOpts{chunk: 8192, tamperChunk: 3}},
		{"chunks out of order", streamOpts{chunk: 8192, swapChunks: true}},
		{"chunk without its signature", streamOpts{chunk: 8192, dropSigOnChunk: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := signedStreamPut(t, ts, "/sig/bad", body, tc.o)
			if r.code != http.StatusForbidden || !strings.Contains(r.body, "SignatureDoesNotMatch") {
				t.Fatalf("%d %s, want 403 SignatureDoesNotMatch", r.code, r.body)
			}
			if code, _ := headChecksum(t, ts, "/sig/bad", "crc32"); code != http.StatusNotFound {
				t.Fatalf("refused upload is readable: HEAD %d", code)
			}
			over := signedStreamPut(t, ts, "/sig/good", bytes.Repeat([]byte("x"), len(body)), tc.o)
			if over.code != http.StatusForbidden {
				t.Fatalf("refused overwrite: %d", over.code)
			}
			if got := call(t, ts, http.MethodGet, "/sig/good", ""); got.body != string(body) {
				t.Fatal("a refused overwrite changed the object")
			}
		})
	}
}

// The -TRAILER form signs the trailer too. An honest one stores its checksum;
// a wrong or missing trailer signature is refused.
func TestSignedStreamingTrailerIsVerified(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/strl", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("signed trailer "), 1000)
	name, value := "x-amz-checksum-crc32", checksumOf("crc32", body)

	signedStreamPut(t, ts, "/strl/good", body, streamOpts{chunk: 4096, trailerName: name, trailerValue: value}).must(t, "honest signed trailer, AWS layout")
	if code, got := headChecksum(t, ts, "/strl/good", "crc32"); code != 200 || got != value {
		t.Fatalf("HEAD %d %q, want 200 %q", code, got, value)
	}
	// minio-go lays the trailer out differently (a blank line before the
	// signature). A real minio-go upload was refused until this was handled.
	signedStreamPut(t, ts, "/strl/minio", body, streamOpts{chunk: 4096, trailerName: name, trailerValue: value, minioLayout: true}).must(t, "honest signed trailer, minio-go layout")
	if code, got := headChecksum(t, ts, "/strl/minio", "crc32"); code != 200 || got != value {
		t.Fatalf("minio-go layout: HEAD %d %q, want 200 %q", code, got, value)
	}
	if r := signedStreamPut(t, ts, "/strl/minio-bad", body, streamOpts{chunk: 4096, trailerName: name, trailerValue: value, trailerSig: strings.Repeat("ab", 32), minioLayout: true}); r.code != http.StatusForbidden {
		t.Fatalf("minio-go layout, wrong trailer signature: %d, want 403", r.code)
	}
	for _, tc := range []struct{ name, sig string }{
		{"wrong trailer signature", strings.Repeat("ab", 32)},
		{"no trailer signature", "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := signedStreamPut(t, ts, "/strl/bad", body, streamOpts{chunk: 4096, trailerName: name, trailerValue: value, trailerSig: tc.sig})
			if r.code != http.StatusForbidden || !strings.Contains(r.body, "SignatureDoesNotMatch") {
				t.Fatalf("%d %s, want 403 SignatureDoesNotMatch", r.code, r.body)
			}
			if code, _ := headChecksum(t, ts, "/strl/bad", "crc32"); code != http.StatusNotFound {
				t.Fatalf("refused upload is readable: HEAD %d", code)
			}
		})
	}
}

// Only a request signed with an Authorization header has a signature to chain
// chunks from. A presigned request claiming signed chunks passes its own
// authentication, so it is refused here rather than accepted unchecked.
func TestSignedStreamingNeedsAnAuthorizationHeader(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/spre", "").must(t, "create bucket")
	u := GeneratePresignedPutURL(ts.URL, "spre", "k", testAccessKey, testSecretKey, testRegion, time.Minute, nil)
	req, _ := http.NewRequest(http.MethodPut, u, strings.NewReader("5;chunk-signature=00\r\nhello\r\n0;chunk-signature=00\r\n\r\n"))
	req.Header.Set("X-Amz-Content-Sha256", signedStreaming)
	req.Header.Set("X-Amz-Decoded-Content-Length", "5")
	r := send(t, req)
	if r.code != http.StatusForbidden {
		t.Fatalf("presigned signed streaming: %d %s, want 403", r.code, r.body)
	}
	if code, _ := headChecksum(t, ts, "/spre/k", "crc32"); code != http.StatusNotFound {
		t.Fatalf("unverifiable chunks were stored: HEAD %d", code)
	}
}

// A signed upload whose body is not the one its signed SHA-256 names is
// refused with XAmzContentSHA256Mismatch, for a small body (checked up front)
// and a large one (checked as it streams), and an object already at the key is
// kept. Before, the hash was signed but never compared with the body.
func TestBodyMustMatchItsSignedSHA256(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/hash", "").must(t, "create bucket")
	for _, size := range []int{1000, 3 << 20} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			signedFor := bytes.Repeat([]byte("s"), size)
			sent := bytes.Repeat([]byte("t"), size)
			path := fmt.Sprintf("/hash/k%d", size)
			call(t, ts, http.MethodPut, path, string(signedFor)).must(t, "honest PUT")

			req, _ := http.NewRequest(http.MethodPut, ts.URL+path, nil)
			req.ContentLength = int64(size)
			signV4Request(req, testAccessKey, testSecretKey, signedFor)
			req.Body = io.NopCloser(bytes.NewReader(sent))
			r := send(t, req)
			if r.code != http.StatusBadRequest || !strings.Contains(r.body, "XAmzContentSHA256Mismatch") {
				t.Fatalf("%d %s, want 400 XAmzContentSHA256Mismatch", r.code, r.body)
			}
			if got := call(t, ts, http.MethodGet, path, ""); got.body != string(signedFor) {
				t.Fatal("a refused body replaced the object")
			}
		})
	}
}

// Configuration bodies are XML that a handler may stop reading at its closing
// tag, so they are checked in full before the handler runs. A tampered one
// must not be applied.
func TestTamperedConfigurationBodyIsNotApplied(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/cfg", "").must(t, "create bucket")
	signed := `<Tagging><TagSet><Tag><Key>team</Key><Value>a</Value></Tag></TagSet></Tagging>`
	sent := `<Tagging><TagSet><Tag><Key>team</Key><Value>b</Value></Tag></TagSet></Tagging>`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/cfg?tagging", nil)
	req.ContentLength = int64(len(sent))
	signV4Request(req, testAccessKey, testSecretKey, []byte(signed))
	req.Body = io.NopCloser(strings.NewReader(sent))
	if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "XAmzContentSHA256Mismatch") {
		t.Fatalf("%d %s, want 400 XAmzContentSHA256Mismatch", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/cfg?tagging", ""); strings.Contains(got.body, "<Value>b</Value>") {
		t.Fatal("the tampered tagging was applied")
	}
}

// exactSizeServer is a full server whose engine reads exactly the declared
// size, like erasure coding, and the directory holding its data.
func exactSizeServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
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
	return ts, filepath.Join(dir, "data")
}

// Regression (independent review): on an engine that stops at the declared
// size, a refused upload used to be checked only after the engine had saved
// it. An overwrite then served the refused bytes, and a new key kept them on
// disk. The checks now run inside the engine's read, before anything is saved.
func TestRefusedUploadOnExactSizeEngineChangesNothing(t *testing.T) {
	ts, data := exactSizeServer(t)
	call(t, ts, http.MethodPut, "/exb", "").must(t, "create bucket")
	orig := bytes.Repeat([]byte("original "), 100)
	call(t, ts, http.MethodPut, "/exb/k", string(orig)).must(t, "original")
	repl := bytes.Repeat([]byte("REPLACED "), 50)
	name := "x-amz-checksum-crc32"
	lateTail = true
	t.Cleanup(func() { lateTail = false })

	if r := trailerPut(t, ts, "/exb/k", repl, name, name+":"+wrongChecksumOf("crc32", repl)+"\r\n"); r.code != http.StatusBadRequest {
		t.Fatalf("wrong trailer on overwrite: %d", r.code)
	}
	if got := call(t, ts, http.MethodGet, "/exb/k", ""); got.body != string(orig) {
		t.Fatalf("refused overwrite changed the object: %d bytes", len(got.body))
	}
	trailerPut(t, ts, "/exb/fresh", repl, name, name+":"+wrongChecksumOf("crc32", repl)+"\r\n")
	if _, err := os.Stat(filepath.Join(data, "exb", "fresh")); err == nil {
		t.Fatal("refused upload left its bytes on disk")
	}
}

// Regression (independent review): a body longer than its declared length,
// with a checksum of the whole body, used to store the first part under a
// checksum that did not match it.
func TestBodyLongerThanDeclaredIsRefused(t *testing.T) {
	ts, _ := exactSizeServer(t)
	call(t, ts, http.MethodPut, "/exb", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("abcdefgh"), 100)
	name := "x-amz-checksum-crc32"
	for _, tc := range []struct {
		name, path, trailer string
	}{
		// With a checksum of the whole body, which used to match.
		{"with a whole-body checksum", "/exb/short1", fmt.Sprintf("%s:%s\r\n", name, checksumOf("crc32", body))},
		// With no checksum at all, only the length check can refuse it.
		{"with no checksum", "/exb/short2", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			framed := fmt.Sprintf("%x\r\n%s\r\n0\r\n%s\r\n", len(body), body, tc.trailer)
			req, _ := http.NewRequest(http.MethodPut, ts.URL+tc.path, strings.NewReader(framed))
			req.ContentLength = int64(len(framed))
			req.Header.Set("Content-Encoding", "aws-chunked")
			req.Header.Set("X-Amz-Decoded-Content-Length", "100")
			if tc.trailer != "" {
				req.Header.Set("X-Amz-Trailer", name)
			}
			signStreamingTrailer(req)
			if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "IncompleteBody") {
				t.Fatalf("800-byte body declared as 100: %d %s, want 400 IncompleteBody", r.code, r.body)
			}
			if code, _ := headChecksum(t, ts, tc.path, "crc32"); code != http.StatusNotFound {
				t.Fatalf("refused upload is readable: HEAD %d", code)
			}
		})
	}
}

// endless is a body that never sends a line ending.
type endless struct{ n int64 }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	e.n += int64(len(p))
	if e.n > 256<<20 {
		return len(p), io.EOF
	}
	return len(p), nil
}

// Regression (independent review): the trailer size cap was checked after a
// whole line had been read, so a line with no end grew memory without bound.
func TestTrailerReadIsBoundedInMemory(t *testing.T) {
	src := io.MultiReader(strings.NewReader("3\r\nabc\r\n0\r\n"), &endless{})
	r := httptest.NewRequest(http.MethodPut, "/b/k", src)
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	maybeDecodeAwsChunked(r)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := io.Copy(io.Discard, r.Body)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("an endless trailer line was accepted")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Fatalf("reading the trailer allocated %d MiB", grew>>20)
	}
}

// carelessEngine ignores a failed read and saves whatever arrived, as a buggy
// or third-party engine might.
type carelessEngine struct{ storage.Engine }

func (e carelessEngine) PutObject(bucket, key string, r io.Reader, size int64) (int64, string, error) {
	data, _ := io.ReadAll(r)
	return e.Engine.PutObject(bucket, key, bytes.NewReader(data), int64(len(data)))
}

// Even an engine that ignores a failed read cannot keep a refused upload: the
// failure the body reader met is recorded, answered with its own code, and the
// saved bytes are removed.
func TestRefusalSurvivesAnEngineThatIgnoresReadErrors(t *testing.T) {
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
	ts := httptest.NewServer(NewHandler(store, carelessEngine{fs}, NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil), false, "", nil))
	t.Cleanup(ts.Close)

	call(t, ts, http.MethodPut, "/careless", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("careless "), 2000)
	r := signedStreamPut(t, ts, "/careless/k", body, streamOpts{chunk: 4096, tamperChunk: 2})
	if r.code != http.StatusForbidden || !strings.Contains(r.body, "SignatureDoesNotMatch") {
		t.Fatalf("%d %s, want 403 SignatureDoesNotMatch", r.code, r.body)
	}
	if code, _ := headChecksum(t, ts, "/careless/k", "crc32"); code != http.StatusNotFound {
		t.Fatalf("refused upload is readable: HEAD %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "careless", "k")); err == nil {
		t.Fatal("refused upload left its bytes on disk")
	}
}
