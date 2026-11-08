package s3

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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
	signed := signedHeaderList(req)
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

// A presigned URL cannot be given an X-Amz-Decoded-Content-Length it did not
// sign, to claim a length for chunks nothing can verify: it is refused at
// authentication and names the header. Without that header, the request
// reaches chunkSignerFor's own refusal, which
// TestPresignedRequestClaimingSignedChunksIsRefused pins.
func TestPresignedUploadWithUnsignedDecodedLengthIsRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/spre", "").must(t, "create bucket")
	u := GeneratePresignedPutURL(ts.URL, "spre", "k", testAccessKey, testSecretKey, testRegion, time.Minute, nil)
	req, _ := http.NewRequest(http.MethodPut, u, strings.NewReader("5;chunk-signature=00\r\nhello\r\n0;chunk-signature=00\r\n\r\n"))
	req.Header.Set("X-Amz-Content-Sha256", signedStreaming)
	req.Header.Set("X-Amz-Decoded-Content-Length", "5")
	r := send(t, req)
	if r.code != http.StatusForbidden || !strings.Contains(r.body, "AccessDenied") || !strings.Contains(r.body, "x-amz-decoded-content-length") {
		t.Fatalf("presigned signed streaming: %d %s, want 403 AccessDenied naming x-amz-decoded-content-length", r.code, r.body)
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

// Regression (second independent review): outside an object upload, a body
// was only checked when it was at most 1 MiB with a known length, and most
// handlers stop at the closing XML tag, so a check at EOF never ran. A
// tampered multi-object delete over 1 MiB deleted the wrong object, and a
// tampered configuration sent chunked was applied. Every such body is now read
// and checked in full before its handler runs.
func TestTamperedNonObjectBodiesAreNeverCarriedOut(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/nonobj", "").must(t, "create bucket")
	call(t, ts, http.MethodPut, "/nonobj/victim", "precious").must(t, "victim")
	call(t, ts, http.MethodPut, "/nonobj/other1", "x").must(t, "other")

	t.Run("multi-object delete over 1 MiB", func(t *testing.T) {
		pad := strings.Repeat(" ", 1<<20+100)
		signed := `<Delete><Object><Key>other1</Key></Object></Delete>` + pad
		sent := `<Delete><Object><Key>victim</Key></Object></Delete>` + pad
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/nonobj?delete", nil)
		req.ContentLength = int64(len(sent))
		signV4Request(req, testAccessKey, testSecretKey, []byte(signed))
		req.Body = io.NopCloser(strings.NewReader(sent))
		if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "XAmzContentSHA256Mismatch") {
			t.Fatalf("%d, want 400 XAmzContentSHA256Mismatch", r.code)
		}
		if got := call(t, ts, http.MethodGet, "/nonobj/victim", ""); got.code != 200 {
			t.Fatalf("tampered delete list was carried out: victim GET %d", got.code)
		}
	})

	// versioning uses xml.Decoder, which stops at the closing tag. Tagging,
	// which an earlier test used, reads to EOF and so hid this.
	t.Run("configuration sent chunked", func(t *testing.T) {
		signed := `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/nonobj?versioning", nil)
		signV4Request(req, testAccessKey, testSecretKey, []byte(signed))
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte(versioningOn))
			time.Sleep(300 * time.Millisecond)
			_ = pw.Close()
		}()
		req.Body = pr
		req.ContentLength = -1
		if r := send(t, req); r.code != http.StatusBadRequest {
			t.Fatalf("%d %s, want 400", r.code, r.body)
		}
		if got := call(t, ts, http.MethodGet, "/nonobj?versioning", ""); strings.Contains(got.body, "Enabled") {
			t.Fatal("tampered versioning was applied")
		}
	})

	t.Run("body over the verified limit", func(t *testing.T) {
		big := strings.Repeat(" ", maxVerifiedBody+10)
		if r := call(t, ts, http.MethodPost, "/nonobj?delete", big); r.code != http.StatusBadRequest || !strings.Contains(r.body, "MaxMessageLengthExceeded") {
			t.Fatalf("%d, want 400 MaxMessageLengthExceeded", r.code)
		}
	})
}

// Regression (second independent review): an empty body sent with the hash
// of a non-empty one replaced the object with nothing. It must match the hash
// of an empty body.
func TestEmptyBodyMustMatchTheEmptyHash(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/empt", "").must(t, "create bucket")
	call(t, ts, http.MethodPut, "/empt/k", "original").must(t, "original")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/empt/k", nil)
	signV4Request(req, testAccessKey, testSecretKey, []byte("the signed body"))
	req.Body = http.NoBody
	req.ContentLength = 0
	if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "XAmzContentSHA256Mismatch") {
		t.Fatalf("%d %s, want 400 XAmzContentSHA256Mismatch", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/empt/k", ""); got.body != "original" {
		t.Fatalf("object replaced with %q", got.body)
	}
	call(t, ts, http.MethodPut, "/empt/empty", "").must(t, "an honest empty object")
}

// snowballArchive is a tar with a small entry and a 2 MiB one.
func snowballArchive(t *testing.T, small string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range []struct {
		name string
		data []byte
	}{{"a.txt", []byte(small)}, {"big.bin", bytes.Repeat([]byte("z"), 2<<20)}} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Regression (second independent review): a Snowball archive's entries were
// stored while it streamed, and tar stops before the body's end, so a tampered
// archive was extracted. A signed archive is now verified before extraction.
func TestTamperedSnowballArchiveIsNotExtracted(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/snowb", "").must(t, "create bucket")
	honest := snowballArchive(t, "honest")

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/snowb/archive.tar", nil)
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	req.ContentLength = int64(len(honest))
	signV4Request(req, testAccessKey, testSecretKey, honest)
	req.Body = io.NopCloser(bytes.NewReader(snowballArchive(t, "EVILLL")))
	if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "XAmzContentSHA256Mismatch") {
		t.Fatalf("%d %s, want 400 XAmzContentSHA256Mismatch", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/snowb/a.txt", ""); got.code != http.StatusNotFound {
		t.Fatalf("an entry of the tampered archive was stored: %d %q", got.code, got.body)
	}

	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/snowb/archive.tar", bytes.NewReader(honest))
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	req.ContentLength = int64(len(honest))
	signV4Request(req, testAccessKey, testSecretKey, honest)
	send(t, req)
	if got := call(t, ts, http.MethodGet, "/snowb/a.txt", ""); got.body != "honest" {
		t.Fatalf("honest archive: a.txt = %d %q", got.code, got.body)
	}
}

// A trailer checksum that was not announced in X-Amz-Trailer was copied but
// never hashed, so it was neither checked nor stored. It is refused.
func TestUnannouncedTrailerChecksumIsRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/unann", "").must(t, "create bucket")
	body := []byte("unannounced")
	r := trailerPut(t, ts, "/unann/k", body, "", "x-amz-checksum-crc32:"+wrongChecksumOf("crc32", body)+"\r\n")
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "MalformedTrailerError") {
		t.Fatalf("%d %s, want 400 MalformedTrailerError", r.code, r.body)
	}
}

// Large object uploads stream (they are not buffered), including presigned
// ones and those with the x-id query parameter SDKs add. Buffering them would
// refuse anything over 8 MiB.
func TestLargeObjectUploadsStillStream(t *testing.T) {
	for _, tc := range []struct {
		method, query string
		want          bool
	}{
		{http.MethodPut, "", true},
		{http.MethodPut, "x-id=PutObject", true},
		{http.MethodPut, "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=ab&X-Amz-Expires=60", true},
		{http.MethodPut, "partNumber=1&uploadId=u&x-id=UploadPart", true},
		{http.MethodPut, "tagging", false},
		{http.MethodPut, "retention", false},
		{http.MethodPut, "acl", false},
		{http.MethodPut, "legal-hold", false},
		{http.MethodPost, "delete", false},
		// Fifth review: any other query parameter still routes to PutObject
		// or UploadPart in the dispatcher, so it must still stream.
		{http.MethodPut, "versionId=null", true},
		{http.MethodPut, "foo=bar", true},
		{http.MethodPut, "partNumber=2&uploadId=u&foo=bar", true},
		{http.MethodPut, "uploadId=u&tagging", false},
	} {
		r := httptest.NewRequest(tc.method, "/b/k?"+tc.query, nil)
		if got := streamsToEngine(r, "k"); got != tc.want {
			t.Errorf("%s ?%s: streams=%v, want %v", tc.method, tc.query, got, tc.want)
		}
	}
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/bigup", "").must(t, "create bucket")
	big := bytes.Repeat([]byte("L"), maxVerifiedBody+(1<<20))
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/bigup/k?x-id=PutObject", bytes.NewReader(big))
	req.ContentLength = int64(len(big))
	signV4Request(req, testAccessKey, testSecretKey, big)
	send(t, req).must(t, "9 MiB PUT with a signed hash")

	// An extra query parameter does not make an upload a configuration body.
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/bigup/k2?foo=bar", bytes.NewReader(big))
	req.ContentLength = int64(len(big))
	signV4Request(req, testAccessKey, testSecretKey, big)
	send(t, req).must(t, "9 MiB PUT with a signed hash and an extra query parameter")
}

// signWithoutContentHash signs like a minimal SigV4 client: host and
// x-amz-date only, no X-Amz-Content-Sha256, which authentication treats as a
// request with an empty body.
// extra names further headers to sign, in sorted order after x-amz-date.
func signWithoutContentHash(r *http.Request, extra ...string) {
	now := time.Now().UTC()
	dateStr, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("Host", r.Host)
	signed := strings.Join(append([]string{"host", "x-amz-date"}, extra...), ";")
	sts := buildStringToSignAt(amzDate, dateStr, testRegion, "s3", buildCanonicalRequest(r, signed, ""))
	sig := hex.EncodeToString(hmacSHA256(deriveSigningKey(testSecretKey, dateStr, testRegion, "s3"), []byte(sts)))
	r.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		testAccessKey, dateStr, testRegion, signed, sig))
}

// Regression (third review): a header-signed request without
// X-Amz-Content-Sha256 is signed as an empty body, so a body attached to it
// must be refused, not stored.
func TestBodyOnARequestSignedAsEmptyIsRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/nohash", "").must(t, "create bucket")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/nohash/obj", nil)
	signWithoutContentHash(req)
	evil := "attacker content"
	req.Body = io.NopCloser(strings.NewReader(evil))
	req.ContentLength = int64(len(evil))
	if r := send(t, req); r.code != http.StatusBadRequest {
		t.Fatalf("%d %s, want 400", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/nohash/obj", ""); got.code != http.StatusNotFound {
		t.Fatalf("attacker body stored: %d %q", got.code, got.body)
	}
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/nohash/empty", nil)
	signWithoutContentHash(req)
	send(t, req).must(t, "an honest empty PUT signed without the header")
}

// Regression (seventh review): a form upload is read through the boundary in
// its Content-Type, and the body hash covers the bytes, not how they split.
// With Content-Type unsigned, a boundary swapped on the way for one inside the
// uploaded file parsed a different form (another key, another file) out of
// the same signed bytes. A signed form upload must sign Content-Type.
func TestFormUploadMustSignItsContentType(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/formct", "").must(t, "create bucket")
	// A file that happens to hold a form of its own.
	var inner bytes.Buffer
	iw := multipart.NewWriter(&inner)
	_ = iw.SetBoundary("INNERBOUNDARY")
	_ = iw.WriteField("key", "evil.txt")
	fw, _ := iw.CreateFormFile("file", "evil.txt")
	_, _ = fw.Write([]byte("EVIL CONTENT"))
	_ = iw.Close()
	var outer bytes.Buffer
	ow := multipart.NewWriter(&outer)
	_ = ow.SetBoundary("OUTERBOUNDARY")
	_ = ow.WriteField("key", "report.txt")
	ff, _ := ow.CreateFormFile("file", "report.txt")
	_, _ = ff.Write(inner.Bytes())
	_ = ow.Close()
	body := outer.Bytes()

	post := func(signCT bool, sentCT string) result {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/formct", nil)
		if signCT {
			req.Header.Set("Content-Type", ow.FormDataContentType())
		}
		signV4Request(req, testAccessKey, testSecretKey, body)
		req.Header.Set("Content-Type", sentCT)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		return send(t, req)
	}
	// Content-Type left out of the signature: refused, whatever it says.
	for _, ct := range []string{ow.FormDataContentType(), "multipart/form-data; boundary=INNERBOUNDARY"} {
		if r := post(false, ct); r.code != http.StatusForbidden || !strings.Contains(r.body, "not signed: content-type") {
			t.Fatalf("form upload with unsigned Content-Type %q: %d %s, want 403 naming content-type", ct, r.code, r.body)
		}
	}
	// Signed, and swapped on the way: the signature breaks.
	if r := post(true, "multipart/form-data; boundary=INNERBOUNDARY"); r.code != http.StatusForbidden || !strings.Contains(r.body, "SignatureDoesNotMatch") {
		t.Fatalf("form upload with a swapped boundary: %d %s, want 403 SignatureDoesNotMatch", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/formct/evil.txt", ""); got.code != http.StatusNotFound {
		t.Fatalf("the inner form was stored: evil.txt = %q", got.body)
	}
	// Signed and unchanged: stored under the signed form's key.
	if r := post(true, ow.FormDataContentType()); r.code >= 300 {
		t.Fatalf("honest signed form upload: %d %s", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/formct/report.txt", ""); got.body != inner.String() {
		t.Fatalf("report.txt = %q, want the uploaded file", got.body)
	}
}

// Regression (seventh review): a SignedHeaders name written with a space
// counted as signed for the unsigned-header check, but the canonical request
// looked the untrimmed name up, found nothing, and signed an empty value, so
// the header could be changed on the way. Names are trimmed in both.
func TestSignedHeaderNameWithASpaceBindsItsValue(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/spc", "").must(t, "create bucket")
	call(t, ts, http.MethodPut, "/spc/secret", "TOP-SECRET").must(t, "secret")
	call(t, ts, http.MethodPut, "/spc/public", "public").must(t, "public")
	now := time.Now().UTC()
	dateStr, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/spc/dst", nil)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Host", req.Host)
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadSHA256)
	req.Header.Set("X-Amz-Copy-Source", "/spc/public")
	signed := "host; x-amz-copy-source;x-amz-content-sha256;x-amz-date"
	// The client's canonical request binds the value it means, as a signer
	// that trims would.
	cr := strings.Replace(buildCanonicalRequest(req, strings.ReplaceAll(signed, " ", ""), ""),
		"\nhost;x-amz-copy-source;", "\nhost; x-amz-copy-source;", 1)
	sts := buildStringToSignAt(amzDate, dateStr, testRegion, "s3", cr)
	sig := hex.EncodeToString(hmacSHA256(deriveSigningKey(testSecretKey, dateStr, testRegion, "s3"), []byte(sts)))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		testAccessKey, dateStr, testRegion, signed, sig))
	honest := req.Clone(req.Context())
	// Changed on the way: the signature no longer matches.
	req.Header.Set("X-Amz-Copy-Source", "/spc/secret")
	if r := send(t, req); r.code != http.StatusForbidden || !strings.Contains(r.body, "SignatureDoesNotMatch") {
		t.Fatalf("copy source changed on the way: %d %s, want 403 SignatureDoesNotMatch", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/spc/dst", ""); got.code != http.StatusNotFound {
		t.Fatalf("dst = %q", got.body)
	}
	// Unchanged: the copy happens.
	send(t, honest).must(t, "the honest copy")
	if got := call(t, ts, http.MethodGet, "/spc/dst", ""); got.body != "public" {
		t.Fatalf("dst = %q, want public", got.body)
	}
}

// Regression (sixth review): a Snowball archive on a request signed without
// X-Amz-Content-Sha256, so signed as an empty body, was taken as unsigned
// and extracted while it was read, before the empty-body check ran.
func TestSnowballArchiveOnARequestSignedAsEmptyIsRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/snowempty", "").must(t, "create bucket")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/snowempty/archive.tar", nil)
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	signWithoutContentHash(req, "x-amz-meta-snowball-auto-extract")
	evil := snowballArchive(t, "attacker content")
	req.Body = io.NopCloser(bytes.NewReader(evil))
	req.ContentLength = int64(len(evil))
	if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "XAmzContentSHA256Mismatch") {
		t.Fatalf("%d %s, want 400 XAmzContentSHA256Mismatch", r.code, r.body)
	}
	for _, k := range []string{"a.txt", "big.bin", "archive.tar"} {
		if got := call(t, ts, http.MethodGet, "/snowempty/"+k, ""); got.code != http.StatusNotFound {
			t.Fatalf("%s stored from an archive signed as empty: %d", k, got.code)
		}
	}
}

// Regression (third review): an unsigned X-Amz-Decoded-Content-Length of 0
// made a configuration body look empty, so it skipped the check and a body
// framed as aws-chunked was applied.
func TestDecodedLengthZeroCannotHideAConfigurationBody(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/dlzero", "").must(t, "create bucket")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/dlzero?versioning", nil)
	// The length header is signed here, so this pins the full-read guard
	// itself, not the refusal of unsigned headers.
	req.Header.Set("X-Amz-Decoded-Content-Length", "0")
	signV4Request(req, testAccessKey, testSecretKey, nil)
	req.Header.Set("Content-Encoding", "aws-chunked")
	framed := fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(versioningOn), versioningOn)
	req.Body = io.NopCloser(strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	send(t, req)
	if got := call(t, ts, http.MethodGet, "/dlzero?versioning", ""); strings.Contains(got.body, "Enabled") {
		t.Fatal("injected versioning configuration was applied")
	}
}

// countingReader counts how much of a body the server read.
type countingReader struct {
	n   atomic.Int64
	max int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.n.Load() >= c.max {
		return 0, io.EOF
	}
	n := len(p)
	if rem := c.max - c.n.Load(); int64(n) > rem {
		n = int(rem)
	}
	c.n.Add(int64(n))
	return n, nil
}

// Regression (third review): an anonymous request (here a GET on a public
// object) with an X-Amz-Content-Sha256 header made the server read and hash up
// to 8 MiB into memory. Only header-signed PUT and POST bodies are checked:
// the method rule covers the GET, the Authorization rule the presigned PUT.
func TestAnonymousRequestsAreNotBufferedForTheHashCheck(t *testing.T) {
	ts, store := fullServer(t)
	call(t, ts, http.MethodPut, "/anonbuf", "").must(t, "create bucket")
	call(t, ts, http.MethodPut, "/anonbuf/public/a", "hi").must(t, "object")
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::anonbuf/public/*"]}]}`
	if err := store.PutBucketPolicy("anonbuf", []byte(policy)); err != nil {
		t.Fatal(err)
	}
	cr := &countingReader{max: maxVerifiedBody}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/anonbuf/public/a", cr)
	req.ContentLength = maxVerifiedBody
	s := sha256.Sum256([]byte("whatever"))
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(s[:]))
	r := send(t, req)
	if r.code != http.StatusOK || r.body != "hi" {
		t.Fatalf("anonymous GET: %d %q, want 200 \"hi\" (it read %d body bytes)", r.code, r.body, cr.n.Load())
	}
	// Counted on the client side: net/http drains up to 256 KiB after the
	// handler, plus TCP buffers. Reading it for the check would be 8 MiB.
	if n := cr.n.Load(); n > 2<<20 {
		t.Fatalf("the anonymous body was read to %d bytes", n)
	}

	// Anonymous writes are never allowed, so the PUT that reaches this rule
	// without an Authorization header is a presigned one. It signs
	// UNSIGNED-PAYLOAD, so an X-Amz-Content-Sha256 header on it vouches for
	// nothing and is not compared with the body.
	u := GeneratePresignedPutURL(ts.URL, "anonbuf", "public/b", testAccessKey, testSecretKey, testRegion, time.Minute, nil)
	req, _ = http.NewRequest(http.MethodPut, u, strings.NewReader("presigned"))
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(s[:]))
	if r := send(t, req); r.code != http.StatusOK {
		t.Fatalf("presigned PUT with an unrelated hash header: %d %q, want 200", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/anonbuf/public/b", ""); got.body != "presigned" {
		t.Fatalf("presigned PUT stored %q", got.body)
	}
}

// Only PUT and POST bodies are acted on, so only theirs are checked: a signed
// GET whose X-Amz-Content-Sha256 names a body it does not carry is answered,
// and its body is not read for the check.
func TestSignedGetBodyIsNotCheckedOrRead(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/getbody", "").must(t, "create bucket")
	call(t, ts, http.MethodPut, "/getbody/k", "hi").must(t, "object")
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/getbody/k", nil)
	signV4Request(req, testAccessKey, testSecretKey, []byte("a body this GET never sends"))
	cr := &countingReader{max: maxVerifiedBody}
	req.Body = io.NopCloser(cr)
	req.ContentLength = maxVerifiedBody
	if r := send(t, req); r.code != http.StatusOK || r.body != "hi" {
		t.Fatalf("signed GET with a body: %d %q, want 200 \"hi\" (read %d body bytes)", r.code, r.body, cr.n.Load())
	}
	if n := cr.n.Load(); n > 2<<20 {
		t.Fatalf("the GET body was read to %d bytes", n)
	}
}

// A plain (not aws-chunked) upload that names a trailer has no trailer: the
// declaration is dropped, so the upload is stored rather than refused with
// MalformedTrailerError for a trailer that cannot exist.
func TestTrailerDeclarationOnAPlainBodyIsIgnored(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/plaintrail", "").must(t, "create bucket")
	body := []byte("plain body")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/plaintrail/k", nil)
	req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	signV4Request(req, testAccessKey, testSecretKey, body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if r := send(t, req); r.code != http.StatusOK {
		t.Fatalf("plain PUT declaring a trailer: %d %s, want 200", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/plaintrail/k", ""); got.body != string(body) {
		t.Fatalf("stored %q", got.body)
	}
}

// Regression (compatibility review): signed archives were staged in the system
// temp directory, so a container with a read-only root filesystem answered 500.
// They are staged on the data volume, and the staging file is always removed.
func TestSnowballStagingNeedsNoSystemTempDir(t *testing.T) {
	ts, data := exactSizeServer(t)
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	t.Setenv("TMPDIR", ro)

	call(t, ts, http.MethodPut, "/snowro", "").must(t, "create bucket")
	honest := snowballArchive(t, "honest")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/snowro/a.tar", bytes.NewReader(honest))
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	req.ContentLength = int64(len(honest))
	signV4Request(req, testAccessKey, testSecretKey, honest)
	send(t, req).must(t, "signed archive with an unwritable system temp dir")
	if got := call(t, ts, http.MethodGet, "/snowro/a.txt", ""); got.body != "honest" {
		t.Fatalf("a.txt = %d %q", got.code, got.body)
	}

	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/snowro/b.tar", nil)
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	req.ContentLength = int64(len(honest))
	signV4Request(req, testAccessKey, testSecretKey, honest)
	req.Body = io.NopCloser(bytes.NewReader(snowballArchive(t, "EVILLL")))
	if r := send(t, req); r.code != http.StatusBadRequest {
		t.Fatalf("tampered archive: %d", r.code)
	}

	left, err := os.ReadDir(filepath.Join(data, snowballStagingDir))
	if err != nil {
		t.Fatalf("staging dir: %v", err)
	}
	if len(left) > 0 {
		t.Fatalf("%d staged archives left behind", len(left))
	}
}

// Regression (third review): an x-amz-* header the client did not sign could
// be added on the way and changed what a signed upload did, around every body
// check. Each is now refused with 403 and changes nothing.
func TestUnsignedAmzHeadersAreRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/unsig", "").must(t, "create bucket")
	call(t, ts, http.MethodPut, "/unsig/secret", "TOP-SECRET").must(t, "secret")
	body := "the client's own body"
	honest := snowballArchive(t, "x")

	for _, tc := range []struct {
		name    string
		path    string
		body    []byte
		headers []string
	}{
		{"copy source added to an upload", "/unsig/dst", []byte(body), []string{"X-Amz-Copy-Source", "/unsig/secret"}},
		{"Snowball header added to an upload", "/unsig/backup.tar", honest, []string{"X-Amz-Meta-Snowball-Auto-Extract", "true"}},
		{"SSE-C key added to an upload", "/unsig/sealed", []byte(body), []string{
			"X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256",
			"X-Amz-Server-Side-Encryption-Customer-Key", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
			"X-Amz-Server-Side-Encryption-Customer-Key-Md5", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPut, ts.URL+tc.path, nil)
			req.ContentLength = int64(len(tc.body))
			signV4Request(req, testAccessKey, testSecretKey, tc.body)
			for i := 0; i+1 < len(tc.headers); i += 2 {
				req.Header.Set(tc.headers[i], tc.headers[i+1]) // added after signing
			}
			req.Body = io.NopCloser(bytes.NewReader(tc.body))
			if r := send(t, req); r.code != http.StatusForbidden || !strings.Contains(r.body, "AccessDenied") {
				t.Fatalf("%d %s, want 403 AccessDenied", r.code, r.body)
			}
			if got := call(t, ts, http.MethodGet, tc.path, ""); got.code != http.StatusNotFound {
				t.Fatalf("the request was carried out: GET %s = %d", tc.path, got.code)
			}
			if got := call(t, ts, http.MethodGet, "/unsig/a.txt", ""); got.code != http.StatusNotFound {
				t.Fatal("an archive entry was extracted")
			}
		})
	}
	// The same headers, signed, are fine.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/unsig/copied", nil)
	req.Header.Set("X-Amz-Copy-Source", "/unsig/secret")
	signV4Request(req, testAccessKey, testSecretKey, nil)
	send(t, req).must(t, "a signed copy")
	if got := call(t, ts, http.MethodGet, "/unsig/copied", ""); got.body != "TOP-SECRET" {
		t.Fatalf("signed copy = %q", got.body)
	}
}

// Regression (fourth review): presigned URLs accepted unsigned x-amz-*
// headers, so the holder of an upload URL (or anyone on the path) could copy
// any object the signer could read into the URL's key, or extract an archive
// over keys outside it. AWS refuses unsigned x-amz-* headers here too.
func TestPresignedURLsRefuseUnsignedAmzHeaders(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/drop", "").must(t, "bucket drop")
	e.admin(t, http.MethodPut, "/private", "").must(t, "bucket private")
	e.admin(t, http.MethodPut, "/private/secret.txt", "TOP-SECRET").must(t, "secret")
	e.admin(t, http.MethodPut, "/drop/important.txt", "ORIGINAL").must(t, "important")
	host := strings.TrimPrefix(e.ts.URL, "http://")
	mk := func(key string) string {
		return GeneratePresignedPutURL(host, "drop", key, testAccessKey, testSecretKey, testRegion, time.Hour,
			&PresignedUploadRestrictions{MaxSize: 1 << 20, AllowTypes: "text/plain", RequirePrefix: "inbox/"})
	}

	req, _ := http.NewRequest(http.MethodPut, mk("inbox/a.txt"), nil)
	req.Header.Set("X-Amz-Copy-Source", "/private/secret.txt")
	req.Header.Set("Content-Type", "text/plain")
	if r := send(t, req); r.code != http.StatusForbidden {
		t.Fatalf("copy through a presigned URL: %d %s, want 403", r.code, r.body)
	}
	if got := e.admin(t, http.MethodGet, "/drop/inbox/a.txt", ""); got.code != http.StatusNotFound {
		t.Fatalf("the secret was copied: %q", got.body)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	data := []byte("OVERWRITTEN")
	_ = tw.WriteHeader(&tar.Header{Name: "important.txt", Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(data)
	_ = tw.Close()
	req, _ = http.NewRequest(http.MethodPut, mk("inbox/b.tar"), bytes.NewReader(buf.Bytes()))
	req.ContentLength = int64(buf.Len())
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	if r := send(t, req); r.code != http.StatusForbidden {
		t.Fatalf("Snowball through a presigned URL: %d %s, want 403", r.code, r.body)
	}
	if got := e.admin(t, http.MethodGet, "/drop/important.txt", ""); got.body != "ORIGINAL" {
		t.Fatalf("important.txt was overwritten: %q", got.body)
	}

	// An ordinary upload through the URL still works.
	req, _ = http.NewRequest(http.MethodPut, mk("inbox/ok.txt"), strings.NewReader("fine"))
	req.Header.Set("Content-Type", "text/plain")
	send(t, req).must(t, "a plain presigned upload")
}

// signedStreamingCall sends body as signed chunks, chunk signed over body and
// then altered by tamper on the way.
func signedStreamingCall(t *testing.T, base, method, path string, body []byte, tamper func([]byte) []byte) result {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, nil)
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(body)))
	now := time.Now().UTC()
	dateStr, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Host", req.Host)
	req.Header.Set("X-Amz-Content-Sha256", signedStreaming)
	signed := signedHeaderList(req)
	key := deriveSigningKey(testSecretKey, dateStr, testRegion, "s3")
	seed := hex.EncodeToString(hmacSHA256(key, []byte(buildStringToSignAt(amzDate, dateStr, testRegion, "s3",
		buildCanonicalRequest(req, signed, "")))))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		testAccessKey, dateStr, testRegion, signed, seed))
	c := &chunkSigner{key: key, amzDate: amzDate, scope: dateStr + "/" + testRegion + "/s3/aws4_request", prev: seed}
	sign := func(d []byte) string {
		s := sha256.Sum256(d)
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.amzDate + "\n" + c.scope + "\n" + c.prev + "\n" + emptyPayloadSHA256 + "\n" + hex.EncodeToString(s[:])
		c.prev = hex.EncodeToString(hmacSHA256(c.key, []byte(sts)))
		return c.prev
	}
	sig := sign(body)
	sent := tamper(append([]byte(nil), body...))
	framed := fmt.Sprintf("%x;chunk-signature=%s\r\n%s\r\n", len(sent), sig, sent) + "0;chunk-signature=" + sign(nil) + "\r\n\r\n"
	req.Body = io.NopCloser(strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	return send(t, req)
}

