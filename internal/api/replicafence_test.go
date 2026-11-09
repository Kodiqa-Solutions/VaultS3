package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// A replica copy is stored only while it is the object the metadata names.
// Copies are sent in no order, and the copy of a previous object arriving
// after the new one put the old bytes back on every node (5.0.14 soak:
// 30 of 36 overwrites lost on three nodes).
func TestReplicaCopyOfAnOlderObjectIsRefused(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("rb"); err != nil {
		t.Fatal(err)
	}
	put := func(body, etag string) int {
		req := httptest.NewRequest(http.MethodPost, "/cluster/replica-put?bucket=rb&key=k", strings.NewReader(body))
		req.Header.Set(clusterSecretHeader, "s")
		if etag != "" {
			req.Header.Set(ReplicaETagHeader, etag)
		}
		rr := httptest.NewRecorder()
		h.ClusterReplicaPutHandler("s")(rr, req)
		return rr.Code
	}
	stored := func() string {
		rc, _, err := h.engine.GetObject("rb", "k")
		if err != nil {
			return ""
		}
		defer func() { _ = rc.Close() }()
		b, _ := io.ReadAll(rc)
		return string(b)
	}
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: "rb", Key: "k", Size: 3, ETag: `"new"`}); err != nil {
		t.Fatal(err)
	}
	if c := put("new", `"new"`); c != http.StatusOK || stored() != "new" {
		t.Fatalf("the current object's copy: %d, stored %q", c, stored())
	}
	if c := put("older-bytes", `"old"`); c != http.StatusConflict || stored() != "new" {
		t.Errorf("an older object's copy: %d, stored %q", c, stored())
	}
	if c := put("xyzw", `"new"`); c != http.StatusConflict || stored() != "new" {
		t.Errorf("a copy whose size is not the object's: %d, stored %q", c, stored())
	}
	if err := store.DeleteObjectMeta("rb", "k"); err != nil {
		t.Fatal(err)
	}
	if c := put("new", `"new"`); c != http.StatusConflict {
		t.Errorf("a copy of a deleted object: %d", c)
	}
	// A node that does not send the ETag yet (a rolling upgrade) is stored
	// as before.
	if c := put("legacy", ""); c != http.StatusOK || stored() != "legacy" {
		t.Errorf("a copy without an ETag: %d, stored %q", c, stored())
	}
}
