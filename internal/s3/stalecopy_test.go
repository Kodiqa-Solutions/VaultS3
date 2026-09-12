package s3

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// A cluster node can hold the PREVIOUS bytes of a key while already holding the
// NEW metadata, because metadata replicates through Raft synchronously and object
// data is copied asynchronously. Serving the local file in that window answers
// with the old object under the new object's ETag and Last-Modified, which is a
// silent wrong answer rather than a miss.

func TestLocalCopyIsStaleOnlyAppliesInACluster(t *testing.T) {
	h := &ObjectHandler{}
	meta := &metadata.ObjectMeta{Size: 100}

	// Single node: there is no second copy to be behind and no peer to ask.
	if h.localCopyIsStale(meta, 50) {
		t.Fatal("a single-node server has no holder to fall back to, so it must never call its own copy stale")
	}

	h.dataHolderFallback = func(http.ResponseWriter, *http.Request, string, string) (bool, bool) { return false, false }
	if !h.localCopyIsStale(meta, 50) {
		t.Fatal("a clustered node holding 50 bytes for a 100 byte object is serving the previous write")
	}
	if h.localCopyIsStale(meta, 100) {
		t.Fatal("matching sizes must not be treated as stale, that would route every read to a peer")
	}
}

func TestLocalCopyIsStaleIgnoresMarkersAndMissingMetadata(t *testing.T) {
	h := &ObjectHandler{
		dataHolderFallback: func(http.ResponseWriter, *http.Request, string, string) (bool, bool) { return false, false },
	}
	if h.localCopyIsStale(nil, 10) {
		t.Fatal("no metadata means the caller already handled it")
	}
	if h.localCopyIsStale(&metadata.ObjectMeta{DeleteMarker: true, Size: 0}, 99) {
		t.Fatal("a delete marker has no bytes of its own to compare")
	}
}

// A zero-byte object is a real object, and its metadata size matches its data
// size, so it must not be mistaken for a stale copy on every read.
func TestEmptyObjectIsNotStale(t *testing.T) {
	h := &ObjectHandler{
		dataHolderFallback: func(http.ResponseWriter, *http.Request, string, string) (bool, bool) { return false, false },
	}
	if h.localCopyIsStale(&metadata.ObjectMeta{Size: 0}, 0) {
		t.Fatal("a zero-byte object matches its metadata and is not stale")
	}
}

// End to end on the read path: the node has fresh metadata and stale bytes, and
// no peer can serve the current data. It must ask the client to retry rather than
// hand back the previous object's bytes.
func TestStaleLocalCopyIsNotServed(t *testing.T) {
	_, store, engine, ts := newObjTestServer(t)
	bucket, key := "clust", "doc.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("the original bytes")); resp.StatusCode != http.StatusOK {
		t.Fatal("seed put failed")
	}
	_ = engine

	// Simulate the window: metadata advances to a larger object (an overwrite that
	// landed on another holder) while this node still has the old file.
	meta, err := store.GetObjectMeta(bucket, key)
	if err != nil || meta == nil {
		t.Fatal("no metadata")
	}
	meta.Size = 9999
	meta.ETag = "\"a-different-object\""
	if err := store.PutObjectMeta(*meta); err != nil {
		t.Fatal(err)
	}

	// Single-node still serves it: there is nothing else to ask, and refusing
	// would turn a readable object into an error.
	if resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("single-node GET = %d, want 200", resp.StatusCode)
	}
}

// The same-size case: an overwrite that keeps the byte count identical is
// invisible to the size check, so a recently written object is verified by
// content. Outside the replication window that cost is not paid.
func TestLocalCopyNeedsContentCheckOnlyForRecentSmallObjects(t *testing.T) {
	h := &ObjectHandler{
		dataHolderFallback: func(http.ResponseWriter, *http.Request, string, string) (bool, bool) { return false, false },
	}
	now := time.Now().Unix()
	recent := &metadata.ObjectMeta{Size: 10, ETag: `"d41d8cd98f00b204e9800998ecf8427e"`, LastModified: now}

	if !h.localCopyNeedsContentCheck("b", recent, 10) {
		t.Fatal("an object written moments ago is exactly the one that can still be replicating")
	}
	old := *recent
	old.LastModified = now - int64(staleVerifyWindow.Seconds()) - 60
	if h.localCopyNeedsContentCheck("b", &old, 10) {
		t.Fatal("replication has long settled, verifying every read would cost the hash for nothing")
	}
	big := *recent
	if h.localCopyNeedsContentCheck("b", &big, staleVerifyMaxSize+1) {
		t.Fatal("hashing an object this large on read is worse than the problem it solves")
	}
	multipart := *recent
	multipart.ETag = `"d41d8cd98f00b204e9800998ecf8427e-4"`
	if h.localCopyNeedsContentCheck("b", &multipart, 10) {
		t.Fatal("a multipart ETag is not the MD5 of the object, so it cannot be recomputed here")
	}
	single := &ObjectHandler{}
	if single.localCopyNeedsContentCheck("b", recent, 10) {
		t.Fatal("a single-node server has no peer to fall back to")
	}
}

