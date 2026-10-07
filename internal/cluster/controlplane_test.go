package cluster

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// --- failure detector follows membership ---

// The detector learned its peers once at startup, so a node that joined later
// was never probed and a node whose address changed was probed at the old one
// and declared down. It must follow the membership sync.
func TestDetectorFollowsMembershipSync(t *testing.T) {
	var newHits int64
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&newHits, 1)
	}))
	defer moved.Close()
	joined := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer joined.Close()

	det := NewFailureDetector("self", DetectorConfig{SuspectAfter: 1, DownAfter: 1, ProbeTimeoutSecs: 1})
	det.AddNode("peer", "127.0.0.1:1") // the address it had at startup, now dead
	det.AddNode("gone", "127.0.0.1:1")

	p := &Proxy{ring: NewHashRing(16), node: &Node{cfg: ClusterConfig{NodeID: "self"}},
		nodeAddrs: map[string]string{}, proxies: map[string]*httputil.ReverseProxy{}}
	NewFailoverProxy(p, det)

	// What the membership sync reports now: peer moved, late joined, gone left.
	members := map[string]string{
		"self": "127.0.0.1:9",
		"peer": strings.TrimPrefix(moved.URL, "http://"),
		"late": strings.TrimPrefix(joined.URL, "http://"),
	}
	p.mu.Lock()
	obs := p.onMembership
	p.mu.Unlock()
	if obs == nil {
		t.Fatal("the failover proxy did not subscribe the detector to membership")
	}
	obs(members)

	det.probeAll()

	states := map[string]NodeHealth{}
	for _, nh := range det.NodeStates() {
		states[nh.NodeID] = nh
	}
	if _, ok := states["self"]; ok {
		t.Error("the detector monitors itself")
	}
	if _, ok := states["gone"]; ok {
		t.Error("a node that left the cluster is still monitored")
	}
	if nh, ok := states["late"]; !ok || nh.State != NodeHealthy {
		t.Errorf("a node that joined after startup is not monitored as healthy: %+v", nh)
	}
	if nh := states["peer"]; nh.State != NodeHealthy || atomic.LoadInt64(&newHits) == 0 {
		t.Errorf("a node whose address changed was not probed at its new address: %+v", nh)
	}
}

// The membership sync itself must hand the member set to the observer.
func TestMembershipSyncNotifiesObserver(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	self := nodes[0].node
	p := NewProxy(NewHashRing(16), self, PlacementConfig{ReplicaCount: 3}, map[string]string{})
	var mu sync.Mutex
	var got map[string]string
	p.setMembershipObserver(func(m map[string]string) {
		mu.Lock()
		got = m
		mu.Unlock()
	})
	p.syncMembership(9000)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("observer saw %d members, want 3: %v", len(got), got)
	}
}

// --- a failed hop that consumed body bytes is not retried elsewhere ---

// A hop that reads part of an upload and then dies must not be retried against
// the next holder with what is left of the body: with a chunked upload that
// holder cannot tell and stores a truncated object with a 200.
func TestForwardWithRetryDoesNotReplayPartlyReadBody(t *testing.T) {
	var secondGot int64
	var secondBody atomic.Value
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 10)
		io.ReadFull(r.Body, buf)
		hj, _ := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&secondGot, 1)
		b, _ := io.ReadAll(r.Body)
		secondBody.Store(string(b))
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	ring := NewHashRing(64)
	for _, id := range []string{"self", "a", "b"} {
		ring.AddNode(id)
	}
	// Find a key both peers hold and this node does not.
	key := ""
	for i := 0; i < 5000; i++ {
		k := "k" + strings.Repeat("z", i%5) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		holders := ring.GetNodes("b", k, 2)
		if len(holders) == 2 && holders[0] == "a" && holders[1] == "b" {
			key = k
			break
		}
	}
	if key == "" {
		t.Fatal("no key placed on a then b")
	}
	f := newTestProxy("self", map[string]string{
		"a": hostOf(t, first),
		"b": hostOf(t, second),
	}, ring, 2)

	payload := strings.Repeat("0123456789", 100*1024) // 1 MiB
	pr, pw := io.Pipe()
	go func() {
		// Chunked: no declared length, so a receiver cannot detect truncation.
		for i := 0; i < len(payload); i += 4096 {
			end := i + 4096
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := pw.Write([]byte(payload[i:end])); err != nil {
				return
			}
		}
		pw.Close()
	}()
	req := httptest.NewRequest(http.MethodPut, "/b/"+key, pr)
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	f.ForwardWithRetry(rec, req, "b", key)
	pr.CloseWithError(errors.New("test done"))

	if n := atomic.LoadInt64(&secondGot); n != 0 {
		got, _ := secondBody.Load().(string)
		t.Fatalf("the upload was retried on another holder after part of its body was consumed: it stored %d of %d bytes",
			len(got), len(payload))
	}
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadGateway {
		t.Fatalf("client saw %d, want 503 or 502 so it resends the whole upload", rec.Code)
	}
}

