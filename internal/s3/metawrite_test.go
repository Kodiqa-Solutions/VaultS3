package s3

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// errNoLeader is what a clustered metadata write returns when the Raft group has
// no leader: internal/metadata/distributed.go forwards the write and the forward
// fails. The bytes are already on disk at that point, which is exactly the
// situation that must not be reported as success.
var errNoLeader = errors.New("cluster: no leader to forward write to")

// failingMetaStore is a real store whose object-metadata writes fail, standing in
// for a node that has lost its Raft leader. Embedding the interface means the
// other 120-odd methods keep working normally.
type failingMetaStore struct {
	metadata.StoreAPI
	failPut     bool
	failDelete  bool
	failVersion bool
}

func (f *failingMetaStore) PutObjectMeta(m metadata.ObjectMeta) error {
	if f.failPut {
		return errNoLeader
	}
	return f.StoreAPI.PutObjectMeta(m)
}

func (f *failingMetaStore) DeleteObjectMeta(bucket, key string) error {
	if f.failDelete {
		return errNoLeader
	}
	return f.StoreAPI.DeleteObjectMeta(bucket, key)
}

func (f *failingMetaStore) PutObjectVersion(m metadata.ObjectMeta) error {
	if f.failVersion {
		return errNoLeader
	}
	return f.StoreAPI.PutObjectVersion(m)
}

func newFailingMetaServer(t *testing.T) (*failingMetaStore, *metadata.Store, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	real, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { real.Close() })
	engine, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	store := &failingMetaStore{StoreAPI: real}
	auth := NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil)
	ts := httptest.NewServer(NewHandler(store, engine, auth, false, "", nil))
	t.Cleanup(ts.Close)
	if err := real.CreateBucket("b"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return store, real, ts
}

// A PUT whose metadata write fails must NOT report success. Acknowledging it
// leaves a client believing an object exists that will never list and whose GET
// returns 404 forever, with the bytes orphaned on disk (found reviewing #50).
func TestPutReportsFailureWhenMetadataWriteFails(t *testing.T) {
	store, real, ts := newFailingMetaServer(t)
	store.failPut = true

	resp := doSigned(t, http.MethodPut, ts.URL+"/b/report.parquet", []byte("payload"))
	if resp.StatusCode == http.StatusOK {
		t.Fatal("PUT returned 200 while the metadata write failed: the client now believes in an object that does not exist")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so SDKs retry", resp.StatusCode)
	}
	// The object must be absent from the listing, matching what the client was told.
	if m, err := real.GetObjectMeta("b", "report.parquet"); err == nil && m != nil {
		t.Fatal("metadata exists after a failed write")
	}
}

// The same rule for a delete: metadata is authoritative (issue #34), so a delete
// that removed only the bytes must not answer 204.
func TestDeleteReportsFailureWhenMetadataWriteFails(t *testing.T) {
	store, real, ts := newFailingMetaServer(t)

	if resp := doSigned(t, http.MethodPut, ts.URL+"/b/gone.parquet", []byte("x")); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup PUT: %d", resp.StatusCode)
	}
	store.failDelete = true

	resp := doSigned(t, http.MethodDelete, ts.URL+"/b/gone.parquet", nil)
	if resp.StatusCode == http.StatusNoContent {
		t.Fatal("DELETE reported success while the object still has metadata and will keep listing")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if m, err := real.GetObjectMeta("b", "gone.parquet"); err != nil || m == nil {
		t.Fatal("metadata is gone, so the 503 misreported what happened")
	}
}

// A multi-object delete reports per key, so a failure must appear as an error
// entry for that key rather than being listed among the deleted.
func TestBatchDeleteReportsPerKeyMetadataFailure(t *testing.T) {
	store, _, ts := newFailingMetaServer(t)
	keys := []string{"one.parquet", "two.parquet"}
	for _, k := range keys {
		if resp := doSigned(t, http.MethodPut, ts.URL+"/b/"+k, []byte("x")); resp.StatusCode != http.StatusOK {
			t.Fatalf("setup PUT %s: %d", k, resp.StatusCode)
		}
	}
	store.failDelete = true

	resp := doSigned(t, http.MethodPost, ts.URL+"/b?delete", batchDeleteBody(keys...))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch delete status = %d", resp.StatusCode)
	}
	var out struct {
		Deleted []struct{ Key string } `xml:"Deleted"`
		Errors  []struct {
			Key  string
			Code string
		} `xml:"Error"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	resp.Body.Close()
	if len(out.Deleted) != 0 {
		t.Fatalf("%d keys reported deleted although every metadata delete failed", len(out.Deleted))
	}
	if len(out.Errors) != len(keys) {
		t.Fatalf("got %d errors, want %d", len(out.Errors), len(keys))
	}
	for _, e := range out.Errors {
		if e.Code != "SlowDown" {
			t.Fatalf("key %s reported code %q, want SlowDown", e.Key, e.Code)
		}
	}
}

// The guard must not fire when the store is healthy, or every write would 503.
func TestWritesSucceedWhenMetadataStoreIsHealthy(t *testing.T) {
	_, real, ts := newFailingMetaServer(t)
	for i := 0; i < 3; i++ {
		k := fmt.Sprintf("healthy-%d.parquet", i)
		if resp := doSigned(t, http.MethodPut, ts.URL+"/b/"+k, []byte("x")); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s = %d, want 200", k, resp.StatusCode)
		}
		if m, err := real.GetObjectMeta("b", k); err != nil || m == nil {
			t.Fatalf("%s was not recorded: %v", k, err)
		}
		if resp := doSigned(t, http.MethodDelete, ts.URL+"/b/"+k, nil); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE %s = %d, want 204", k, resp.StatusCode)
		}
	}
}
