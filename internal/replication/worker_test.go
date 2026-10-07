package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// site is one VaultS3 node's store and engine.
type site struct {
	store  *metadata.Store
	engine *storage.FileSystem
}

func newSite(t *testing.T) site {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	engine, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	return site{store, engine}
}

// put stores an object the way the S3 handler would, data then metadata.
func (s site) put(t *testing.T, bucket, key, body string) {
	t.Helper()
	if !s.store.BucketExists(bucket) {
		if err := s.store.CreateBucket(bucket); err != nil {
			t.Fatal(err)
		}
		if err := s.engine.CreateBucketDir(bucket); err != nil {
			t.Fatal(err)
		}
	}
	n, etag, err := s.engine.PutObject(bucket, key, strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.PutObjectMeta(metadata.ObjectMeta{Bucket: bucket, Key: key, Size: n, ETag: etag, LastModified: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
}

// read returns an object's body, or "" with false when it is not there.
func (s site) read(t *testing.T, bucket, key string) (string, bool) {
	t.Helper()
	if _, err := s.store.GetObjectMeta(bucket, key); err != nil {
		return "", false
	}
	r, _, err := s.engine.GetObject(bucket, key)
	if err != nil {
		return "", false
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b), true
}

// peerServer runs the real S3 handler for a site, so every request the worker
// sends must also pass real signature verification. It records the decoded
// path of each request.
func peerServer(t *testing.T, s site) (*httptest.Server, func() []string) {
	t.Helper()
	auth := s3.NewAuthenticator("peer-ak", "peer-sk", s.store, nil, nil)
	h := s3.NewHandler(s.store, s.engine, auth, false, "", nil)
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func pushWorker(local site, peerURL string, engine storage.Engine) *Worker {
	if engine == nil {
		engine = local.engine
	}
	return NewWorker(local.store, engine, config.ReplicationConfig{
		Peers:      []config.ReplicationPeer{{Name: "p", URL: peerURL, AccessKey: "peer-ak", SecretKey: "peer-sk"}},
		MaxRetries: 3,
		BatchSize:  10,
	})
}

func queued(t *testing.T, s site) []metadata.ReplicationEvent {
	t.Helper()
	ev, err := s.store.DequeueReplication(100, time.Now().Unix()+1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// Push replication formatted the bucket and key into the peer URL, so the
// first '?' ended the path and a '#' began a fragment: replicating "a?b"
// overwrote the peer's "a", deleting "a#b" deleted the peer's "a", and "a%41b"
// landed as "aAb". Each case keeps a bystander on the peer.
func TestPushReplicationAddressesTheKeyNamed(t *testing.T) {
	for _, key := range []string{"a?b", "a#b", "a%41b", "dir/sp ace+plus", "x&y=z"} {
		t.Run(key, func(t *testing.T) {
			local, remote := newSite(t), newSite(t)
			srv, seen := peerServer(t, remote)
			remote.put(t, "bkt", "a", "peer bystander")
			remote.put(t, "bkt", "aAb", "peer bystander 2")
			local.put(t, "bkt", key, "replicated "+key)

			w := pushWorker(local, srv.URL, nil)
			if err := local.store.EnqueueReplication(metadata.ReplicationEvent{Peer: "p", Type: "put", Bucket: "bkt", Key: key}); err != nil {
				t.Fatal(err)
			}
			w.processQueue()

			if got, ok := remote.read(t, "bkt", key); !ok || got != "replicated "+key {
				t.Errorf("peer copy of %q = %q (present %v), requests %v", key, got, ok, seen())
			}
			if got, _ := remote.read(t, "bkt", "a"); got != "peer bystander" {
				t.Errorf("replicating %q changed the peer's bystander \"a\" to %q", key, got)
			}
			if got, _ := remote.read(t, "bkt", "aAb"); got != "peer bystander 2" {
				t.Errorf("replicating %q changed the peer's bystander \"aAb\" to %q", key, got)
			}
			if ev := queued(t, local); len(ev) != 0 {
				t.Errorf("event not acked: %+v", ev)
			}

			// And the delete reaches the same key, and only that key.
			if err := local.store.EnqueueReplication(metadata.ReplicationEvent{Peer: "p", Type: "delete", Bucket: "bkt", Key: key}); err != nil {
				t.Fatal(err)
			}
			w.processQueue()
			if _, ok := remote.read(t, "bkt", key); ok {
				t.Errorf("peer still holds %q after the delete, requests %v", key, seen())
			}
			if got, _ := remote.read(t, "bkt", "a"); got != "peer bystander" {
				t.Errorf("deleting %q removed or changed the peer's bystander \"a\"", key)
			}
		})
	}
}

func TestPeerURLKeepsABasePath(t *testing.T) {
	got, err := peerURL("https://peer.example/s3/", "bkt", "a?b c")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://peer.example/s3/bkt/a%3Fb%20c" {
		t.Errorf("peerURL = %s", got)
	}
}

// Peers come from the operator's config, like external_auth. The SSRF filter
// for caller-supplied URLs dropped a peer on a private or loopback address
// with only a warning, and every event for it was then dead-lettered.
func TestOperatorPeersOnPrivateAddressesAreKept(t *testing.T) {
	local := newSite(t)
	for _, u := range []string{"http://127.0.0.1:9000", "http://10.0.0.5:9000", "https://192.168.1.20", "http://[::1]:9000", "http://localhost:9000"} {
		w := pushWorker(local, u, nil)
		if _, ok := w.peers["p"]; !ok {
			t.Errorf("peer %s was dropped", u)
		}
		b := NewBiDirectionalWorker(local.store, local.engine, config.ReplicationConfig{
			Peers: []config.ReplicationPeer{{Name: "p", URL: u}},
		})
		if _, ok := b.peers["p"]; !ok {
			t.Errorf("bidirectional peer %s was dropped", u)
		}
	}
	for _, u := range []string{"ftp://10.0.0.5", "http://169.254.169.254/latest", "http://metadata.google.internal", "http://"} {
		if w := pushWorker(local, u, nil); len(w.peers) != 0 {
			t.Errorf("unusable peer %s was kept", u)
		}
	}
}

// failingEngine fails every read with the given error.
type failingEngine struct {
	storage.Engine
	err error
}

func (f failingEngine) GetObject(string, string) (storage.ReadSeekCloser, int64, error) {
	return nil, 0, f.err
}

// A read error other than "not found" acked the event, so the object was never
// replicated. It is now retried like any other transient failure, while an
// object that is really gone is still skipped.
func TestPushReplicationRetriesALocalReadError(t *testing.T) {
	local, remote := newSite(t), newSite(t)
	srv, _ := peerServer(t, remote)
	local.put(t, "bkt", "k", "body")

	w := pushWorker(local, srv.URL, failingEngine{local.engine, errors.New("input/output error")})
	if err := local.store.EnqueueReplication(metadata.ReplicationEvent{Peer: "p", Type: "put", Bucket: "bkt", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	w.processQueue()
	ev := queued(t, local)
	if len(ev) != 1 || ev[0].RetryCount != 1 {
		t.Fatalf("a read error was not kept for retry: %+v", ev)
	}

	gone := pushWorker(local, srv.URL, failingEngine{local.engine, fmt.Errorf("stat object: %w", errNotExist())})
	local.store.AckReplication(ev[0].ID)
	if err := local.store.EnqueueReplication(metadata.ReplicationEvent{Peer: "p", Type: "put", Bucket: "bkt", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	gone.processQueue()
	if ev := queued(t, local); len(ev) != 0 {
		t.Errorf("an object deleted locally was retried: %+v", ev)
	}
}

// errNotExist is what the filesystem engine returns for a missing object.
func errNotExist() error {
	return &fs.PathError{Op: "stat", Path: "/data/bkt/k", Err: fs.ErrNotExist}
}

// Active-active pulls fetched the object from a URL built the same way, so
// pulling "a?b" stored the peer's "a" under the name "a?b".
func TestPullReplicationFetchesTheKeyNamed(t *testing.T) {
	local, remote := newSite(t), newSite(t)
	srv, _ := peerServer(t, remote)
	remote.put(t, "bkt", "a", "bystander")
	remote.put(t, "bkt", "a?b", "the real a?b")

	w := NewBiDirectionalWorker(local.store, local.engine, config.ReplicationConfig{
		SiteID: "local",
		Peers:  []config.ReplicationPeer{{Name: "p", URL: srv.URL, AccessKey: "peer-ak", SecretKey: "peer-sk"}},
	})
	change := ChangeEntry{Bucket: "bkt", Key: "a?b", EventType: "put", SiteID: "remote", VectorClock: VectorClock{"remote": 1}}
	if err := w.applyRemoteChange(context.Background(), w.peers["p"], change); err != nil {
		t.Fatal(err)
	}
	if got, _ := local.read(t, "bkt", "a?b"); got != "the real a?b" {
		t.Errorf("pulled %q for a?b", got)
	}
	if _, ok := local.read(t, "bkt", "a"); ok {
		t.Error("pulling a?b created a")
	}
}

// syncPeerServer serves a fixed change log and the objects behind it. Objects
// listed in broken answer 500.
func syncPeerServer(t *testing.T, changes []ChangeEntry, broken map[string]bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_replication/sync" {
			var req SyncRequest
			json.NewDecoder(r.Body).Decode(&req)
			var out []ChangeEntry
			var last uint64
			for _, c := range changes {
				if c.Seq > req.SinceSeq {
					out = append(out, c)
				}
				last = c.Seq
			}
			json.NewEncoder(w).Encode(SyncResponse{SiteID: "remote", Changes: out, LastSeq: last})
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/bkt/")
		if broken[key] {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, "body of "+key)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A change that failed to apply was logged and then skipped for good: the
// cursor jumped to the peer's last sequence anyway, so it was never pulled
// again. The cursor now stops before the failed change, and the change is
// applied once the peer serves it.
func TestPullReplicationHoldsTheCursorOnAFailedChange(t *testing.T) {
	local := newSite(t)
	changes := []ChangeEntry{
		{Bucket: "bkt", Key: "one", EventType: "put", SiteID: "remote", VectorClock: VectorClock{"remote": 1}, Seq: 1},
		{Bucket: "bkt", Key: "two", EventType: "put", SiteID: "remote", VectorClock: VectorClock{"remote": 2}, Seq: 2},
		{Bucket: "bkt", Key: "three", EventType: "put", SiteID: "remote", VectorClock: VectorClock{"remote": 3}, Seq: 3},
	}
	broken := map[string]bool{"two": true}
	srv := syncPeerServer(t, changes, broken)
	w := NewBiDirectionalWorker(local.store, local.engine, config.ReplicationConfig{
		SiteID: "local",
		Peers:  []config.ReplicationPeer{{Name: "p", URL: srv.URL}},
	})

	if err := w.syncPeer(context.Background(), "p", w.peers["p"]); err != nil {
		t.Fatal(err)
	}
	if got := w.peerCursors["p"]; got != 1 {
		t.Errorf("cursor after a failed change = %d, want 1", got)
	}

	delete(broken, "two")
	if err := w.syncPeer(context.Background(), "p", w.peers["p"]); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"one", "two", "three"} {
		if got, ok := local.read(t, "bkt", k); !ok || got != "body of "+k {
			t.Errorf("%s = %q (present %v), want it applied", k, got, ok)
		}
	}
	if got := w.peerCursors["p"]; got != 3 {
		t.Errorf("cursor after recovery = %d, want 3", got)
	}
}

// The sync limit comes from the remote site and was used as given.
func TestSyncHandlerCapsTheBatch(t *testing.T) {
	local := newSite(t)
	prev := maxSyncBatch
	maxSyncBatch = 5
	t.Cleanup(func() { maxSyncBatch = prev })
	w := NewBiDirectionalWorker(local.store, local.engine, config.ReplicationConfig{SiteID: "local"})
	for i := 0; i < maxSyncBatch+5; i++ {
		if err := w.changeLog.Record("bkt", fmt.Sprintf("k%d", i), "put", "", 1, VectorClock{"local": uint64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	w.HandleSyncRequest(rec, httptest.NewRequest("POST", "/_replication/sync", strings.NewReader(`{"since_seq":0,"limit":100000000}`)))
	var resp SyncResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Changes) != maxSyncBatch {
		t.Errorf("sync answered %d changes, want the cap %d", len(resp.Changes), maxSyncBatch)
	}
	if resp.Changes[0].Seq == 0 {
		t.Error("changes carry no sequence, so a puller cannot resume after a failure")
	}
}

// failingMetaStore fails every metadata write.
type failingMetaStore struct {
	*metadata.Store
}

func (failingMetaStore) PutObjectMeta(metadata.ObjectMeta) error {
	return errors.New("metadata write failed")
}

func (failingMetaStore) DeleteObjectMeta(string, string) error {
	return errors.New("metadata write failed")
}

// A pull that wrote the data but failed to write its metadata reported
// success, so the object was on disk, listed nowhere, and never retried. The
// same held for a delete. Both now fail the change, which holds the cursor.
func TestPullReplicationReportsAFailedMetadataWrite(t *testing.T) {
	local, remote := newSite(t), newSite(t)
	srv, _ := peerServer(t, remote)
	remote.put(t, "bkt", "k", "body")
	local.put(t, "bkt", "gone", "local copy")

	w := NewBiDirectionalWorker(failingMetaStore{local.store}, local.engine, config.ReplicationConfig{
		SiteID: "local",
		Peers:  []config.ReplicationPeer{{Name: "p", URL: srv.URL, AccessKey: "peer-ak", SecretKey: "peer-sk"}},
	})
	put := ChangeEntry{Bucket: "bkt", Key: "k", EventType: "put", SiteID: "remote", VectorClock: VectorClock{"remote": 1}}
	if err := w.applyRemoteChange(context.Background(), w.peers["p"], put); err == nil {
		t.Error("a put whose metadata write failed was reported applied")
	}
	del := ChangeEntry{Bucket: "bkt", Key: "gone", EventType: "delete", SiteID: "remote", VectorClock: VectorClock{"remote": 2}}
	if err := w.applyRemoteChange(context.Background(), w.peers["p"], del); err == nil {
		t.Error("a delete whose metadata write failed was reported applied")
	}
	if got, ok := local.read(t, "bkt", "gone"); !ok || got != "local copy" {
		t.Error("a failed delete still removed the object's data")
	}
}
