package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// fakeEngine is the slice of storage.Engine replica repair actually touches.
// The embedded interface is nil, so a method this feature should never call
// panics instead of quietly returning a zero value.
type fakeEngine struct {
	storage.Engine
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeEngine() *fakeEngine { return &fakeEngine{data: map[string][]byte{}} }

func (e *fakeEngine) put(bucket, key string, b []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.data[bucket+"/"+key] = b
}

func (e *fakeEngine) ObjectExists(bucket, key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.data[bucket+"/"+key]
	return ok
}

func (e *fakeEngine) ObjectSize(bucket, key string) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.data[bucket+"/"+key]
	if !ok {
		return 0, fmt.Errorf("not found")
	}
	return int64(len(b)), nil
}

func (e *fakeEngine) GetObject(bucket, key string) (storage.ReadSeekCloser, int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.data[bucket+"/"+key]
	if !ok {
		return nil, 0, fmt.Errorf("not found")
	}
	return nopSeekCloser{bytes.NewReader(b)}, int64(len(b)), nil
}

func (e *fakeEngine) PutObject(bucket, key string, r io.Reader, size int64) (int64, string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, "", err
	}
	e.put(bucket, key, b)
	return int64(len(b)), "", nil
}

// countingTransport records every request repair tries to make, including ones
// that never reach a server. A failed push and a push that was never attempted
// produce the same tallies, so the attempt itself is what has to be observed.
type countingTransport struct {
	inner http.RoundTripper
	mu    sync.Mutex
	paths map[string]int
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.paths[req.URL.Path]++
	c.mu.Unlock()
	return c.inner.RoundTrip(req)
}

func (c *countingTransport) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paths[path]
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// fakeStore supplies the object list. Everything else would panic, which is the
// point: repair must read metadata and nothing more.
type fakeStore struct {
	metadata.StoreAPI
	objects [][2]string
}

// ListBuckets is read once, before the object walk, so the walk never calls back
// into the store. Repair asks for it now, so the fake must answer.
func (s *fakeStore) ListBuckets() ([]metadata.BucketInfo, error) {
	seen := map[string]bool{}
	var out []metadata.BucketInfo
	for _, o := range s.objects {
		if !seen[o[0]] {
			seen[o[0]] = true
			out = append(out, metadata.BucketInfo{Name: o[0]})
		}
	}
	return out, nil
}

func (s *fakeStore) IterateAllObjects(fn func(bucket, key string, meta metadata.ObjectMeta) bool) error {
	for _, o := range s.objects {
		if !fn(o[0], o[1], metadata.ObjectMeta{Bucket: o[0], Key: o[1], Size: 4}) {
			return nil
		}
	}
	return nil
}

// peerStub stands in for another node, answering the two endpoints repair uses.
type peerStub struct {
	srv      *httptest.Server
	engine   *fakeEngine
	mu       sync.Mutex
	putCount int
	getCount int
	// forceStatus makes the stub answer probes with this status instead of
	// looking at its contents, standing in for a peer that is reachable but
	// broken (a 500, a proxy error) rather than one that is simply down.
	forceStatus int
}

func newPeerStub() *peerStub {
	p := &peerStub{engine: newFakeEngine()}
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/replica-get", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.getCount++
		p.mu.Unlock()
		p.mu.Lock()
		forced := p.forceStatus
		p.mu.Unlock()
		if forced != 0 {
			w.WriteHeader(forced)
			return
		}
		bucket, key := r.URL.Query().Get("bucket"), r.URL.Query().Get("key")
		if !p.engine.ObjectExists(bucket, key) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rd, size, _ := p.engine.GetObject(bucket, key)
		defer rd.Close()
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			io.Copy(w, rd)
		}
	})
	mux.HandleFunc("/cluster/replica-put", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.putCount++
		p.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		p.engine.put(r.URL.Query().Get("bucket"), r.URL.Query().Get("key"), b)
		w.WriteHeader(http.StatusOK)
	})
	p.srv = httptest.NewServer(mux)
	return p
}

func (p *peerStub) addr() string { return strings.TrimPrefix(p.srv.URL, "http://") }
func (p *peerStub) puts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.putCount
}

// keyOwnedBy finds a key whose ring primary is nodeID, so the test exercises the
// branch it means to rather than whichever one the hash happens to pick.
func keyOwnedBy(t *testing.T, ring *HashRing, bucket, nodeID string) string {
	t.Helper()
	for i := 0; i < 2000; i++ {
		k := fmt.Sprintf("obj-%d", i)
		if ring.GetNode(bucket, k) == nodeID {
			return k
		}
	}
	t.Fatalf("no key hashed to %s", nodeID)
	return ""
}

