package cluster

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Rebalance must never move or delete data. The old pass PUT every object this
// node was not primary for to the primary, then deleted the local copy and the
// object's cluster-wide metadata. Here this node holds a secondary replica of an
// object whose primary is "other": a trigger must send nothing, delete nothing
// and leave no pass running.
func TestRebalanceMovesNothing(t *testing.T) {
	var hits int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
	}))
	defer peer.Close()

	ring := NewHashRing(64)
	ring.AddNode("self")
	ring.AddNode("other")
	// Find a key whose primary is the other node.
	key := ""
	for i := 0; i < 1000 && key == ""; i++ {
		k := "obj-" + strings.Repeat("x", i%7) + string(rune('a'+i%26))
		if ring.GetNode("b", k) == "other" {
			key = k
		}
	}
	if key == "" {
		t.Fatal("no key maps to the other node")
	}

	eng := &listingEngine{fakeEngine: newFakeEngine()}
	eng.put("b", key, []byte("replica bytes"))
	store := &deleteCountingStore{fakeStore: &fakeStore{objects: [][2]string{{"b", key}}}}
	proxy := &Proxy{
		ring:      ring,
		node:      &Node{cfg: ClusterConfig{NodeID: "self"}},
		nodeAddrs: map[string]string{"other": strings.TrimPrefix(peer.URL, "http://")},
		proxies:   map[string]*httputil.ReverseProxy{},
	}
	r := NewRebalancer(store, eng, ring, proxy, "self", RebalanceConfig{})
	r.Trigger()
	r.Trigger()
	time.Sleep(300 * time.Millisecond)
	for r.IsRunning() {
		time.Sleep(10 * time.Millisecond)
	}

	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Errorf("rebalance sent %d requests to a peer", n)
	}
	if !eng.ObjectExists("b", key) {
		t.Error("rebalance deleted this node's replica")
	}
	if n := atomic.LoadInt64(&store.deletes); n != 0 {
		t.Errorf("rebalance deleted object metadata %d times, which is cluster-wide", n)
	}
	if st := r.Status(); st.Running || st.Message == "" {
		t.Errorf("status = %+v, want not running with an explanation", st)
	}
}

type listingEngine struct{ *fakeEngine }

// ListObjects lists what the fake holds, so a pass that walks local data would
// find the replica.
func (e *listingEngine) ListObjects(bucket, prefix, startAfter string, max int) ([]storage.ObjectInfo, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []storage.ObjectInfo
	for k, v := range e.data {
		if b, key, ok := strings.Cut(k, "/"); ok && b == bucket && key > startAfter {
			out = append(out, storage.ObjectInfo{Key: key, Size: int64(len(v))})
		}
	}
	return out, false, nil
}

func (e *listingEngine) DeleteObject(bucket, key string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.data, bucket+"/"+key)
	return nil
}

type deleteCountingStore struct {
	*fakeStore
	deletes int64
}

func (s *deleteCountingStore) DeleteObjectMeta(bucket, key string) error {
	atomic.AddInt64(&s.deletes, 1)
	return nil
}

var _ metadata.StoreAPI = (*deleteCountingStore)(nil)
