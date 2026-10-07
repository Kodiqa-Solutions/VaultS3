package cluster

import (
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
)

// countingBody counts the bytes read from a request body, so the caller can
// tell whether a failed hop consumed any of it.
type countingBody struct {
	io.ReadCloser
	read atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// FailoverProxy extends the basic Proxy with failure-aware routing.
// When the primary node is down, requests are forwarded to the next
// healthy replica in the hash ring.
type FailoverProxy struct {
	*Proxy
	detector *FailureDetector
	// replicasFor resolves a bucket's replica count, overriding the cluster
	// default when that bucket asks for a different level of protection.
	replicasFor func(bucket string) int
}

// NewFailoverProxy creates a failover-aware proxy.
//
// The detector is subscribed to the proxy's membership sync, so it probes the
// nodes the cluster has now, at the addresses they have now, rather than the set
// it was given at startup.
func NewFailoverProxy(proxy *Proxy, detector *FailureDetector) *FailoverProxy {
	if proxy != nil && detector != nil {
		proxy.setMembershipObserver(detector.Reconcile)
	}
	return &FailoverProxy{
		Proxy:    proxy,
		detector: detector,
	}
}

// ShouldProxy returns the target node for a request, accounting for failed nodes.
// Returns empty string if this node should handle the request locally.
//
// Only the first replica_count nodes on the ring actually hold an object's data.
// A read served by any other node returns a phantom "Object not found" for a live
// object, so we route strictly within that holder set — we do NOT widen it for
// failover, because a node outside it has nothing to serve (issue #37). This is the
// crux of the read-after-write miss: a healthy owner that a per-node failure
// detector has (possibly wrongly) marked down must NOT cause the read to be
// answered by a data-less node; it is forwarded to the owner regardless.
func (f *FailoverProxy) ShouldProxy(bucket, key string) string {
	if bucket == "" {
		return ""
	}

	holders := f.ring.GetNodes(bucket, key, f.dataReplicas(bucket))
	if len(holders) == 0 {
		return ""
	}
	selfID := f.node.NodeID()

	// If this node holds the data, serve locally.
	for _, nodeID := range holders {
		if nodeID == selfID {
			return ""
		}
	}

	// Forward to the first healthy holder.
	for _, nodeID := range holders {
		if f.detector == nil || !f.detector.IsNodeDown(nodeID) {
			return nodeID
		}
		slog.Debug("failover: holder marked down", "node_id", nodeID, "bucket", bucket, "key", key)
	}

	// Every holder is marked down. Forward to the primary owner anyway rather than
	// answering from this node (which has no data): the "down" may be a false
	// positive — in which case the read succeeds — and if the owner really is down,
	// an honest upstream error beats a misleading 404 (issue #37).
	slog.Warn("failover: all data holders marked down, forwarding to primary owner",
		"owner", holders[0], "bucket", bucket, "key", key)
	return holders[0]
}

// SetReplicaPolicy wires a per-bucket replica count, so a bucket holding data
// that is cheap to recreate can be stored once while the rest keeps its copies
// (issue #39). Unset means every bucket uses the cluster default.
func (f *FailoverProxy) SetReplicaPolicy(fn func(bucket string) int) { f.replicasFor = fn }

// dataReplicas is the number of ring nodes that hold this bucket's data — the set
// a read may legitimately be served from. Never less than 1.
//
// Lowering a bucket's replica count leaves earlier objects with copies on nodes
// that are no longer holders. Reads stay correct because the ring order is stable,
// so the primary holder is unchanged and still has the data; the surplus copies
// are simply dead weight until the object is deleted (a delete reaps every node)
// or a rebalance runs.
func (f *FailoverProxy) dataReplicas(bucket string) int {
	n := f.placement.ReplicaCount
	if f.replicasFor != nil {
		if perBucket := f.replicasFor(bucket); perBucket > 0 {
			n = perBucket
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

// ForwardWithRetry forwards a request to the node that should serve it, moving on
// to the next holder when a hop fails before the client has seen anything.
//
// The retry is what keeps a momentary blip off the client: a pooled connection the
// peer just closed, a peer whose accept queue is briefly full, a pod restarting.
// Each of those used to surface as a 502 Bad Gateway that succeeded the moment the
// caller tried again (issue #42) — so the caller's retry is one we can do here,
// before the error ever leaves the cluster.
func (f *FailoverProxy) ForwardWithRetry(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
	target := f.ShouldProxy(bucket, key)
	if target == "" {
		return false // handle locally
	}

	// A hop that fails before answering may still have read part of the request
	// body. Sending what is left of it to the next holder would store a truncated
	// object, and with a chunked upload, which declares no length, that holder
	// cannot tell and answers 200. Once any byte of the body has gone, the only
	// safe answer is to fail the request so the client sends it again whole.
	var body *countingBody
	if r.Body != nil && r.Body != http.NoBody {
		body = &countingBody{ReadCloser: r.Body}
		r.Body = body
	}
	for _, node := range f.forwardCandidates(bucket, key, target) {
		if f.forwardOnce(w, r, node) {
			return true
		}
		if body != nil && body.read.Load() > 0 {
			slog.Error("proxy: hop failed after reading part of the request body, not retrying on another holder",
				"bucket", bucket, "key", key, "node", node, "body_bytes_sent", body.read.Load())
			WriteUnavailable(w)
			return true
		}
	}

	slog.Error("proxy: every candidate node failed", "bucket", bucket, "key", key, "primary", target)
	WriteUnavailable(w)
	return true
}

// forwardCandidates lists the nodes a request may be tried against, target first
// and then the object's other data holders. Only holders are included: a node
// outside the holder set has no copy of the object and would answer a read with a
// phantom 404 (issue #37).
func (f *FailoverProxy) forwardCandidates(bucket, key, target string) []string {
	candidates := []string{target}
	selfID := f.node.NodeID()
	for _, id := range f.ring.GetNodes(bucket, key, f.dataReplicas(bucket)) {
		if id != target && id != selfID {
			candidates = append(candidates, id)
		}
	}
	return candidates
}

// ForwardToDataHolder re-routes a read that this node cannot satisfy to a peer
// that holds the object's data, and reports whether one served it.
//
// It closes the window opened by asynchronous replica placement. Object metadata
// travels through Raft and lands on every node at once, but the object's BYTES are
// pushed to the other holders in the background after the write completes. Until
// that push lands, a holder can be in a state where its metadata says the object
// exists while its disk does not have it — and because any holder may serve a read
// locally, a GET arriving there returned "not found" for an object that had just
// been written successfully (issue #42, and the same desync behind the rclone
// reports in issue #40). The bigger the object the wider the window: 4 MiB objects
// missed on 43% of reads in a 3-node cluster under load.
//
// Metadata still decides whether the object exists. This only answers the separate
// question of WHICH node has the bytes, so a delete stays a delete (issue #34).
//
// Returns whether a peer served the read, and whether any holder was unreachable.
// The caller needs both: an object whose holder is merely restarting is
// temporarily unavailable rather than absent, and calling it "not found" is a
// wrong answer the client cannot recover from.
func (f *FailoverProxy) ForwardToDataHolder(w http.ResponseWriter, r *http.Request, bucket, key string) (served, unreachable bool) {
	if r.Header.Get(dataFallbackHeader) != "" {
		return false, false // already a fallback hop: never bounce it around the ring
	}
	selfID := f.node.NodeID()
	r.Header.Set(dataFallbackHeader, selfID)

	peers := make([]string, 0, f.dataReplicas(bucket))
	for _, id := range f.ring.GetNodes(bucket, key, f.dataReplicas(bucket)) {
		if id != selfID {
			peers = append(peers, id)
		}
	}

	for i, id := range peers {
		// A peer whose own copy has not landed yet answers 404 too. Hold that
		// response back while another holder is left to ask, and only let it reach
		// the client if nobody has the data.
		pw := &peekWriter{ResponseWriter: w, discardNotFound: i < len(peers)-1}
		if !f.forwardOnce(pw, r, id) {
			unreachable = true
			continue
		}
		if !pw.discarded {
			slog.Debug("proxy: served read from peer holder after local data miss",
				"bucket", bucket, "key", key, "peer", id)
			return true, false
		}
	}
	return false, unreachable
}

// peekWriter withholds a proxied response until its status is known, so a 404 from
// a peer that is also still waiting for its replica copy can be dropped and the
// next holder tried. Anything it does commit to streams straight through, so a
// large object is never buffered.
type peekWriter struct {
	http.ResponseWriter
	discardNotFound bool
	hdr             http.Header
	committed       bool
	discarded       bool
}

func (p *peekWriter) Header() http.Header {
	if p.hdr == nil {
		p.hdr = make(http.Header)
	}
	return p.hdr
}

func (p *peekWriter) WriteHeader(code int) {
	if p.committed || p.discarded {
		return
	}
	if code == http.StatusNotFound && p.discardNotFound {
		p.discarded = true
		return
	}
	p.committed = true
	dst := p.ResponseWriter.Header()
	for k, v := range p.hdr {
		dst[k] = v
	}
	p.ResponseWriter.WriteHeader(code)
}

func (p *peekWriter) Write(b []byte) (int, error) {
	if p.discarded {
		return len(b), nil // swallow the rejected 404 body
	}
	if !p.committed {
		p.WriteHeader(http.StatusOK)
		if p.discarded {
			return len(b), nil
		}
	}
	return p.ResponseWriter.Write(b)
}

func (p *peekWriter) Flush() {
	if p.discarded || !p.committed {
		return
	}
	if f, ok := p.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// dataFallbackHeader marks a hop taken because the previous node had the object's
// metadata but not its data, so the receiving node knows not to bounce it on.
const dataFallbackHeader = "X-VaultS3-Data-Fallback"

// OnNodeDown is called when the detector declares a node as down.
// It updates the hash ring and proxy state.
func (f *FailoverProxy) OnNodeDown(nodeID string) {
	slog.Warn("failover: node down, traffic will route to replicas",
		"node_id", nodeID,
	)
	// Don't remove from ring — the ring is used to determine ownership.
	// The failover logic in ShouldProxy skips down nodes.
	// This preserves object placement for when the node recovers.
}

// OnNodeRecover is called when a previously down node comes back.
func (f *FailoverProxy) OnNodeRecover(nodeID string) {
	slog.Info("failover: node recovered, resuming normal routing",
		"node_id", nodeID,
	)
}
