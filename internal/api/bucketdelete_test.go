package api

import (
	"net/http"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// The dashboard decided a bucket was empty by counting files on disk, a count
// that skips .vs/, where a versioning enabled bucket keeps every object. It then
// deleted the bucket with everything in it.
func TestDashboardRefusesToDeleteAVersionedBucketThatHoldsData(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	if err := store.CreateBucket("vb"); err != nil {
		t.Fatal(err)
	}
	h.engine.CreateBucketDir("vb")
	store.SetBucketVersioning("vb", "Enabled")
	if err := store.PutObjectVersion(metadata.ObjectMeta{Bucket: "vb", Key: "k", VersionID: "v1", IsLatest: true, Size: 1}); err != nil {
		t.Fatal(err)
	}

	if rr := doRequest(h, "DELETE", "/buckets/vb", nil, token); rr.Code != http.StatusConflict {
		t.Errorf("deleting a versioned bucket that holds a version: %d %s, want 409", rr.Code, rr.Body.String())
	}
	if !store.BucketExists("vb") {
		t.Error("the bucket was deleted")
	}
}