// Regression (fourth review): a configuration body sent as signed chunks was
// acted on before its chunk signature was checked, because the XML decoder
// stops at the closing tag. A tampered delete list deleted the wrong object,
// and a tampered object lock default was applied. Honest ones still work.
func TestSignedStreamingConfigurationBodiesAreCheckedFirst(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/bdx", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/bdx/keep.txt", "KEEP").must(t, "keep")
	e.admin(t, http.MethodPut, "/bdx/gone.txt", "GONE").must(t, "gone")
	del := []byte(`<Delete><Object><Key>gone.txt</Key></Object></Delete>`)
	r := signedStreamingCall(t, e.ts.URL, http.MethodPost, "/bdx?delete", del, func(b []byte) []byte {
		return bytes.Replace(b, []byte("gone.txt"), []byte("keep.txt"), 1)
	})
	if r.code != http.StatusForbidden {
		t.Fatalf("tampered signed delete: %d %s, want 403", r.code, r.body)
	}
	if got := e.admin(t, http.MethodGet, "/bdx/keep.txt", ""); got.code != 200 {
		t.Fatal("keep.txt was deleted by a tampered body")
	}

	e.admin(t, http.MethodPut, "/bdx?versioning", versioningOn).must(t, "versioning")
	lock := `<ObjectLockConfiguration><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`
	r = signedStreamingCall(t, e.ts.URL, http.MethodPut, "/bdx?object-lock", []byte(lock), func(b []byte) []byte {
		b = bytes.Replace(b, []byte("GOVERNANCE"), []byte("COMPLIANCE"), 1)
		return bytes.Replace(b, []byte("<Days>1</Days>"), []byte("<Days>9</Days>"), 1)
	})
	if r.code != http.StatusForbidden {
		t.Fatalf("tampered signed object-lock: %d %s, want 403", r.code, r.body)
	}
	if info, err := e.store.GetBucket("bdx"); err == nil && info.DefaultRetentionMode == "COMPLIANCE" {
		t.Fatal("a tampered object lock default was applied")
	}

	signedStreamingCall(t, e.ts.URL, http.MethodPost, "/bdx?delete", del, func(b []byte) []byte { return b }).must(t, "an honest signed delete")
	if got := e.admin(t, http.MethodGet, "/bdx/gone.txt", ""); got.code != http.StatusNotFound {
		t.Fatal("an honest signed delete did not delete")
	}
}

