package s3

import (
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Regression (5.0.15 review): a node that missed an in-place overwrite of the null version (Suspended
// bucket) still holds the old bytes, recorded as the old ETag. GET without a
// versionId routes away (record != meta.ETag); GET ?versionId=null serves the
// old bytes under the new object's ETag.
func TestAVersionReadChecksTheCopyRecord(t *testing.T) {
	h, store, _, ts := newObjTestServer(t)
	idx, err := metadata.OpenCopyIndex(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	h.SetCopyIndex(idx)
	h.SetDataHolderFallback(func(http.ResponseWriter, *http.Request, string, string) (bool, bool) { return false, false })

	bucket, key := "clust", "doc.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketVersioning(bucket, "Suspended"); err != nil {
		t.Fatal(err)
	}
	if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("AAAA old bytes")); resp.StatusCode != http.StatusOK {
		t.Fatal("seed put failed")
	}
	cur, err := store.GetObjectVersion(bucket, key, "null")
	if err != nil || cur == nil {
		t.Fatalf("no null version: %v", err)
	}
	// Overwrite committed on another node (same size), not applied to bytes here.
	cur.ETag = `"0123456789abcdef0123456789abcdef"`
	if err := store.PutObjectVersion(*cur); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObjectMeta(*cur); err != nil {
		t.Fatal(err)
	}
	resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	t.Logf("GET latest: %d", resp.StatusCode)
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key+"?versionId=null", nil)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("GET ?versionId=null served %q with ETag %s although its copy is recorded as another object", got, resp.Header.Get("ETag"))
	}
}