type repairFixture struct {
	repairer *ReplicaRepairer
	engine   *fakeEngine
	peer     *peerStub
	sent     *countingTransport
	bucket   string
	key      string
}

func newRepairFixture(t *testing.T, replicas int) *repairFixture {
	t.Helper()
	peer := newPeerStub()
	t.Cleanup(peer.srv.Close)

	ring := NewHashRing(64)
	ring.AddNode("self")
	ring.AddNode("peer")
	proxy := NewProxy(ring, nil, PlacementConfig{ReplicaCount: replicas},
		map[string]string{"self": "127.0.0.1:1", "peer": peer.addr()})

	bucket := "b1"
	key := keyOwnedBy(t, ring, bucket, "self")
	engine := newFakeEngine()
	store := &fakeStore{objects: [][2]string{{bucket, key}}}

	r := NewReplicaRepairer(store, engine, ring, proxy, "self", "s3cret", "http",
		config.RepairConfig{IntervalSecs: -1, MaxBandwidthMBps: 1000, BatchSize: 10})
	r.SetReplicaPolicy(func(string) int { return replicas })
	r.SetErasurePolicy(func(string) bool { return false })
	sent := &countingTransport{inner: http.DefaultTransport, paths: map[string]int{}}
	r.client.Transport = sent
	r.probeClient.Transport = sent
	return &repairFixture{repairer: r, engine: engine, peer: peer, sent: sent, bucket: bucket, key: key}
}

// runScan drives one pass synchronously so the assertions do not race the loop.
func (f *repairFixture) runScan() RepairStats {
	f.repairer.scan(t0ctx())
	return f.repairer.Status()
}

func TestRepairCopiesToHolderThatLostIt(t *testing.T) {
	f := newRepairFixture(t, 2)
	f.engine.put(f.bucket, f.key, []byte("data"))

	st := f.runScan()

	if st.Repaired != 1 {
		t.Fatalf("expected 1 repaired, got %+v", st)
	}
	if !f.peer.engine.ObjectExists(f.bucket, f.key) {
		t.Fatal("peer should have received the missing copy")
	}
}

// The guard that matters most, in its two separate halves. A peer that cannot
// be reached and a peer that answers but not with a 404 both say nothing about
// whether the object is there, and reading either as a lost copy would turn a
// brief partition into a cluster-wide copy storm.
//
// These are genuinely different code paths: a downed peer fails the request
// itself, while a broken one completes it with the wrong status. Testing only
// the first leaves the second free to regress, which is what happened here.
func TestRepairTreatsDownPeerAsUnknownNotMissing(t *testing.T) {
	f := newRepairFixture(t, 2)
	f.engine.put(f.bucket, f.key, []byte("data"))
	f.peer.srv.Close() // the peer is now down, not empty

	st := f.runScan()

	if st.Repaired != 0 {
		t.Fatalf("an unreachable peer must not be repaired, got %+v", st)
	}
	if st.Undecidable != 1 {
		t.Fatalf("expected the object to be recorded undecidable, got %+v", st)
	}
	// A push that fails and a push never attempted leave the same tallies
	// behind, so assert the copy was never even tried.
	if n := f.sent.count("/cluster/replica-put"); n != 0 {
		t.Fatalf("no copy should have been attempted, got %d attempts", n)
	}
}

func TestRepairTreatsErroringPeerAsUnknownNotMissing(t *testing.T) {
	f := newRepairFixture(t, 2)
	f.engine.put(f.bucket, f.key, []byte("data"))
	f.peer.mu.Lock()
	f.peer.forceStatus = http.StatusInternalServerError // reachable, but broken
	f.peer.mu.Unlock()

	st := f.runScan()

	if st.Repaired != 0 {
		t.Fatalf("a peer that never said 404 must not be repaired, got %+v", st)
	}
	if st.Undecidable != 1 {
		t.Fatalf("expected the object to be recorded undecidable, got %+v", st)
	}
	if n := f.sent.count("/cluster/replica-put"); n != 0 {
		t.Fatalf("no copy should have been attempted, got %d attempts", n)
	}
}

func TestRepairSkipsErasureCodedBuckets(t *testing.T) {
	f := newRepairFixture(t, 2)
	f.engine.put(f.bucket, f.key, []byte("data"))
	f.repairer.SetErasurePolicy(func(string) bool { return true })

	st := f.runScan()

	if st.Scanned != 0 || f.peer.puts() != 0 {
		t.Fatalf("erasure buckets belong to the erasure healer, got %+v", st)
	}
}

