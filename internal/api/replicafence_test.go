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

// A version's copy is checked against that version's record and stored as
// that version; replica-get serves it. Versions were never copied, so on a
// cluster each lived on the node that wrote it alone.
func TestReplicaCopyOfAVersion(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("vb"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObjectVersion(metadata.ObjectMeta{Bucket: "vb", Key: "k", VersionID: "v1", Size: 4, ETag: `"e1"`}); err != nil {
		t.Fatal(err)
	}
	put := func(version, body, etag string) int {
		req := httptest.NewRequest(http.MethodPost, "/cluster/replica-put?bucket=vb&key=k&version="+version, strings.NewReader(body))
		req.Header.Set(clusterSecretHeader, "s")
		req.Header.Set(ReplicaETagHeader, etag)
		rr := httptest.NewRecorder()
		h.ClusterReplicaPutHandler("s")(rr, req)
		return rr.Code
	}
	if c := put("v1", "vers", `"other"`); c != http.StatusConflict {
		t.Errorf("a copy with another ETag: %d", c)
	}
	if c := put("v2", "vers", `"e1"`); c != http.StatusConflict {
		t.Errorf("a copy of a version with no record: %d", c)
	}
	if c := put("..%2Fx", "vers", `"e1"`); c != http.StatusBadRequest {
		t.Errorf("a version ID naming another path: %d", c)
	}
	if c := put("v1", "vers", `"e1"`); c != http.StatusOK {
		t.Fatalf("the version's copy: %d", c)
	}
	if h.engine.ObjectExists("vb", "k") {
		t.Error("the version was stored at the plain path")
	}
	req := httptest.NewRequest(http.MethodGet, "/cluster/replica-get?bucket=vb&key=k&version=v1", nil)
	req.Header.Set(clusterSecretHeader, "s")
	rr := httptest.NewRecorder()
	h.ClusterReplicaGetHandler("s")(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "vers" {
		t.Errorf("replica-get of the version: %d %q", rr.Code, rr.Body.String())
	}
}