// --- forwarded requests carry the original client address ---

// The node that checks IP rules must see the client, not the node that
// forwarded the request, and only when the forwarding node proves itself with
// the cluster secret.
func TestForwardCarriesClientIPAndReceiverTrustsOnlyPeers(t *testing.T) {
	var seen atomic.Value
	backend := TrustForwardedClient("s3cret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.RemoteAddr + "|" + r.Header.Get(ClientIPHeader) + "|" + r.Header.Get(clusterSecretHeader))
	}))
	peer := httptest.NewServer(backend)
	defer peer.Close()

	f := newTestProxy("self", map[string]string{"peer": hostOf(t, peer)}, nil, 1)
	f.node.cfg.Secret = "s3cret"

	req := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	req.RemoteAddr = "203.0.113.7:51234"
	req.Header.Set(ClientIPHeader, "10.0.0.1") // a client trying to choose its own address
	rec := httptest.NewRecorder()
	f.ForwardRequest(rec, req, "peer")

	got, _ := seen.Load().(string)
	parts := strings.Split(got, "|")
	if len(parts) != 3 {
		t.Fatalf("backend not reached: %q", got)
	}
	if host, _, _ := net.SplitHostPort(parts[0]); host != "203.0.113.7" {
		t.Errorf("receiving node saw client %q, want the original client 203.0.113.7", parts[0])
	}
	if parts[1] != "" || parts[2] != "" {
		t.Errorf("trust headers leaked into the handler: %q", got)
	}

	// Straight from a client, the same headers must be ignored and removed.
	direct := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	direct.RemoteAddr = "198.51.100.9:4000"
	direct.Header.Set(ClientIPHeader, "10.0.0.1")
	direct.Header.Set(clusterSecretHeader, "wrong")
	backend.ServeHTTP(httptest.NewRecorder(), direct)
	got, _ = seen.Load().(string)
	if !strings.HasPrefix(got, "198.51.100.9:4000||") {
		t.Errorf("an unauthenticated client chose its own address or kept the headers: %q", got)
	}
}

// --- control-plane calls honour the TLS scheme ---