// Regression (fourth review): a signed Snowball archive was staged in full,
// whatever the bucket quota. One whose files cannot fit is refused as soon as
// its headers show it, while it is still arriving.
func TestSnowballStagingStopsAtTheQuota(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/qqq", "").must(t, "bucket")
	if err := e.store.UpdateBucketQuota("qqq", 1<<20, 0); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/qqq/x.tar", nil)
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	signV4Request(req, testAccessKey, testSecretKey, []byte("whatever, the hash never matches"))
	req.Body = pr
	req.ContentLength = -1
	done := make(chan result, 1)
	go func() { done <- send(t, req) }()
	// An entry declaring 2 MiB in a bucket with 1 MiB left, then data that
	// never ends: only an early refusal finishes the request.
	var hdr bytes.Buffer
	tw := tar.NewWriter(&hdr)
	if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o644, Size: 2 << 20, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Flush()
	go func() {
		if _, err := pw.Write(hdr.Bytes()); err != nil {
			return
		}
		chunk := bytes.Repeat([]byte{0}, 64<<10)
		for {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		_ = pw.Close()
		t.Fatal("an archive whose files exceed the quota was staged on, not refused")
	}
	_ = pw.Close()
	if r.code != http.StatusForbidden || !strings.Contains(r.body, "QuotaExceeded") {
		t.Fatalf("%d %s, want 403 QuotaExceeded", r.code, r.body)
	}
	left, _ := os.ReadDir(filepath.Join(e.dataDir, snowballStagingDir))
	if len(left) > 0 {
		t.Fatalf("%d staged archives left", len(left))
	}
}

// signUnsignedPayload signs r with UNSIGNED-PAYLOAD, as an SDK does over
// HTTPS: the body is not covered.
func signUnsignedPayload(r *http.Request) {
	now := time.Now().UTC()
	dateStr, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("Host", r.Host)
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	signed := signedHeaderList(r)
	sts := buildStringToSignAt(amzDate, dateStr, testRegion, "s3", buildCanonicalRequest(r, signed, ""))
	sig := hex.EncodeToString(hmacSHA256(deriveSigningKey(testSecretKey, dateStr, testRegion, "s3"), []byte(sts)))
	r.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		testAccessKey, dateStr, testRegion, signed, sig))
}

