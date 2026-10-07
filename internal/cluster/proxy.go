package cluster

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// traceForward, when set via VAULTS3_TRACE_FORWARD=1, makes every proxied request
// log a latency breakdown of the upstream hop (DNS, connect, connection reuse,
// time-to-first-byte). It is off by default (zero overhead) and exists to diagnose
// where a large fixed GET TTFB comes from in a real deployment: an extra pod hop, a
// slow DNS lookup (e.g. k8s ndots search), or a cold connection setup (issue #38).
var traceForward = os.Getenv("VAULTS3_TRACE_FORWARD") == "1"

// Proxy handles forwarding S3 requests to the correct node in the cluster
// based on the hash ring placement.
type Proxy struct {
	ring      *HashRing
	node      *Node
	placement PlacementConfig
	nodeAddrs map[string]string // nodeID → "host:apiPort"
	// configured holds the cluster.peer_apis entries. The membership sync derives
	// peer addresses from Raft, which carries no API port, so it assumes every
	// node serves on this one's. That holds for one node per host but not when
	// nodes share a host on different ports, where every peer then resolved to
	// THIS node: object forwarding, rebalance, and the capacity rollup all
	// silently talked to the wrong node. An explicitly configured address wins.
	// Only peers belong here: this node's own entry stays derived, since the
	// configured form is a bind address that may be a wildcard.
	configured map[string]string
	mu         sync.RWMutex
	proxies    map[string]*httputil.ReverseProxy // cached per-node proxies
	// onMembership hears every membership sync, so state kept elsewhere about
	// the peers (the failure detector's probe list) follows the same source.
	onMembership func(members map[string]string)
}

// setMembershipObserver registers fn to receive the member set after every
// membership sync.
func (p *Proxy) setMembershipObserver(fn func(members map[string]string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onMembership = fn
}

// NewProxy creates a new cluster proxy.
func NewProxy(ring *HashRing, node *Node, placement PlacementConfig, nodeAddrs map[string]string) *Proxy {
	applyPlacementDefaults(&placement)
	return &Proxy{
		ring:      ring,
		node:      node,
		placement: placement,
		nodeAddrs: nodeAddrs,
		proxies:   make(map[string]*httputil.ReverseProxy),
	}
}

// SetPeerAPIs pins peers to explicitly configured API addresses, so the
// membership sync cannot replace them with addresses derived from Raft.
func (p *Proxy) SetPeerAPIs(peers map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configured = make(map[string]string, len(peers))
	for id, addr := range peers {
		if addr != "" {
			p.configured[id] = addr
		}
	}
}

// RunMembershipSync keeps the hash ring and the node-address map in step with the
// live Raft membership, so data placement is identical on every node. This is
// what makes auto-clustering work: nodes join dynamically (not via static config),
// so the ring must follow the cluster, not a fixed peer list. apiPort is the API
// port every node serves on (Raft addresses carry the raft port, which we swap).
func (p *Proxy) RunMembershipSync(ctx context.Context, apiPort int) {
	p.syncMembership(apiPort)
	// Reconcile the ring to live Raft membership frequently so the window where two
	// pods disagree about a key's owner (and a read is routed to a node that lacks
	// it) stays small during membership churn (issue #37).
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.syncMembership(apiPort)
		}
	}
}

func (p *Proxy) syncMembership(apiPort int) {
	servers, err := p.node.Servers()
	if err != nil {
		return
	}
	selfID := ""
	if p.node != nil {
		selfID = p.node.NodeID()
	}
	members := make(map[string]string, len(servers))
	for _, s := range servers {
		id := string(s.ID)
		// A configured peer_apis entry is exact; the derived one is a guess that
		// reuses this node's API port for everyone.
		if addr := p.configuredAddr(id); addr != "" && id != selfID {
			members[id] = addr
			continue
		}
		host := string(s.Address)
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		members[id] = fmt.Sprintf("%s:%d", host, apiPort)
	}

	p.mu.Lock()
	observer := p.onMembership
	if observer != nil && len(members) > 0 {
		snapshot := make(map[string]string, len(members))
		for id, addr := range members {
			snapshot[id] = addr
		}
		// Told after the lock is released, so the observer may call back in.
		defer observer(snapshot)
	}
	defer p.mu.Unlock()
	// Drop cached reverse proxies whose address changed or whose node left, so the
	// next request rebuilds them against the current address. Pods get a new IP on
	// restart, and without this a node would route to a dead address forever.
	p.invalidateStaleProxiesLocked(members)
	p.nodeAddrs = members
	// Reconcile the ring to exactly the live member set.
	want := make(map[string]bool, len(members))
	for id := range members {
		want[id] = true
	}
	for _, id := range p.ring.Nodes() {
		if !want[id] {
			p.ring.RemoveNode(id)
		}
	}
	for id := range members {
		if !p.ring.HasNode(id) {
			p.ring.AddNode(id)
		}
	}
}

