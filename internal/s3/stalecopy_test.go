package s3

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
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

	if !h.localCopyNeedsContentCheck(recent, 10) {
		t.Fatal("an object written moments ago is exactly the one that can still be replicating")
	}
	old := *recent
	old.LastModified = now - int64(staleVerifyWindow.Seconds()) - 60
	if h.localCopyNeedsContentCheck(&old, 10) {
		t.Fatal("replication has long settled, verifying every read would cost the hash for nothing")
	}
	big := *recent
	if h.localCopyNeedsContentCheck(&big, staleVerifyMaxSize+1) {
		t.Fatal("hashing an object this large on read is worse than the problem it solves")
	}
	multipart := *recent
	multipart.ETag = `"d41d8cd98f00b204e9800998ecf8427e-4"`
	if h.localCopyNeedsContentCheck(&multipart, 10) {
		t.Fatal("a multipart ETag is not the MD5 of the object, so it cannot be recomputed here")
	}
	single := &ObjectHandler{}
	if single.localCopyNeedsContentCheck(recent, 10) {
		t.Fatal("a single-node server has no peer to fall back to")
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