// paxSparseArchive hand-builds a PAX 1.0 sparse entry (tar.Writer refuses
// GNU.sparse records): a few KiB declaring a file of realsize bytes, all
// holes but its last byte.
func paxSparseArchive(name string, realsize int64) []byte {
	ustar := func(name string, typ byte, size int64) []byte {
		b := make([]byte, 512)
		copy(b[0:], name)
		copy(b[100:], "0000644\x00")
		copy(b[108:], "0000000\x00")
		copy(b[116:], "0000000\x00")
		copy(b[124:], fmt.Sprintf("%011o\x00", size))
		copy(b[136:], "00000000000\x00")
		b[156] = typ
		copy(b[257:], "ustar\x0000")
		copy(b[148:], "        ")
		var sum int
		for _, c := range b {
			sum += int(c)
		}
		copy(b[148:], fmt.Sprintf("%06o\x00 ", sum))
		return b
	}
	pad := func(b []byte) []byte {
		if r := len(b) % 512; r != 0 {
			b = append(b, make([]byte, 512-r)...)
		}
		return b
	}
	rec := func(k, v string) string { // "<len> k=v\n", len counting itself
		body := " " + k + "=" + v + "\n"
		n := len(body) + 1
		for len(fmt.Sprint(n))+len(body) != n {
			n = len(fmt.Sprint(n)) + len(body)
		}
		return fmt.Sprint(n) + body
	}
	pax := rec("GNU.sparse.major", "1") + rec("GNU.sparse.minor", "0") +
		rec("GNU.sparse.name", name) + rec("GNU.sparse.realsize", fmt.Sprint(realsize))
	var out []byte
	out = append(out, ustar("PaxHeaders/"+name, 'x', int64(len(pax)))...)
	out = append(out, pad([]byte(pax))...)
	data := append(pad([]byte(fmt.Sprintf("1\n%d\n1\n", realsize-1))), 'X')
	out = append(out, ustar("GNUSparseFile.0/"+name, '0', int64(len(data)))...)
	out = append(out, pad(data)...)
	return append(out, make([]byte, 1024)...)
}