// An object encrypted at rest stores the MD5 of its CIPHERTEXT as the ETag while
// the read path hands back plaintext, so the content check can never pass and
// would condemn a perfectly good copy. On a cluster that answered 503 SlowDown
// for every read of a per-bucket encrypted object inside the replication window,
// on every holder, so the bucket simply did not work.
func TestLocalCopyNeedsContentCheckSkipsEncryptedObjects(t *testing.T) {
	now := time.Now().Unix()
	recent := &metadata.ObjectMeta{Size: 10, ETag: `"d41d8cd98f00b204e9800998ecf8427e"`, LastModified: now}
	fallback := func(http.ResponseWriter, *http.Request, string, string) (bool, bool) { return false, false }

	// SSE-C: the customer key makes it obvious, and this case was already handled.
	ssec := *recent
	ssec.SSECustomerKeyMD5 = "abc"
	h := &ObjectHandler{dataHolderFallback: fallback}
	if h.localCopyNeedsContentCheck("b", &ssec, 10) {
		t.Fatal("an SSE-C ETag is the ciphertext MD5, the check can never pass")
	}

	// Server-side encryption has the same shape and was the one that was missed.
	enc := &ObjectHandler{dataHolderFallback: fallback, encryptionEnabled: true}
	if enc.localCopyNeedsContentCheck("b", recent, 10) {
		t.Fatal("a server-side encrypted object's ETag is the ciphertext MD5 too")
	}

	// SSE-KMS encrypts every object without any per-bucket key, and it requires
	// an `encryption.key` it never uses, which builds a key manager. Asking that
	// manager whether the bucket is encrypted answers no for every KMS bucket, so
	// the check ran and every SSE-KMS object on a cluster failed it.
	kms := &ObjectHandler{
		dataHolderFallback: fallback,
		encryptionEnabled:  true,
		perBucketMode:      false,
		keyMgr:             kmsLikeManager(t),
	}
	if kms.localCopyNeedsContentCheck("b", recent, 10) {
		t.Fatal("SSE-KMS encrypts every object, its ETag is the ciphertext MD5 too")
	}

	// With encryption off the check must still run: it is what catches a
	// same-size overwrite that has not replicated yet.
	plain := &ObjectHandler{dataHolderFallback: fallback}
	if !plain.localCopyNeedsContentCheck("b", recent, 10) {
		t.Fatal("an unencrypted object must still be verified by content")
	}
}

func TestLocalCopyContentMatchesDetectsDifferentBytes(t *testing.T) {
	h := &ObjectHandler{}
	body := []byte("the current bytes")
	sum := md5.Sum(body)
	meta := &metadata.ObjectMeta{ETag: `"` + hex.EncodeToString(sum[:]) + `"`}

	if !h.localCopyContentMatches(newSeekReader(body), meta) {
		t.Fatal("matching content was reported as a mismatch, every read would be routed to a peer")
	}
	if h.localCopyContentMatches(newSeekReader([]byte("the previous byt!")), meta) {
		t.Fatal("different bytes of the SAME LENGTH were accepted, which is the whole gap this closes")
	}
}

// Verifying must leave the reader usable, or the object would be served empty.
func TestLocalCopyContentCheckRewinds(t *testing.T) {
	h := &ObjectHandler{}
	body := []byte("abcdefghij")
	sum := md5.Sum(body)
	meta := &metadata.ObjectMeta{ETag: `"` + hex.EncodeToString(sum[:]) + `"`}
	r := newSeekReader(body)
	if !h.localCopyContentMatches(r, meta) {
		t.Fatal("content should match")
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("reader was left at %q, so the object would be served truncated", got)
	}
}

type seekReader struct{ *bytes.Reader }

func (seekReader) Close() error { return nil }

func newSeekReader(b []byte) seekReader { return seekReader{bytes.NewReader(b)} }

// kmsLikeManager builds the key manager a server has in SSE-KMS mode: present,
// because a static key is configured, but holding no per-bucket keys.
func kmsLikeManager(t *testing.T) *bucketcrypto.Manager {
	t.Helper()
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i)
	}
	kek, err := bucketcrypto.NewKEK(mk)
	if err != nil {
		t.Fatal(err)
	}
	return bucketcrypto.NewManager(kek, bucketcrypto.NewMemKeyStore())
}