func TestRepairSkipsBucketsKeepingOneCopy(t *testing.T) {
	f := newRepairFixture(t, 1)
	f.engine.put(f.bucket, f.key, []byte("data"))

	st := f.runScan()

	if st.Scanned != 0 || f.peer.puts() != 0 {
		t.Fatalf("a single-copy bucket has nothing to repair, got %+v", st)
	}
}

// No node has the bytes any more. Repair must say so and change nothing:
// metadata still describes the object, and removing it here would convert an
// operator problem that may still be recoverable into real data loss.
func TestRepairReportsUnrecoverableAndDeletesNothing(t *testing.T) {
	f := newRepairFixture(t, 2)
	// neither side holds the object

	st := f.runScan()

	if st.Unrecoverable != 1 {
		t.Fatalf("expected 1 unrecoverable, got %+v", st)
	}
	if st.Repaired != 0 || f.peer.puts() != 0 {
		t.Fatalf("nothing should have been copied, got %+v", st)
	}
}

func TestRepairDefersToRunningRebalance(t *testing.T) {
	f := newRepairFixture(t, 2)
	f.engine.put(f.bucket, f.key, []byte("data"))
	f.repairer.SetRebalanceGuard(func() bool { return true })

	st := f.runScan()

	if st.Scanned != 0 || f.peer.puts() != 0 {
		t.Fatalf("repair must not run alongside a rebalance, got %+v", st)
	}
}

// A node that holds no copy itself still coordinates: it tells the node that is
// short one to pull from a node that has it, so the bytes never detour through
// the coordinator.
func TestRepairPullsWhenCoordinatorHasNoCopy(t *testing.T) {
	f := newRepairFixture(t, 2)
	f.peer.engine.put(f.bucket, f.key, []byte("data"))

	st := f.runScan()

	if st.Repaired != 1 {
		t.Fatalf("expected the local copy to be restored, got %+v", st)
	}
	if !f.engine.ObjectExists(f.bucket, f.key) {
		t.Fatal("this node should have pulled the copy it was missing")
	}
}

func t0ctx() context.Context { return context.Background() }

// A holder in the ring that this node has no address for is another way of
// knowing nothing. It is not evidence the copy is gone, and treating it as a
// gap would aim a push at an empty address on every pass.
func TestRepairTreatsUnaddressableHolderAsUnknownNotMissing(t *testing.T) {
	peer := newPeerStub()
	t.Cleanup(peer.srv.Close)

	ring := NewHashRing(64)
	ring.AddNode("self")
	ring.AddNode("peer")
	ring.AddNode("ghost") // in the ring, absent from membership
	proxy := NewProxy(ring, nil, PlacementConfig{ReplicaCount: 3},
		map[string]string{"self": "127.0.0.1:1", "peer": peer.addr()})

	bucket := "b1"
	key := keyOwnedBy(t, ring, bucket, "self")
	engine := newFakeEngine()
	engine.put(bucket, key, []byte("data"))
	peer.engine.put(bucket, key, []byte("data"))

	r := NewReplicaRepairer(&fakeStore{objects: [][2]string{{bucket, key}}},
		engine, ring, proxy, "self", "s3cret", "http",
		config.RepairConfig{IntervalSecs: -1, MaxBandwidthMBps: 1000, BatchSize: 10})
	r.SetReplicaPolicy(func(string) int { return 3 })
	r.SetErasurePolicy(func(string) bool { return false })
	sent := &countingTransport{inner: http.DefaultTransport, paths: map[string]int{}}
	r.client.Transport = sent
	r.probeClient.Transport = sent

	r.scan(context.Background())
	st := r.Status()

	if st.Repaired != 0 || st.Undecidable != 1 {
		t.Fatalf("an unaddressable holder must not be repaired, got %+v", st)
	}
	if n := sent.count("/cluster/replica-put"); n != 0 {
		t.Fatalf("no copy should have been attempted, got %d attempts", n)
	}
}