// Regression (ninth review, from before 5.0.9): a PAX sparse entry is read as
// a regular file of its full size with the holes filled by zeros, so a few
// KiB of archive stored an object of any size, past the 5 GiB staging cap
// and the per-object limit, whether the archive was signed or not. Sparse
// entries are refused, and an entry is held to the 5 GiB of one upload.
func TestSnowballSparseAndOversizedEntriesAreRefused(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/spbomb", "").must(t, "bucket")
	arch := paxSparseArchive("bomb.bin", 64<<20)
	// The archive really declares a 64 MiB file, so a refusal below is the
	// guard's, not a malformed archive's.
	tr := tar.NewReader(bytes.NewReader(arch))
	hdr, err := tr.Next()
	if err != nil || hdr.Typeflag != tar.TypeReg || hdr.Size != 64<<20 || hdr.Name != "bomb.bin" {
		t.Fatalf("test archive: %v %+v", err, hdr)
	}
	for _, signed := range []bool{true, false} {
		req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/spbomb/x.tar", nil)
		req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
		if signed {
			signV4Request(req, testAccessKey, testSecretKey, arch)
		} else {
			signUnsignedPayload(req)
		}
		req.Body = io.NopCloser(bytes.NewReader(arch))
		req.ContentLength = int64(len(arch))
		r := send(t, req)
		if r.code != http.StatusOK {
			t.Fatalf("signed=%v: %d %s, want 200 with the entry refused", signed, r.code, r.body)
		}
		if got := e.admin(t, http.MethodGet, "/spbomb/bomb.bin", ""); got.code != http.StatusNotFound {
			t.Fatalf("signed=%v: a %d-byte archive stored bomb.bin of %d bytes", signed, len(arch), len(got.body))
		}
	}

	// A plain entry over the per-object limit (shrunk here) is refused too,
	// and the rest of the archive is extracted.
	old := maxSnowballEntry
	maxSnowballEntry = 1000
	t.Cleanup(func() { maxSnowballEntry = old })
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		size int
	}{{"big.bin", 1001}, {"ok.txt", 5}} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(f.size), Typeflag: tar.TypeReg})
		_, _ = tw.Write(bytes.Repeat([]byte("o"), f.size))
	}
	_ = tw.Close()
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/spbomb/y.tar", nil)
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	signV4Request(req, testAccessKey, testSecretKey, buf.Bytes())
	req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	req.ContentLength = int64(buf.Len())
	send(t, req).must(t, "archive with an oversized entry")
	if got := e.admin(t, http.MethodGet, "/spbomb/big.bin", ""); got.code != http.StatusNotFound {
		t.Fatalf("an entry over the object size limit was stored (%d bytes)", len(got.body))
	}
	if got := e.admin(t, http.MethodGet, "/spbomb/ok.txt", ""); got.body != "ooooo" {
		t.Fatalf("ok.txt = %d %q", got.code, got.body)
	}
}

// Regression (eighth review): staging charged the quota for every file in the
// archive, also the ones extraction refuses (a "../" name, a key the caller
// may not write), so a signed archive was refused for quota where the same
// archive unsigned was extracted. Only stored entries count.
func TestSnowballQuotaIgnoresEntriesExtractionRefuses(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/qref", "").must(t, "bucket")
	if err := e.store.UpdateBucketQuota("qref", 1<<20, 0); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		size int
	}{{"../escape.bin", 2 << 20}, {"ok.txt", 5}} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(f.size), Typeflag: tar.TypeReg})
		_, _ = tw.Write(bytes.Repeat([]byte("o"), f.size))
	}
	_ = tw.Close()
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/qref/x.tar", nil)
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	signV4Request(req, testAccessKey, testSecretKey, buf.Bytes())
	req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	req.ContentLength = int64(buf.Len())
	r := send(t, req)
	if r.code != http.StatusOK {
		t.Fatalf("signed archive with a refused 2 MiB entry in a 1 MiB quota: %d %s, want 200", r.code, r.body)
	}
	if got := e.admin(t, http.MethodGet, "/qref/ok.txt", ""); got.body != "ooooo" {
		t.Fatalf("ok.txt = %d %q", got.code, got.body)
	}
}

