package cluster

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Regression (5.0.15 review): a 5.0.14 peer ignores the version parameter on replica-get and answers
// about the key's plain file. Repair on an upgraded node then pulls the
// plain (pre-versioning, null) object's bytes and stores them as version v1.
func TestRepairNeverTakesAnOldPeersPlainFileForAVersion(t *testing.T) {
	f := newRepairFixture(t, 2)
	store := f.repairer.store.(*fakeStore)
	store.objects = nil
	store.versions = []metadata.ObjectMeta{{Bucket: f.bucket, Key: f.key, VersionID: "v1", Size: 4}}

	// Replace the peer with an "old" one: version is ignored.
	old := newFakeEngine()
	old.put(f.bucket, f.key, []byte("PLAN"))       // pre-versioning object at plain path
	old.put(f.bucket, f.key+"@v1", []byte("vers")) // v1, written on the old node
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/replica-get", func(w http.ResponseWriter, r *http.Request) {
		b, k := r.URL.Query().Get("bucket"), r.URL.Query().Get("key")
		rd, size, err := old.GetObject(b, k)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, rd)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f.repairer.proxy = NewProxy(f.repairer.ring, nil, PlacementConfig{ReplicaCount: 2},
		map[string]string{"self": "127.0.0.1:1", "peer": strings.TrimPrefix(srv.URL, "http://")})

	f.runScan()
	rc, _, err := f.engine.GetObjectVersion(f.bucket, f.key, "v1")
	if err != nil {
		return // not pulled: the old peer's answer says nothing about v1
	}
	got, _ := io.ReadAll(rc)
	if string(got) != "vers" {
		t.Fatalf("version v1 on the upgraded node now holds %q (the plain object's bytes), want \"vers\"", got)
	}
}

// Regression (5.0.15 review): repair replaces a local copy recorded as another object, which is exactly
// the state of a key whose client write on this node is between its Record
// and its metadata commit (objwrite.go). pullLocal takes no key lock, so it
// writes over the in-flight write's bytes while the writer holds the key.
func TestRepairWaitsForAClientWriteOfTheKey(t *testing.T) {
	f := newRepairFixture(t, 2)
	idx, err := metadata.OpenCopyIndex(t.TempDir() + "/c.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	f.repairer.SetCopyIndex(idx)
	// A client write of "new!" on this node: engine write done, Record done,
	// metadata (still naming "e", the previous object) not yet committed.
	unlock := storage.LockObjectKeyShared(f.bucket, f.key)
	f.engine.put(f.bucket, f.key, []byte("new!"))
	idx.Record(f.bucket, f.key, "", `"new"`, 4)
	f.peer.engine.put(f.bucket, f.key, []byte("old!"))

	done := make(chan struct{})
	go func() { f.runScan(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		unlock()
		return // repair waited for the writer: fine
	}
	unlock()
	rc, _, _ := f.engine.GetObject(f.bucket, f.key)
	b, _ := io.ReadAll(rc)
	if string(b) != "new!" {
		t.Fatalf("repair replaced the bytes of an in-flight client write (holding the key lock) with %q", b)
	}
}