// With TLS on, every node-to-node call must use https: the cluster secret rides
// in a header on each of them.
func TestControlPlaneUsesConfiguredScheme(t *testing.T) {
	var gotTLS int64
	var gotPlain int64
	tlsPeer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&gotTLS, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"index": 7}`)
	}))
	defer tlsPeer.Close()
	addr := strings.TrimPrefix(tlsPeer.URL, "https://")
	tlsPeer.Config.ErrorLog = nil

	SetInterNodeScheme("https")
	defer SetInterNodeScheme("http")

	n := &Node{cfg: ClusterConfig{NodeID: "self", Secret: "s"}}
	if idx, err := n.peerAppliedIndex(addr); err != nil || idx != 7 {
		t.Errorf("readindex over TLS: idx=%d err=%v", idx, err)
	}

	r := &ShardRouter{nodeID: "self", secret: "s", timeout: 2 * time.Second}
	if resp, err := r.post(addr, shardCallPath, []byte(`{}`), 2*time.Second); err == nil {
		resp.Body.Close()
	} else {
		t.Errorf("shard call over TLS: %v", err)
	}

	if err := postJoin(t.Context(), InterNodeClient(2*time.Second), addr, []byte(`{}`), "s"); err != nil {
		t.Errorf("join over TLS: %v", err)
	}

	det := NewFailureDetector("self", DetectorConfig{SuspectAfter: 1, DownAfter: 1, ProbeTimeoutSecs: 2})
	det.AddNode("peer", addr)
	det.probeAll()
	if det.IsNodeDown("peer") {
		t.Error("health probe did not use https")
	}

	f := newTestProxy("self", map[string]string{"peer": addr}, nil, 1)
	rec := httptest.NewRecorder()
	f.ForwardRequest(rec, httptest.NewRequest(http.MethodGet, "/b/k", nil), "peer")
	if rec.Code != http.StatusOK {
		t.Errorf("data forward over TLS answered %d", rec.Code)
	}

	if atomic.LoadInt64(&gotTLS) < 5 || atomic.LoadInt64(&gotPlain) != 0 {
		t.Errorf("TLS peer saw %d requests, want at least 5", atomic.LoadInt64(&gotTLS))
	}
}

// --- /cluster/status needs the secret ---

func TestClusterStatusRequiresSecret(t *testing.T) {
	nodes := newRaftCluster(t, 1)
	n := nodes[0].node
	n.cfg.Secret = "s3cret"
	h := n.StatusHandler()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/cluster/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /cluster/status answered %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), n.cfg.NodeID) {
		t.Fatal("anonymous /cluster/status disclosed the node ID")
	}

	req := httptest.NewRequest(http.MethodGet, "/cluster/status", nil)
	req.Header.Set(clusterSecretHeader, "s3cret")
	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), n.cfg.NodeID) {
		t.Fatalf("authenticated /cluster/status answered %d: %s", rec.Code, rec.Body.String())
	}
}

// --- shard calls fail fast when members are down ---

// A member on a stopped host drops packets rather than refusing them. Each
// member must cost a short connect timeout, not the request timeout.
func TestShardCallFailsFastWhenMembersAreDown(t *testing.T) {
	probe := time.Now()
	if _, err := net.DialTimeout("tcp", "192.0.2.1:9000", 300*time.Millisecond); err == nil ||
		time.Since(probe) < 250*time.Millisecond {
		t.Skip("this network does not black-hole TEST-NET-1, so a down member cannot be simulated")
	}
	defer func(d time.Duration) { shardDialTimeout = d }(shardDialTimeout)
	shardDialTimeout = 200 * time.Millisecond

	addrs := map[string]string{"m1": "192.0.2.1:9000", "m2": "192.0.2.2:9000", "m3": "192.0.2.3:9000"}
	r := NewShardRouter("self", NewShardRuntime("self", t.TempDir(), nil, nil),
		func(id string) (string, bool) { a, ok := addrs[id]; return a, ok }, "s")
	r.SetMap(&ShardMap{Version: 1, Epoch: 1, Shards: 1, Replicas: 3,
		Members: [][]string{{"m1", "m2", "m3"}}, Founders: [][]string{{"m1", "m2", "m3"}}})

	start := time.Now()
	_, err := r.Call(metadata.ShardRequest{Bucket: "b"})
	if err == nil || !errors.Is(err, ErrShardUnavailable) {
		t.Fatalf("Call with every member down: %v, want ErrShardUnavailable", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Call took %s with every member down, want well under the request timeout", el)
	}

	start = time.Now()
	if err := r.Write("b", []byte(`{}`)); err == nil {
		t.Fatal("Write succeeded with every member down")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Write took %s with every member down", el)
	}
}

// --- a forwarded write that may have committed is reported as such ---

func TestForwardToLeaderReportsUnknownOutcome(t *testing.T) {
	var calls int64
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		io.ReadAll(r.Body)
		hj, _ := w.(http.Hijacker)
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close() // the leader got it, and the answer is lost
		}
	}))
	defer leader.Close()

	nodes := newRaftCluster(t, 1)
	n := nodes[0].node
	n.cfg.PeerAPIs = map[string]string{n.cfg.NodeID: hostOf(t, leader)}
	err := n.ForwardToLeader([]byte(`{}`))
	if err == nil || !errors.Is(err, ErrLeaderUnavailable) || !strings.Contains(err.Error(), "may have been applied") {
		t.Fatalf("err = %v, want ErrLeaderUnavailable saying the write may have been applied", err)
	}
	if c := atomic.LoadInt64(&calls); c != 1 {
		t.Fatalf("leader saw the write %d times, want exactly 1 (no silent retry)", c)
	}
}