// Regression (seventh review): staging held the archive's own size to the
// remaining quota, and tar adds a header and padding to every entry (GNU tar
// and Python pad the file to 10 KiB), so an archive whose files fit was
// refused with QuotaExceeded, which 5.0.8 extracted. The files are what count.
func TestSnowballArchiveWhoseFilesFitTheQuotaIsExtracted(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/qfit", "").must(t, "bucket")
	if err := e.store.UpdateBucketQuota("qfit", 8192, 0); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: 1000, Typeflag: tar.TypeReg})
	_, _ = tw.Write(bytes.Repeat([]byte("a"), 1000))
	_ = tw.Close()
	buf.Write(make([]byte, 10240-buf.Len())) // padded to a 10 KiB record, as tar does
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/qfit/x.tar", nil)
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	signV4Request(req, testAccessKey, testSecretKey, buf.Bytes())
	req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	req.ContentLength = int64(buf.Len())
	r := send(t, req)
	if r.code != http.StatusOK {
		t.Fatalf("archive of 1000 bytes of files, 10240 framed, quota 8192: %d %s, want 200", r.code, r.body)
	}
	if got := e.admin(t, http.MethodGet, "/qfit/a.txt", ""); got.body != strings.Repeat("a", 1000) {
		t.Fatalf("a.txt = %d bytes", len(got.body))
	}
}

// Regression (fourth review): a small configuration body was read up to 8 MiB
// before its handler, whose own limit is 64 KiB. It is read up to 256 KiB.
func TestSmallConfigurationBodiesAreNotOverRead(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/buf", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/buf/k", "x").must(t, "object")
	cr := &countingReader{max: maxVerifiedBody}
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/buf/k?tagging", nil)
	signV4Request(req, testAccessKey, testSecretKey, []byte("<Tagging/>"))
	req.Body = io.NopCloser(cr)
	req.ContentLength = maxVerifiedBody
	send(t, req)
	// Counted on the client side, so TCP buffers and the up to 256 KiB that
	// net/http drains after a handler answers add to the 256 KiB the check
	// reads. Before the fix the whole 8 MiB was read.
	if n := cr.n.Load(); n > 2<<20 {
		t.Fatalf("a ?tagging body was read to %d bytes", n)
	}
}

// A browser form upload sent with a signed hash is checked before anything is
// stored: the form parser stops at the closing boundary, so the handler reads
// on to the end, where the check runs.
func TestFormUploadBodyIsCheckedBeforeStoring(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/formb", "").must(t, "create bucket")
	// One boundary for both: an attacker who rewrites the body keeps the
	// boundary the signed Content-Type names. Two random boundaries made the
	// parser read to EOF by accident, so an earlier version of this test
	// passed without the guard.
	form := func(key, content string, epilogue int) (string, []byte) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		_ = mw.SetBoundary("vaults3testboundary")
		_ = mw.WriteField("key", key)
		fw, _ := mw.CreateFormFile("file", "f.txt")
		_, _ = fw.Write([]byte(content))
		_ = mw.Close()
		// Bytes after the closing boundary (multipart allows them): the
		// parser stops before them, so only reading on reaches the check,
		// and an epilogue past the 1 MiB an unchecked body is drained to
		// must not skip it.
		b.WriteString(strings.Repeat("\r\n", epilogue))
		return mw.FormDataContentType(), b.Bytes()
	}
	for _, epilogue := range []int{0, 32 << 10, 1 << 20} {
		// Without an epilogue the body ends at the boundary: the parser's
		// own read gets the hash error with the last bytes and swallows it,
		// so the check must keep reporting it to the read that follows.
		t.Run(fmt.Sprintf("epilogue=%d", 2*epilogue), func(t *testing.T) {
			ct, signedBody := form("honest.txt", "honest", epilogue)
			_, sentBody := form("victim.txt", "EVIL!!", epilogue)
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/formb", nil)
			req.Header.Set("Content-Type", ct)
			req.ContentLength = int64(len(sentBody))
			signV4Request(req, testAccessKey, testSecretKey, signedBody)
			req.Body = io.NopCloser(bytes.NewReader(sentBody))
			if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "XAmzContentSHA256Mismatch") {
				t.Fatalf("tampered form upload: %d %s, want 400 XAmzContentSHA256Mismatch", r.code, r.body)
			}
			if got := call(t, ts, http.MethodGet, "/formb/victim.txt", ""); got.code != http.StatusNotFound {
				t.Fatalf("tampered form upload stored %q", got.body)
			}
		})
	}

	// A form upload streams: an honest one larger than any configuration
	// body is not cut off at the 256 KiB those are held to.
	big := strings.Repeat("F", 300<<10)
	ct, body := form("up.txt", big, 0)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/formb", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.ContentLength = int64(len(body))
	signV4Request(req, testAccessKey, testSecretKey, body)
	if r := send(t, req); r.code >= 300 {
		t.Fatalf("honest 300 KiB form upload: %d %s", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/formb/up.txt", ""); got.body != big {
		t.Fatalf("honest form upload stored %d bytes, want %d", len(got.body), len(big))
	}
}

// Archives staged by a run stopped mid-upload are removed at startup.
func TestStagedSnowballArchivesAreClearedAtStartup(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, snowballStagingDir)
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "archive-123"), []byte("left behind"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClearSnowballStaging(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging dir still there: %v", err)
	}
}

// Headers VaultS3 never acts on, and the payload hash that the signature
// covers on its own line, may be unsigned: CloudFront adds X-Amz-Cf-Id after
// the client signed, the .NET SDK's browser mode sends X-Amz-User-Agent
// unsigned, and the 5.0.8 FUSE mount sends X-Amz-Content-Sha256 unlisted. A
// refusal names the header, as AWS does, so the cause is visible.
func TestHarmlessUnsignedHeadersAreAllowedAndRefusalsNameTheHeader(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/harml", "").must(t, "create bucket")
	for _, h := range []string{"X-Amz-Cf-Id", "X-Amz-User-Agent"} {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/harml/k", nil)
		req.ContentLength = 2
		signV4Request(req, testAccessKey, testSecretKey, []byte("ok"))
		req.Header.Set(h, "added-by-a-proxy") // after signing
		send(t, req).must(t, "unsigned "+h)
	}
	// X-Amz-Content-Sha256 unlisted (old FUSE): signed only through the
	// payload hash line, so changing it breaks the signature.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/harml/k", nil)
	signWithoutContentHash(req)
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadSHA256) // what auth assumed
	send(t, req).must(t, "unlisted payload hash that matches")
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/harml/k", nil)
	signWithoutContentHash(req)
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD") // changed on the way
	if r := send(t, req); r.code != http.StatusForbidden {
		t.Fatalf("a changed unlisted payload hash: %d, want 403", r.code)
	}

	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/harml/k2", nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	req.Header.Set("X-Amz-Acl", "public-read")
	r := send(t, req)
	if r.code != http.StatusForbidden || !strings.Contains(r.body, "not signed: x-amz-acl") {
		t.Fatalf("%d %s, want 403 naming x-amz-acl", r.code, r.body)
	}
}

// Audit: a body longer than declared on the normal filesystem engine, which
// reads to EOF, is refused by the length check in the reader itself.
func TestBodyLongerThanDeclaredIsRefusedOnTheFilesystemEngine(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/longfs", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("abcdefgh"), 100)
	framed := fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(body), body)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/longfs/k", strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", "100")
	signStreamingTrailer(req)
	if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "IncompleteBody") {
		t.Fatalf("%d %s, want 400 IncompleteBody", r.code, r.body)
	}
	if code, _ := headChecksum(t, ts, "/longfs/k", "crc32"); code != http.StatusNotFound {
		t.Fatalf("refused upload is readable: HEAD %d", code)
	}
}

