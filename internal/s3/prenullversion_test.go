package s3

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// An object written BEFORE versioning was enabled has no version record of its
// own. Hiding it behind a delete marker used to drop the only reference to it:
// the marker overwrote the latest pointer, nothing else named the object, so
// removing the marker could not bring it back and the bytes were orphaned on
// disk. That is silent data loss on a bucket the user had just asked to keep
// every version. S3 calls such an object the "null" version, and it must survive
// the marker like any other version does.

// objTestEnv is newObjTestServer plus the data directory, so a test can assert
// on what is actually left on disk rather than only on what the API reports.
type objTestEnv struct {
	store   *metadata.Store
	url     string
	dataDir string
}

func newPreVersionEnv(t *testing.T) *objTestEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	dataDir := filepath.Join(dir, "data")
	engine, err := storage.NewFileSystem(dataDir)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	auth := NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil)
	ts := httptest.NewServer(NewHandler(store, engine, auth, false, "", nil))
	t.Cleanup(ts.Close)
	return &objTestEnv{store: store, url: ts.URL, dataDir: dataDir}
}

func preVersionedObject(t *testing.T, bucket, key, body string) (*objTestEnv, string) {
	t.Helper()
	env := newPreVersionEnv(t)
	if err := env.store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	// Written while the bucket is still UNVERSIONED, so it gets no version id.
	if resp := doSigned(t, http.MethodPut, env.url+"/"+bucket+"/"+key, []byte(body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT before versioning: %d", resp.StatusCode)
	}
	meta, err := env.store.GetObjectMeta(bucket, key)
	if err != nil || meta == nil {
		t.Fatalf("no metadata after the pre-versioning write: %v", err)
	}
	if meta.VersionID != "" {
		t.Fatalf("setup is wrong: the object carries version id %q, so it is not a pre-versioning object", meta.VersionID)
	}
	if err := env.store.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}
	return env, meta.ETag
}

func TestDeleteMarkerOverPreVersioningObjectIsReversible(t *testing.T) {
	bucket, key := "adopted", "legacy.txt"
	env, _ := preVersionedObject(t, bucket, key, "written-before-versioning")

	resp := doSigned(t, http.MethodDelete, env.url+"/"+bucket+"/"+key, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	marker := resp.Header.Get("X-Amz-Version-Id")
	if marker == "" {
		t.Fatal("no delete marker version id returned")
	}
	if resp := doSigned(t, http.MethodGet, env.url+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET behind the marker = %d, want 404", resp.StatusCode)
	}

	// The object must still be addressable as the null version while hidden.
	resp = doSigned(t, http.MethodGet, env.url+"/"+bucket+"/"+key+"?versionId=null", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET ?versionId=null while hidden = %d, want 200: the object was not preserved as a version", resp.StatusCode)
	}
	if got := bodyOf(t, resp); got != "written-before-versioning" {
		t.Fatalf("null version body = %q", got)
	}

	// Removing the marker must bring it back.
	if resp := doSigned(t, http.MethodDelete, env.url+"/"+bucket+"/"+key+"?versionId="+marker, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE marker: %d", resp.StatusCode)
	}
	resp = doSigned(t, http.MethodGet, env.url+"/"+bucket+"/"+key, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after removing the marker = %d, want 200: the object was NOT restorable", resp.StatusCode)
	}
	if got := bodyOf(t, resp); got != "written-before-versioning" {
		t.Fatalf("restored body = %q, want the original bytes", got)
	}
}

// The hidden object must appear in ListObjectVersions as "null", which is how a
// client discovers it is still there to restore.
func TestPreVersioningObjectListsAsNullVersionBehindAMarker(t *testing.T) {
	bucket, key := "adopted2", "legacy.bin"
	env, _ := preVersionedObject(t, bucket, key, "keep-me")

	if resp := doSigned(t, http.MethodDelete, env.url+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatal("delete failed")
	}

	resp := doSigned(t, http.MethodGet, env.url+"/"+bucket+"?versions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list versions: %d", resp.StatusCode)
	}
	var out struct {
		Versions []struct {
			Key       string `xml:"Key"`
			VersionID string `xml:"VersionId"`
		} `xml:"Version"`
		Markers []struct {
			Key string `xml:"Key"`
		} `xml:"DeleteMarker"`
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := xml.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal versions: %v\n%s", err, body)
	}
	var sawNull int
	for _, v := range out.Versions {
		if v.Key == key && v.VersionID == "null" {
			sawNull++
		}
	}
	if sawNull != 1 {
		t.Fatalf("null version appears %d times in %s, want exactly 1", sawNull, body)
	}
	if len(out.Markers) != 1 {
		t.Fatalf("delete markers = %d, want 1", len(out.Markers))
	}
}

// Deleting the null version explicitly, while a marker hides it, must actually
// remove the bytes. They live at the ordinary object path rather than under
// .vs/, so a version-aware delete alone removes nothing and orphans the file:
// exactly the leak this whole fix exists to stop.
func TestDeletingTheNullVersionBehindAMarkerRemovesTheBytes(t *testing.T) {
	bucket, key := "adopted3", "legacy.dat"
	env, _ := preVersionedObject(t, bucket, key, "orphan-me-not")

	dataPath := filepath.Join(env.dataDir, bucket, key)
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("test cannot find the object on disk at %s: %v", dataPath, err)
	}

	if resp := doSigned(t, http.MethodDelete, env.url+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatal("delete failed")
	}
	if resp := doSigned(t, http.MethodDelete, env.url+"/"+bucket+"/"+key+"?versionId=null", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatal("deleting the null version failed")
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("the null version's bytes are still on disk at %s, orphaned with no metadata naming them", dataPath)
	}
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
