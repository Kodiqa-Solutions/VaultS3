package s3

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// SSE-C used to seal the whole object as one AES-GCM message at the handler, so
// every GET (a 1 KiB Range request included) read the entire ciphertext and
// allocated the entire plaintext, and N concurrent readers held N copies: the
// OOM shape of issue #49, which 4.4.53 fixed for server-side encryption by
// moving to the chunked VS3S format. These tests pin SSE-C to that same format.

func ssecHeaderMap(key []byte) map[string]string {
	h := ssecHeaders(key)
	return map[string]string{
		hdrSSECAlgo:   h.Get(hdrSSECAlgo),
		hdrSSECKey:    h.Get(hdrSSECKey),
		hdrSSECKeyMD5: h.Get(hdrSSECKeyMD5),
	}
}

// A new SSE-C object is stored as a VS3S stream and a Range GET is served from
// the streaming reader, not from a decrypted copy of the object.
func TestSSEC_RangeGetStreamsWithoutBuffering(t *testing.T) {
	_, store, engine, ts := newObjTestServer(t)
	bucket, key := "vault", "big.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	custKey := make([]byte, 32)
	rand.Read(custKey)
	plain := make([]byte, 5000)
	rand.Read(plain)

	resp := doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, plain, ssecHeaderMap(custKey))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	// On disk: the streaming header and no plaintext; metadata keeps the
	// plaintext length, which is what Content-Length reports.
	raw, err := os.ReadFile(engine.ObjectPath(bucket, key))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("VS3S")) {
		t.Fatalf("SSE-C object should carry the VS3S streaming header on disk, got %q", raw[:min(4, len(raw))])
	}
	if bytes.Contains(raw, plain[:64]) {
		t.Fatal("plaintext leaked to disk")
	}
	if meta, err := store.GetObjectMeta(bucket, key); err != nil || meta == nil || meta.Size != int64(len(plain)) {
		t.Fatalf("meta.Size should be the plaintext length %d (meta=%v err=%v)", len(plain), meta, err)
	}

	// Range offsets are plaintext offsets, and Content-Range names the plaintext
	// total, even though the stored blob is longer.
	hdrs := ssecHeaderMap(custKey)
	hdrs["Range"] = "bytes=1000-1999"
	resp = doSignedWithHeaders(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil, hdrs)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range GET: %d %s", resp.StatusCode, readBody(t, resp))
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 1000-1999/5000" {
		t.Fatalf("Content-Range = %q", cr)
	}
	if got := readBody(t, resp); got != string(plain[1000:2000]) {
		t.Fatal("range body does not match the plaintext slice")
	}

	// The reader the handler serves from must be the streaming one, not the
	// in-memory buffer the whole-object path builds. Open the stored blob the way
	// GetObject does and look at what comes back.
	k, err := parseSSECHeaders(&http.Request{Header: ssecHeaders(custKey)})
	if err != nil {
		t.Fatal(err)
	}
	src, stored, err := engine.GetObject(bucket, key)
	if err != nil {
		t.Fatal(err)
	}
	r, size, err := ssecOpenStored(k, src, stored)
	if err != nil {
		t.Fatalf("ssecOpenStored: %v", err)
	}
	defer r.Close()
	if _, buffered := r.(ssecReader); buffered {
		t.Fatal("a VS3S SSE-C object was opened into an in-memory ssecReader; it must stream")
	}
	if size != int64(len(plain)) {
		t.Fatalf("plaintext size %d, want %d", size, len(plain))
	}
	if stored <= size {
		t.Fatalf("stored size %d should exceed plaintext size %d (header + tags)", stored, size)
	}
	// Seek then Read is all the handler's Range and partNumber paths use.
	if _, err := r.Seek(4321, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tail, plain[4321:]) {
		t.Fatal("seek+read returned the wrong bytes")
	}
}

// An object written by the pre-streaming SSE-C code (one GCM seal over the whole
// object) has no VS3S header and must keep decrypting after the upgrade; nothing
// on disk is migrated.
func TestSSEC_LegacyWholeObjectStillReads(t *testing.T) {
	_, store, engine, ts := newObjTestServer(t)
	bucket, key := "vault", "old.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	custKey := make([]byte, 32)
	rand.Read(custKey)
	k, err := parseSSECHeaders(&http.Request{Header: ssecHeaders(custKey)})
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("sealed as one message before SSE-C learned to stream")

	// Exactly what the old PUT path did: seal in memory, store the blob, record
	// the plaintext size and the key's MD5.
	sealed, err := ssecSeal(k, plain)
	if err != nil {
		t.Fatal(err)
	}
	_, etag, err := engine.PutObject(bucket, key, bytes.NewReader(sealed), int64(len(sealed)))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutObjectMeta(metadata.ObjectMeta{
		Bucket: bucket, Key: key, ContentType: "application/octet-stream",
		ETag: etag, Size: int64(len(plain)), LastModified: time.Now().Unix(),
		SSECustomerKeyMD5: k.keyMD5,
	}); err != nil {
		t.Fatal(err)
	}

	resp := doSignedWithHeaders(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil, ssecHeaderMap(custKey))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET of legacy SSE-C object: %d %s", resp.StatusCode, readBody(t, resp))
	}
	if got := readBody(t, resp); got != string(plain) {
		t.Fatalf("legacy round-trip mismatch: %q", got)
	}

	// Ranges on legacy objects still work too, through the buffered reader.
	hdrs := ssecHeaderMap(custKey)
	hdrs["Range"] = "bytes=7-16"
	resp = doSignedWithHeaders(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil, hdrs)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("legacy Range GET: %d", resp.StatusCode)
	}
	if got := readBody(t, resp); got != string(plain[7:17]) {
		t.Fatalf("legacy range mismatch: %q", got)
	}
}

// On a cluster node the stale-copy check compares the opened size with
// meta.Size, which is the plaintext length. It used to run against the SSE-C
// ciphertext, so every SSE-C GET looked like a copy that had not caught up and
// was routed to a peer or refused with SlowDown.
func TestSSEC_ClusterStaleCheckSeesPlaintextSize(t *testing.T) {
	h, store, _, ts := newObjTestServer(t)
	fallbacks := 0
	h.objects.dataHolderFallback = func(http.ResponseWriter, *http.Request, string, string) (bool, bool) {
		fallbacks++
		return false, false
	}
	bucket, key := "vault", "clustered.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	custKey := make([]byte, 32)
	rand.Read(custKey)
	plain := make([]byte, 3000)
	rand.Read(plain)

	resp := doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, plain, ssecHeaderMap(custKey))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	resp = doSignedWithHeaders(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil, ssecHeaderMap(custKey))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET on a cluster node: %d %s", resp.StatusCode, readBody(t, resp))
	}
	if got := readBody(t, resp); got != string(plain) {
		t.Fatal("round-trip mismatch")
	}
	if fallbacks != 0 {
		t.Fatalf("a freshly written SSE-C object was treated as stale %d time(s)", fallbacks)
	}
}