// A peer that accepts the connection and then says nothing is the shape of a
// node that has been powered off: the packets are dropped rather than refused,
// so nothing fails fast. The probe must give up on its own short deadline.
// Sharing the transfer client's timeout here let one dead node stall an entire
// scan for minutes per object, which a three-node container run surfaced and no
// unit test had covered.
func TestRepairProbeDoesNotHangOnSilentPeer(t *testing.T) {
	blocked := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked // accept, then never answer
	}))
	// Order matters: cleanups run last-registered-first, and Close waits for the
	// handler to return, so the handler has to be released before Close runs.
	t.Cleanup(silent.Close)
	t.Cleanup(func() { close(blocked) })

	ring := NewHashRing(64)
	ring.AddNode("self")
	ring.AddNode("peer")
	proxy := NewProxy(ring, nil, PlacementConfig{ReplicaCount: 2},
		map[string]string{"self": "127.0.0.1:1", "peer": strings.TrimPrefix(silent.URL, "http://")})

	bucket := "b1"
	key := keyOwnedBy(t, ring, bucket, "self")
	engine := newFakeEngine()
	engine.put(bucket, key, []byte("data"))

	r := NewReplicaRepairer(&fakeStore{objects: [][2]string{{bucket, key}}},
		engine, ring, proxy, "self", "s3cret", "http",
		config.RepairConfig{IntervalSecs: -1, MaxBandwidthMBps: 1000, BatchSize: 10})
	r.SetReplicaPolicy(func(string) int { return 2 })
	r.SetErasurePolicy(func(string) bool { return false })
	r.probeClient.Timeout = 100 * time.Millisecond // the transfer client stays minutes

	done := make(chan struct{})
	go func() { r.scan(context.Background()); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("scan did not finish: the probe is not using its own timeout")
	}
	if st := r.Status(); st.Repaired != 0 || st.Undecidable != 1 {
		t.Fatalf("a silent peer must be undecidable, got %+v", st)
	}
}

// reentrantStore fails loudly if anything reads it while the object walk is in
// progress. The real store holds its lock for the whole walk, so a callback that
// reads it again re-enters a lock this goroutine already holds. The store then
// deadlocks as soon as a writer queues behind it, and every request that touches
// metadata hangs forever while the node still answers its health check.
type reentrantStore struct {
	metadata.StoreAPI
	objects   [][2]string
	iterating atomic.Bool
	reentered atomic.Int32
}

func (s *reentrantStore) ListBuckets() ([]metadata.BucketInfo, error) {
	if s.iterating.Load() {
		s.reentered.Add(1)
	}
	seen := map[string]bool{}
	var out []metadata.BucketInfo
	for _, o := range s.objects {
		if !seen[o[0]] {
			seen[o[0]] = true
			out = append(out, metadata.BucketInfo{Name: o[0]})
		}
	}
	return out, nil
}

// durability stands in for the per-bucket lookups the server injects, which read
// the metadata store exactly like BucketDurability does in production.
func (s *reentrantStore) durability(string) int {
	if s.iterating.Load() {
		s.reentered.Add(1)
	}
	return 2
}

func (s *reentrantStore) IterateAllObjects(fn func(bucket, key string, meta metadata.ObjectMeta) bool) error {
	s.iterating.Store(true)
	defer s.iterating.Store(false)
	for _, o := range s.objects {
		if !fn(o[0], o[1], metadata.ObjectMeta{Bucket: o[0], Key: o[1], Size: 4}) {
			return nil
		}
	}
	return nil
}

func TestRepairNeverReadsTheStoreDuringTheObjectWalk(t *testing.T) {
	peer := newPeerStub()
	t.Cleanup(peer.srv.Close)

	ring := NewHashRing(64)
	ring.AddNode("self")
	ring.AddNode("peer")
	proxy := NewProxy(ring, nil, PlacementConfig{ReplicaCount: 2},
		map[string]string{"self": "127.0.0.1:1", "peer": peer.addr()})

	bucket := "b1"
	var objs [][2]string
	for i := 0; i < 25; i++ {
		objs = append(objs, [2]string{bucket, keyOwnedBy(t, ring, bucket, "self")})
	}
	store := &reentrantStore{objects: objs}

	engine := newFakeEngine()
	for _, o := range objs {
		engine.put(o[0], o[1], []byte("data"))
	}
	r := NewReplicaRepairer(store, engine, ring, proxy, "self", "s3cret", "http",
		config.RepairConfig{IntervalSecs: -1, MaxBandwidthMBps: 1000, BatchSize: 10})
	r.SetReplicaPolicy(store.durability)
	r.SetErasurePolicy(func(b string) bool { store.durability(b); return false })

	r.scan(context.Background())

	if n := store.reentered.Load(); n != 0 {
		t.Fatalf("the object walk read the metadata store %d times, which deadlocks it under a concurrent writer", n)
	}
}