// Audit: a body declared empty but carrying data, on an engine that never
// reads an empty body, used to be refused only after the engine had saved an
// empty object over the existing one. It is checked before the engine runs.
func TestBodyDeclaredEmptyButCarryingDataChangesNothing(t *testing.T) {
	ts, _ := exactSizeServer(t)
	call(t, ts, http.MethodPut, "/emptyd", "").must(t, "create bucket")
	call(t, ts, http.MethodPut, "/emptyd/k", "ORIGINAL").must(t, "original")
	framed := "8\r\nINJECTED\r\n0\r\n\r\n"
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/emptyd/k", strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", "0")
	signStreamingTrailer(req)
	if r := send(t, req); r.code != http.StatusBadRequest {
		t.Fatalf("%d %s, want 400", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/emptyd/k", ""); got.body != "ORIGINAL" {
		t.Fatalf("the existing object was changed: %d %q", got.code, got.body)
	}
}

// Audit: the trailer cap is on the whole trailer, not only per line, so many
// short lines cannot grow memory without bound.
func TestManyShortTrailerLinesAreRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/shortl", "").must(t, "create bucket")
	body := []byte("x")
	trailer := strings.Repeat("x-amz-padding:a\r\n", 2*maxTrailerBytes/17) + "x-amz-checksum-crc32:" + checksumOf("crc32", body) + "\r\n"
	r := trailerPut(t, ts, "/shortl/k", body, "x-amz-checksum-crc32", trailer)
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "MalformedTrailerError") {
		t.Fatalf("%d %s, want 400 MalformedTrailerError", r.code, r.body)
	}
}

// Audit: a chunk or trailer sent without its signature is refused by its own
// check (which says so), not only by a signature comparison that happens to
// fail.
func TestMissingChunkAndTrailerSignaturesHaveTheirOwnRefusal(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/missg", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("m"), 9000)
	if r := signedStreamPut(t, ts, "/missg/a", body, streamOpts{chunk: 4096, dropSigOnChunk: 2}); !strings.Contains(r.body, "without its chunk-signature") {
		t.Fatalf("missing chunk signature: %d %s", r.code, r.body)
	}
	name, value := "x-amz-checksum-crc32", checksumOf("crc32", body)
	if r := signedStreamPut(t, ts, "/missg/b", body, streamOpts{chunk: 4096, trailerName: name, trailerValue: value, trailerSig: "-"}); !strings.Contains(r.body, "without its x-amz-trailer-signature") {
		t.Fatalf("missing trailer signature: %d %s", r.code, r.body)
	}
}

// Audit: CopyObject keeps CRC64NVME and GetObjectAttributes returns it.
func TestCRC64NVMESurvivesCopyAndIsInObjectAttributes(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/nvmec", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("c"), 500)
	want := checksumOf("crc64nvme", body)
	call(t, ts, http.MethodPut, "/nvmec/src", string(body), "X-Amz-Checksum-Crc64nvme", want).must(t, "source")
	call(t, ts, http.MethodPut, "/nvmec/dst", "", "X-Amz-Copy-Source", "/nvmec/src").must(t, "copy")
	if code, got := headChecksum(t, ts, "/nvmec/dst", "crc64nvme"); code != 200 || got != want {
		t.Fatalf("copy: HEAD %d %q, want %q", code, got, want)
	}
	attrs := call(t, ts, http.MethodGet, "/nvmec/src?attributes", "", "X-Amz-Object-Attributes", "Checksum").must(t, "attributes")
	if !strings.Contains(attrs.body, "<ChecksumCRC64NVME>"+want+"</ChecksumCRC64NVME>") {
		t.Fatalf("GetObjectAttributes: %s", attrs.body)
	}
}

// A 5.0.8 FUSE mount signs exactly host and x-amz-date, and sends
// X-Amz-Content-Sha256: UNSIGNED-PAYLOAD without listing it. The signature
// still covers that value (it is the payload hash line), so such a mount keeps
// working against this server, for reads and writes.
func TestA508FUSEStyleRequestStillWorks(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/oldfuse", "").must(t, "create bucket")
	old := func(method, path, body string) result {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		now := time.Now().UTC()
		dateStr, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
		req.Header.Set("X-Amz-Date", amzDate)
		req.Header.Set("Host", req.Host)
		req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
		signed := "host;x-amz-date"
		sts := buildStringToSignAt(amzDate, dateStr, testRegion, "s3", buildCanonicalRequest(req, signed, ""))
		sig := hex.EncodeToString(hmacSHA256(deriveSigningKey(testSecretKey, dateStr, testRegion, "s3"), []byte(sts)))
		req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s",
			testAccessKey, dateStr, testRegion, signed, sig))
		return send(t, req)
	}
	old(http.MethodPut, "/oldfuse/f.txt", "written by an old mount").must(t, "old FUSE write")
	if got := old(http.MethodGet, "/oldfuse/f.txt", "").must(t, "old FUSE read"); got.body != "written by an old mount" {
		t.Fatalf("read %q", got.body)
	}
	old(http.MethodGet, "/oldfuse?list-type=2", "").must(t, "old FUSE list")
}

// rtFunc is an http.RoundTripper made from a function.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Regression (fifth review): a form upload sent as signed chunks, rewritten on
// the way (same boundary, same length, so the signed decoded length holds),
// was stored: the chunk signature error came with the last bytes and the form
// parser swallowed it. The handler reads on to the end, where the error stays.
func TestTamperedSignedChunkFormUploadIsRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/formc", "").must(t, "create bucket")
	mk := func(key, content string) (string, []byte) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		_ = mw.SetBoundary("vaults3testboundary")
		_ = mw.WriteField("key", key)
		fw, _ := mw.CreateFormFile("file", "f.txt")
		_, _ = fw.Write([]byte(content))
		_ = mw.Close()
		return mw.FormDataContentType(), b.Bytes()
	}
	ct, signedBody := mk("honest.txt", "honest")
	_, sentBody := mk("victim.txt", "EVIL!!")
	orig := http.DefaultTransport
	http.DefaultTransport = rtFunc(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("Content-Type", ct)
		return orig.RoundTrip(r)
	})
	defer func() { http.DefaultTransport = orig }()
	r := signedStreamingCall(t, ts.URL, http.MethodPost, "/formc", signedBody, func([]byte) []byte { return sentBody })
	if r.code < 400 {
		t.Fatalf("tampered signed-chunk form upload answered %d", r.code)
	}
	if got := call(t, ts, http.MethodGet, "/formc/victim.txt", ""); got.code != http.StatusNotFound {
		t.Fatalf("tampered form stored victim.txt = %q", got.body)
	}
}

// A second X-Amz-Trailer header line added on the way does not count: the
// signature covers the first value only, and the trailer checks read the same.
func TestASecondTrailerHeaderLineIsIgnored(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/dupt", "").must(t, "create bucket")
	body := []byte("duplicated trailer header")
	name := "x-amz-checksum-crc32"
	framed := fmt.Sprintf("%x\r\n%s\r\n0\r\n%s:%s\r\n\r\n", len(body), body, name, checksumOf("crc32", body))
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/dupt/k", strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(body)))
	req.Header.Set("X-Amz-Trailer", name)
	signStreamingTrailer(req)
	req.Header.Add("X-Amz-Trailer", "x-amz-checksum-sha256") // added after signing
	send(t, req).must(t, "upload with an added second trailer line")
}

// Audit (fifth review): a body shorter than its declared length was stored
// truncated (an unsigned aws-chunked body that lost its tail on the way). It
// is refused with IncompleteBody, as on AWS, and nothing is stored.
func TestBodyShorterThanDeclaredIsRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/short", "").must(t, "create bucket")
	call(t, ts, http.MethodPut, "/short/k", "ORIGINAL").must(t, "original")
	body := bytes.Repeat([]byte("s"), 800)
	framed := fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(body), body)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/short/k", strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", "5000")
	signStreamingTrailer(req)
	if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "IncompleteBody") {
		t.Fatalf("%d %s, want 400 IncompleteBody", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/short/k", ""); got.body != "ORIGINAL" {
		t.Fatalf("object changed to %d bytes", len(got.body))
	}
}