// OwnerAPIAddr returns the API address of the node that owns (bucket, key) when
// that is NOT this node, so callers can place or fetch the object there. Returns
// ("", false) when this node is the owner.
func (p *Proxy) OwnerAPIAddr(bucket, key string) (string, bool) {
	target := p.ShouldProxy(bucket, key)
	if target == "" {
		return "", false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	addr, ok := p.nodeAddrs[target]
	if !ok {
		return "", false
	}
	return addr, true
}

// ForwardReadToLeader forwards a consistency-sensitive read to the current Raft
// leader and reports whether it did so. The leader is the only node guaranteed to
// have every committed write applied to its FSM (raft.Apply completes after the
// local apply), so a bucket listing served there cannot miss a key a client just
// PUT, whereas a lagging follower's local index can (issue #37). Object GET/HEAD do
// not need this because they wait out replication with a per-key barrier; a listing
// has no single key to wait on, so we route it to the authoritative node instead.
//
// Returns true when the request was forwarded (caller must stop). Returns false when
// this node should serve it locally: it is the leader, or no leader is currently
// known (local is then the best available answer). Uses no inter-node RPC, only the
// leader identity Raft already tracks, so it is robust to the proxy/network topology
// that made the earlier read-index approach fail.
func (p *Proxy) ForwardReadToLeader(w http.ResponseWriter, r *http.Request) bool {
	if p.node.IsLeader() {
		return false
	}
	leader := p.node.LeaderID()
	if leader == "" || leader == p.node.NodeID() {
		return false
	}
	p.ForwardRequest(w, r, leader)
	return true
}

// ShouldProxy checks if a request for the given bucket/key should be proxied
// to another node. Returns the target node ID if proxying is needed,
// or empty string if this node should handle it.
func (p *Proxy) ShouldProxy(bucket, key string) string {
	if bucket == "" {
		// Service-level operations (ListBuckets) — handle locally
		return ""
	}

	// For bucket-level operations (key == ""), hash on just the bucket
	hashKey := key
	if hashKey == "" {
		hashKey = ""
	}

	primaryNode := p.ring.GetNode(bucket, hashKey)
	if primaryNode == "" || primaryNode == p.node.NodeID() {
		return ""
	}

	return primaryNode
}

// ForwardRequest proxies an HTTP request to the specified target node, telling the
// client to retry if the hop fails.
func (p *Proxy) ForwardRequest(w http.ResponseWriter, r *http.Request, targetNodeID string) {
	if !p.forwardOnce(w, r, targetNodeID) {
		WriteUnavailable(w)
	}
}

// WriteUnavailable reports that no node could serve the request right now.
//
// It answers 503 with an S3 error document rather than a bare text 502. Both
// details matter to a client: 503 SlowDown is the S3 way of saying "this is
// temporary, try again", and every mainstream SDK retries it automatically,
// whereas a 502 surfaced to the user as an unexplained "Bad Gateway" (issue #42).
// Sending XML also means the client can name the error instead of guessing at an
// HTML or plain-text body from something it cannot identify.
func WriteUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
	io.WriteString(w, xml.Header+
		`<Error><Code>SlowDown</Code>`+
		`<Message>The node holding this data is temporarily unavailable, please retry</Message>`+
		`</Error>`)
}

// forwardOnce proxies the request to targetNodeID and reports whether the client
// got a response. It returns false ONLY when the hop failed before a single byte
// reached the client, which is exactly the case where the caller may safely try
// another node instead of failing the request (issue #42). Nothing is written to w
// on a false return.
func (p *Proxy) forwardOnce(w http.ResponseWriter, r *http.Request, targetNodeID string) bool {
	p.mu.RLock()
	addr, ok := p.nodeAddrs[targetNodeID]
	p.mu.RUnlock()

	if !ok {
		slog.Warn("proxy: unknown target node", "node_id", targetNodeID)
		return false
	}

	proxy := p.getOrCreateProxy(targetNodeID, addr)

	// Mark as internal cluster proxy to prevent infinite proxy loops
	r.Header.Set("X-VaultS3-Proxy", p.node.NodeID())

	slog.Debug("proxy: forwarding request",
		"method", r.Method,
		"path", r.URL.Path,
		"from", p.node.NodeID(),
		"to", targetNodeID,
		"addr", addr,
	)

	if traceForward {
		r = traceForwardRequest(r, targetNodeID, addr)
	}

	// The outcome cell lets ErrorHandler report an upstream failure back here
	// instead of writing a 502 straight to the client.
	outcome := &forwardOutcome{}
	r = r.WithContext(context.WithValue(r.Context(), forwardOutcomeKey{}, outcome))
	tw := &trackingWriter{ResponseWriter: w}

	proxy.ServeHTTP(tw, r)

	if outcome.err != nil && !tw.wrote {
		slog.Warn("proxy: upstream hop failed before any response",
			"target", targetNodeID, "addr", addr, "method", r.Method,
			"path", r.URL.Path, "error", outcome.err)
		return false
	}
	return true
}

