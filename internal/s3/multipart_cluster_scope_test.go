package s3

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Issue #47 bug B. In-progress multipart state is node-local (issue #32), but
// ListMultipartUploads is a bucket-level request routed to a single node and the
// per-upload operations route by object key. That mismatch produced two failures:
// the listing showed only the uploads whose key hashed to the listing node, and an
// upload stranded on its creating node by a hash-ring change answered NoSuchUpload
// to every abort forever.

type listUploadsResult struct {
	XMLName xml.Name `xml:"ListMultipartUploadsResult"`
	Uploads []struct {
		Key      string `xml:"Key"`
		UploadID string `xml:"UploadId"`
	} `xml:"Upload"`
}

func listUploads(t *testing.T, url string) listUploadsResult {
	t.Helper()
	resp := doSigned(t, http.MethodGet, url, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list uploads: status %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	var out listUploadsResult
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	return out
}

func TestListMultipartUploadsIncludesUploadsHeldByPeers(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)
	bucket := "b"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// One upload on this node.
	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"/local.bin?uploads", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create upload: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Two more that live on other nodes, as the peer lister reports them.
	handler.SetMultipartPeerLister(func(b string) []metadata.MultipartUpload {
		if b != bucket {
			return nil
		}
		return []metadata.MultipartUpload{
			{UploadID: "aaa", Bucket: bucket, Key: "remote-a.bin"},
			{UploadID: "bbb", Bucket: bucket, Key: "remote-b.bin"},
		}
	})

	got := listUploads(t, ts.URL+"/"+bucket+"?uploads")
	if len(got.Uploads) != 3 {
		t.Fatalf("listed %d uploads, want 3: an upload held by another node is invisible, so nothing can abort it or reclaim its parts", len(got.Uploads))
	}
	keys := map[string]bool{}
	for _, u := range got.Uploads {
		keys[u.Key] = true
	}
	for _, want := range []string{"local.bin", "remote-a.bin", "remote-b.bin"} {
		if !keys[want] {
			t.Errorf("upload %q missing from the listing", want)
		}
	}
}

// The merge must not double-count an upload a peer also reports (for instance a
// peer that still holds a record this node already has).
func TestListMultipartUploadsDeduplicatesAcrossNodes(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)
	bucket := "b"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"/dup.bin?uploads", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create upload: status %d", resp.StatusCode)
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	xml.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()

	handler.SetMultipartPeerLister(func(string) []metadata.MultipartUpload {
		return []metadata.MultipartUpload{{UploadID: init.UploadID, Bucket: bucket, Key: "dup.bin"}}
	})

	got := listUploads(t, ts.URL+"/"+bucket+"?uploads")
	if len(got.Uploads) != 1 {
		t.Fatalf("listed %d uploads, want 1 deduplicated", len(got.Uploads))
	}
}

// The listing merges peer responses whose arrival order varies, so the output
// must be ordered or clients see the same uploads reshuffle between calls.
func TestListMultipartUploadsIsStablyOrdered(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)
	bucket := "b"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	handler.SetMultipartPeerLister(func(string) []metadata.MultipartUpload {
		return []metadata.MultipartUpload{
			{UploadID: "3", Bucket: bucket, Key: "c.bin"},
			{UploadID: "1", Bucket: bucket, Key: "a.bin"},
			{UploadID: "2", Bucket: bucket, Key: "b.bin"},
		}
	})
	got := listUploads(t, ts.URL+"/"+bucket+"?uploads")
	want := []string{"a.bin", "b.bin", "c.bin"}
	for i, w := range want {
		if got.Uploads[i].Key != w {
			t.Fatalf("upload %d is %q, want %q — listing order must be stable", i, got.Uploads[i].Key, w)
		}
	}
}

// The phantom: an upload this node has no record of must be forwarded to the node
// that does, not answered NoSuchUpload.
func TestUploadOperationsFallBackToTheHolderNode(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)
	bucket := "b"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	var asked []string
	handler.SetMultipartHolderFallback(func(w http.ResponseWriter, _ *http.Request, uploadID string) bool {
		asked = append(asked, uploadID)
		w.WriteHeader(http.StatusNoContent) // stand-in for the holder's answer
		return true
	})

	// Abort of an upload that is not local: must reach the holder.
	resp := doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/gone.bin?uploadId=deadbeef", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("abort returned %d, want the holder's 204 — a listed upload that cannot be aborted is a permanent phantom", resp.StatusCode)
	}
	resp.Body.Close()

	// ListParts likewise.
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/gone.bin?uploadId=deadbeef", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ListParts returned %d, want the holder's answer", resp.StatusCode)
	}
	resp.Body.Close()

	if len(asked) != 2 {
		t.Fatalf("holder fallback consulted %d times, want 2 (abort + ListParts)", len(asked))
	}
}

// When no node holds the upload the client must still get a clean NoSuchUpload
// rather than a hang or a 500.
func TestUnknownUploadStillReturnsNoSuchUpload(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)
	bucket := "b"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	handler.SetMultipartHolderFallback(func(http.ResponseWriter, *http.Request, string) bool {
		return false // no peer has it
	})

	resp := doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/nope.bin?uploadId=deadbeef", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("abort of a genuinely unknown upload returned %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// A local upload must never pay for a peer round trip.
func TestLocalUploadNeverConsultsPeers(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)
	bucket := "b"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"/here.bin?uploads", nil)
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	xml.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()

	consulted := false
	handler.SetMultipartHolderFallback(func(http.ResponseWriter, *http.Request, string) bool {
		consulted = true
		return false
	})

	resp = doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/here.bin?uploadId="+init.UploadID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("abort of a local upload returned %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	if consulted {
		t.Error("a locally held upload triggered a peer lookup")
	}
}

// Regression (5.0.11): a part for an upload another node holds was forwarded
// only once the handler looked the upload up, after the router had decoded
// its aws-chunked body. The holder got decoded bytes under a STREAMING-*
// payload hash and without the Content-Encoding the client signed, and
// refused the part. The request is now forwarded exactly as it arrived.
func TestHolderGetsTheStreamingPartAsSent(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)
	if err := store.CreateBucket("hold"); err != nil {
		t.Fatal(err)
	}
	var gotEncoding, gotHash string
	var gotBody []byte
	handler.SetMultipartHolderFallback(func(w http.ResponseWriter, r *http.Request, uploadID string) bool {
		gotEncoding = r.Header.Get("Content-Encoding")
		gotHash = r.Header.Get("X-Amz-Content-Sha256")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		return true
	})
	part := bytes.Repeat([]byte("q"), 200<<10)
	r := signedStreamPut(t, ts, "/hold/big.bin?partNumber=1&uploadId=deadbeef", part, streamOpts{chunk: 64 << 10})
	if r.code != http.StatusOK {
		t.Fatalf("forwarded part: %d %s", r.code, r.body)
	}
	if gotEncoding != "aws-chunked" || gotHash != signedStreaming {
		t.Fatalf("holder got Content-Encoding %q and payload hash %q, want the request as signed", gotEncoding, gotHash)
	}
	if !bytes.Contains(gotBody, []byte(";chunk-signature=")) || len(gotBody) <= len(part) {
		t.Fatalf("holder got %d bytes without the chunk framing, want the body as sent", len(gotBody))
	}
}