// Audit (fifth review): a signed streaming upload cut short (the last chunks
// dropped and a forged final chunk sent) is refused by the final chunk's
// signature, the only check that sees the cut when the declared length was
// also signed.
func TestSignedStreamingUploadCutShortIsRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/cut", "").must(t, "create bucket")
	body := bytes.Repeat([]byte("c"), 3*8192)
	// Sign 3 chunks, send only the first 2, then a final chunk with a
	// signature that does not chain.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/cut/k", nil)
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(2*8192))
	now := time.Now().UTC()
	dateStr, amzDate := now.Format("20060102"), now.Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Host", req.Host)
	req.Header.Set("X-Amz-Content-Sha256", signedStreaming)
	signed := signedHeaderList(req)
	key := deriveSigningKey(testSecretKey, dateStr, testRegion, "s3")
	seed := hex.EncodeToString(hmacSHA256(key, []byte(buildStringToSignAt(amzDate, dateStr, testRegion, "s3", buildCanonicalRequest(req, signed, "")))))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		testAccessKey, dateStr, testRegion, signed, seed))
	prev := seed
	sign := func(d []byte) string {
		s := sha256.Sum256(d)
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + amzDate + "\n" + dateStr + "/" + testRegion + "/s3/aws4_request\n" + prev + "\n" + emptyPayloadSHA256 + "\n" + hex.EncodeToString(s[:])
		prev = hex.EncodeToString(hmacSHA256(key, []byte(sts)))
		return prev
	}
	var framed strings.Builder
	for i := 0; i < 2; i++ {
		d := body[i*8192 : (i+1)*8192]
		fmt.Fprintf(&framed, "%x;chunk-signature=%s\r\n%s\r\n", len(d), sign(d), d)
	}
	framed.WriteString("0;chunk-signature=" + strings.Repeat("0", 64) + "\r\n\r\n")
	req.Body = io.NopCloser(strings.NewReader(framed.String()))
	req.ContentLength = int64(framed.Len())
	if r := send(t, req); r.code != http.StatusForbidden || !strings.Contains(r.body, "SignatureDoesNotMatch") {
		t.Fatalf("%d %s, want 403 SignatureDoesNotMatch", r.code, r.body)
	}
	if code, _ := headChecksum(t, ts, "/cut/k", "crc32"); code != http.StatusNotFound {
		t.Fatalf("a truncated upload was stored: HEAD %d", code)
	}
}

// Audit (fifth review): chunkSignerFor's refusal is reachable through a
// presigned URL (its canonical request ends in the literal UNSIGNED-PAYLOAD,
// and X-Amz-Content-Sha256 may be unsigned), so it is tested directly: a
// presigned PUT claiming signed chunks, with no decoded-length header.
func TestPresignedRequestClaimingSignedChunksIsRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/pchunk", "").must(t, "create bucket")
	u := GeneratePresignedPutURL(ts.URL, "pchunk", "k", testAccessKey, testSecretKey, testRegion, time.Minute, nil)
	req, _ := http.NewRequest(http.MethodPut, u, strings.NewReader("5;chunk-signature=00\r\nhello\r\n0;chunk-signature=00\r\n\r\n"))
	req.Header.Set("X-Amz-Content-Sha256", signedStreaming)
	if r := send(t, req); r.code != http.StatusForbidden || !strings.Contains(r.body, "SignatureDoesNotMatch") {
		t.Fatalf("%d %s, want 403 SignatureDoesNotMatch", r.code, r.body)
	}
	if got := call(t, ts, http.MethodGet, "/pchunk/k", ""); got.code != http.StatusNotFound {
		t.Fatalf("unverified chunks were stored: %q", got.body)
	}
}

// Audit (fifth review): a chunk-signed configuration body over its limit is
// refused, not cut at the limit and handed to the handler with its last
// chunk unverified.
func TestOversizedChunkSignedConfigurationBodyIsRefused(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/bigcfg", "").must(t, "bucket")
	tagging := []byte("<Tagging><TagSet></TagSet></Tagging>" + strings.Repeat(" ", 300<<10))
	r := signedStreamingCall(t, e.ts.URL, http.MethodPut, "/bigcfg?tagging", tagging, func(b []byte) []byte { return b })
	if r.code != http.StatusBadRequest || !strings.Contains(r.body, "MaxMessageLengthExceeded") {
		t.Fatalf("%d %s, want 400 MaxMessageLengthExceeded", r.code, r.body)
	}
}

// Audit (fifth review): each chunk's data must end in CRLF.
func TestChunkWithoutItsLineEndingIsRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/crlf", "").must(t, "create bucket")
	framed := "5\r\nhelloXX0\r\n\r\n"
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/crlf/k", strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", "5")
	signStreamingTrailer(req)
	if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "IncompleteBody") {
		t.Fatalf("%d %s, want 400 IncompleteBody", r.code, r.body)
	}
}

// Audit (fifth review): UploadPart refuses a part declared empty but carrying
// data before writing anything, like PutObject.
func TestUploadPartDeclaredEmptyButCarryingDataIsRefused(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/partempty", "").must(t, "create bucket")
	init := call(t, ts, http.MethodPost, "/partempty/big?uploads", "").must(t, "create upload")
	start := strings.Index(init.body, "<UploadId>") + len("<UploadId>")
	uploadID := init.body[start:strings.Index(init.body, "</UploadId>")]
	framed := "8\r\nINJECTED\r\n0\r\n\r\n"
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/partempty/big?partNumber=1&uploadId="+uploadID, strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", "0")
	signStreamingTrailer(req)
	if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "IncompleteBody") {
		t.Fatalf("%d %s, want 400 IncompleteBody", r.code, r.body)
	}
}

// Audit (fifth review): Snowball staging stops at the 5 GiB single-upload cap
// (shrunk here) when no quota is lower.
func TestSnowballStagingStopsAtTheSizeCap(t *testing.T) {
	old := maxSnowballArchive
	maxSnowballArchive = 1 << 20
	t.Cleanup(func() { maxSnowballArchive = old })
	ts, data := exactSizeServer(t)
	call(t, ts, http.MethodPut, "/scap", "").must(t, "create bucket")
	big := bytes.Repeat([]byte("z"), 2<<20)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/scap/x.tar", nil)
	req.Header.Set("X-Amz-Meta-Snowball-Auto-Extract", "true")
	req.ContentLength = int64(len(big))
	signV4Request(req, testAccessKey, testSecretKey, big)
	req.Body = io.NopCloser(bytes.NewReader(big))
	if r := send(t, req); r.code != http.StatusBadRequest || !strings.Contains(r.body, "EntityTooLarge") {
		t.Fatalf("%d %s, want 400 EntityTooLarge", r.code, r.body)
	}
	if left, _ := os.ReadDir(filepath.Join(data, snowballStagingDir)); len(left) > 0 {
		t.Fatalf("%d staged archives left", len(left))
	}
}

// Audit (fifth review): the presigned URL's size limit still holds for an
// aws-chunked body when no decoded-length header is sent.
func TestPresignedSizeLimitHoldsForAwsChunkedWithoutDecodedLength(t *testing.T) {
	ts, _ := fullServer(t)
	call(t, ts, http.MethodPut, "/psz", "").must(t, "create bucket")
	u := GeneratePresignedPutURL(ts.URL, "psz", "k", testAccessKey, testSecretKey, testRegion, time.Minute,
		&PresignedUploadRestrictions{MaxSize: 10})
	framed := awsChunkedEncode(bytes.Repeat([]byte("x"), 1000), false)
	req, _ := http.NewRequest(http.MethodPut, u, strings.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	if r := send(t, req); r.code < 400 {
		t.Fatalf("1000-byte aws-chunked body on a 10-byte URL answered %d", r.code)
	}
	if got := call(t, ts, http.MethodGet, "/psz/k", ""); got.code != http.StatusNotFound {
		t.Fatalf("stored %d bytes", len(got.body))
	}
}