// forwardOutcomeKey / forwardOutcome carry a failed hop's error back to
// forwardOnce through the request context.
type forwardOutcomeKey struct{}

type forwardOutcome struct{ err error }

// trackingWriter records whether anything was actually sent to the client, so a
// hop that fails midway through a response body is never mistaken for a
// retryable one.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

func (t *trackingWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// traceForwardRequest attaches an httptrace to the upstream request that logs a
// per-hop latency breakdown once the first response byte arrives. This isolates the
// cost of the extra pod hop (issue #38): DNS resolution, TCP connect, whether the
// connection was reused (keep-alive working), and upstream time-to-first-byte.
func traceForwardRequest(r *http.Request, targetNodeID, addr string) *http.Request {
	start := time.Now()
	var dnsStart, connectStart time.Time
	var dnsDur, connectDur time.Duration
	var reused bool
	trace := &httptrace.ClientTrace{
		DNSStart:     func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:      func(httptrace.DNSDoneInfo) { dnsDur = time.Since(dnsStart) },
		ConnectStart: func(string, string) { connectStart = time.Now() },
		ConnectDone:  func(string, string, error) { connectDur = time.Since(connectStart) },
		GotConn:      func(info httptrace.GotConnInfo) { reused = info.Reused },
		GotFirstResponseByte: func() {
			slog.Info("proxy: forward hop timing",
				"to", targetNodeID,
				"addr", addr,
				"path", r.URL.Path,
				"reused_conn", reused,
				"dns_ms", dnsDur.Milliseconds(),
				"connect_ms", connectDur.Milliseconds(),
				"ttfb_ms", time.Since(start).Milliseconds(),
			)
		},
	}
	return r.WithContext(httptrace.WithClientTrace(r.Context(), trace))
}

// IsProxied checks if a request was already proxied from another node.
func IsProxied(r *http.Request) bool {
	return r.Header.Get("X-VaultS3-Proxy") != ""
}

// configuredAddr returns the statically configured API address for a node, or ""
// if the deployment left it to be derived from Raft membership.
func (p *Proxy) configuredAddr(nodeID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.configured[nodeID]
}

// UpdateNodeAddr updates the API address for a node.
func (p *Proxy) UpdateNodeAddr(nodeID, addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodeAddrs[nodeID] = addr
	// Invalidate cached proxy
	delete(p.proxies, nodeID)
}

// RemoveNodeAddr removes the address mapping for a node.
func (p *Proxy) RemoveNodeAddr(nodeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.nodeAddrs, nodeID)
	delete(p.proxies, nodeID)
}

// invalidateStaleProxiesLocked deletes cached reverse proxies for nodes whose
// address in the new membership differs from the currently recorded one (a changed
// address or a departed node). The caller must hold p.mu.
func (p *Proxy) invalidateStaleProxiesLocked(members map[string]string) {
	for id, oldAddr := range p.nodeAddrs {
		if members[id] != oldAddr {
			delete(p.proxies, id)
		}
	}
}

func (p *Proxy) getOrCreateProxy(nodeID, addr string) *httputil.ReverseProxy {
	p.mu.RLock()
	if proxy, ok := p.proxies[nodeID]; ok {
		p.mu.RUnlock()
		return proxy
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock
	if proxy, ok := p.proxies[nodeID]; ok {
		return proxy
	}

	scheme := InterNodeScheme()
	target, err := url.Parse(fmt.Sprintf("%s://%s", scheme, addr))
	if err != nil {
		slog.Error("proxy: invalid target URL", "addr", addr, "error", err)
		target = &url.URL{Scheme: scheme, Host: addr}
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = newForwardTransport()
	secret := ""
	if p.node != nil {
		secret = p.node.cfg.Secret
	}
	director := proxy.Director
	proxy.Director = func(out *http.Request) {
		director(out)
		markForwardedClient(out, secret)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Hand the error to forwardOnce when it is watching, so it can try another
		// holder rather than turning a transient hiccup into a client-visible
		// failure (issue #42). Answer directly only when nobody is watching.
		if outcome, ok := r.Context().Value(forwardOutcomeKey{}).(*forwardOutcome); ok {
			outcome.err = err
			return
		}
		slog.Error("proxy: upstream error", "target", nodeID, "addr", addr, "error", err)
		WriteUnavailable(w)
	}

	p.proxies[nodeID] = proxy
	return proxy
}

// NodeAddrs returns a copy of the node address map.
func (p *Proxy) NodeAddrs() map[string]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	addrs := make(map[string]string, len(p.nodeAddrs))
	for k, v := range p.nodeAddrs {
		addrs[k] = v
	}
	return addrs
}

// Ring returns the underlying hash ring.
func (p *Proxy) Ring() *HashRing {
	return p.ring
}

// Placement returns the placement config.
func (p *Proxy) Placement() PlacementConfig {
	return p.placement
}
