package cluster

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// ShardRouter places a bucket's object metadata on a shard group and gets the
// operation to a node that serves it (issue #50).
//
// It is the cluster half of metadata.ShardRouter. The store calls it; it never
// calls the store, so the request path is unchanged and the only hop a sharded
// deployment adds is this one, inside the metadata layer.
type ShardRouter struct {
	nodeID  string
	runtime *ShardRuntime
	// addrOf resolves a member id to its API address. It follows live membership
	// rather than a snapshot, because addresses are pod IPs that change on every
	// restart.
	addrOf  func(nodeID string) (string, bool)
	secret  string
	timeout time.Duration

	// committed is the assignment this node routes by. It is replaced wholesale
	// when a new map commits, never mutated, so a request either routes by the
	// old map or by the new one and never by half of each.
	committed atomic.Pointer[ShardMap]

	// leaderHint remembers which member last answered as a shard's leader. It is
	// a hint and nothing more: acting on a stale one costs one extra hop, which
	// is exactly what NOT having it costs on every request from a node that holds
	// no copy of the shard.
	mu         sync.RWMutex
	leaderHint map[int]string
}

// NewShardRouter builds the router. addrOf must resolve a node id to its current
// API address; Proxy.NodeAddrs is the live source in a running server.
func NewShardRouter(nodeID string, runtime *ShardRuntime, addrOf func(string) (string, bool), secret string) *ShardRouter {
	return &ShardRouter{
		nodeID:     nodeID,
		runtime:    runtime,
		addrOf:     addrOf,
		secret:     secret,
		timeout:    raftTimeout,
		leaderHint: make(map[int]string),
	}
}

func (r *ShardRouter) hintedLeader(shard int) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.leaderHint[shard]
}

func (r *ShardRouter) rememberLeader(shard int, nodeID string) {
	if nodeID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaderHint[shard] = nodeID
}

// SetMap installs the committed assignment. Called by the shard supervisor as
// the control group commits new versions.
func (r *ShardRouter) SetMap(m *ShardMap) { r.committed.Store(m) }

// Map returns the assignment currently routed by, or nil before one commits.
func (r *ShardRouter) Map() *ShardMap { return r.committed.Load() }

// shardFor resolves a bucket to its shard. It fails rather than guessing: with
// no committed map there is no correct answer, and answering 0 would send every
// bucket to one group.
func (r *ShardRouter) shardFor(bucket string) (int, *ShardMap, error) {
	m := r.Map()
	if m == nil {
		return 0, nil, fmt.Errorf("%w: no committed shard assignment yet", ErrShardUnavailable)
	}
	return ShardForBucket(bucket, m.Shards), m, nil
}

// Local returns this node's handle on the shard owning bucket. The bool is a
// routing fact only: false means "ask a member", never "there is nothing here".
func (r *ShardRouter) Local(bucket string) (metadata.ShardHandle, bool) {
	shard, m, err := r.shardFor(bucket)
	if err != nil {
		return nil, false
	}
	// The committed map decides, not what happens to be running. A node removed
	// from a shard may still have the group up for the moment it takes to leave
	// the Raft configuration, and serving reads from it would serve a replica
	// that has stopped receiving writes.
	if !m.HoldsShard(r.nodeID, shard) {
		return nil, false
	}
	g, err := r.runtime.Group(shard)
	if err != nil {
		return nil, false
	}
	return g, true
}

// Leading returns the shard groups this node currently leads, which is the slice
// of the keyspace it is responsible for scanning.
func (r *ShardRouter) Leading() []metadata.ShardHandle {
	var out []metadata.ShardHandle
	for _, shard := range r.runtime.Shards() {
		g, err := r.runtime.Group(shard)
		if err != nil {
			continue
		}
		if g.IsLeader() {
			out = append(out, g)
		}
	}
	return out
}

// Call runs a read against a member of the bucket's shard. Every member holds a
// replicated copy, so any of them can answer; they are tried in turn and the
// caller is told the shard is unavailable only when none could.
func (r *ShardRouter) Call(req metadata.ShardRequest) (metadata.ShardResponse, error) {
	shard, m, err := r.shardFor(req.Bucket)
	if err != nil {
		return metadata.ShardResponse{}, err
	}
	members := m.MembersOf(shard)
	if len(members) == 0 {
		return metadata.ShardResponse{}, fmt.Errorf("%w: shard %d has no members", ErrShardUnavailable, shard)
	}
	// A leader-only request goes straight to the leader when this node knows who
	// that is, which it does whenever it holds the shard itself.
	order := members
	if req.LeaderOnly {
		leader := r.hintedLeader(shard)
		if g, err := r.runtime.Group(shard); err == nil && g.LeaderID() != "" {
			leader = g.LeaderID()
		}
		if leader != "" {
			order = append([]string{leader}, members...)
		}
	}
	var lastErr error
	tried := make(map[string]bool, len(order))
	for _, id := range order {
		if tried[id] {
			continue
		}
		tried[id] = true
		if id == r.nodeID {
			continue // already tried locally, or the group is not running here
		}
		addr, ok := r.addrOf(id)
		if !ok {
			lastErr = fmt.Errorf("no address for shard %d member %s", shard, id)
			continue
		}
		resp, err := r.postCall(addr, shard, req)
		if err == nil {
			if req.LeaderOnly {
				r.rememberLeader(shard, id)
			}
			return resp, nil
		}
		lastErr = err
		// A follower asked for a leader-only read names the leader; take the hint
		// once rather than asking every member in turn.
		var redirect *shardLeaderRedirect
		if asShardLeaderRedirect(err, &redirect) && redirect.leader != "" && !tried[redirect.leader] {
			tried[redirect.leader] = true
			r.rememberLeader(shard, redirect.leader)
			if redirect.leader == r.nodeID {
				if g, err := r.runtime.Group(shard); err == nil && g.IsLeader() {
					return metadata.ExecuteShardRequest(g, req), nil
				}
				continue
			}
			if leaderAddr, ok := r.addrOf(redirect.leader); ok {
				resp, err := r.postCall(leaderAddr, shard, req)
				if err == nil {
					return resp, nil
				}
				lastErr = err
			}
		}
	}
	return metadata.ShardResponse{}, fmt.Errorf("%w: shard %d: %v", ErrShardUnavailable, shard, lastErr)
}

// Write commits a metadata command to the group owning the bucket.
//
// A write is ordered by the shard's leader and nowhere else. When this node
// leads the shard it commits directly; when it holds the shard it knows the
// leader and posts there; when it holds nothing it asks a member, which answers
// with the leader if it is not the leader itself.
func (r *ShardRouter) Write(bucket string, data []byte) error {
	shard, m, err := r.shardFor(bucket)
	if err != nil {
		return err
	}
	if g, err := r.runtime.Group(shard); err == nil {
		if g.IsLeader() {
			_, err := g.Apply(data, r.timeout)
			return err
		}
		leader := g.LeaderID()
		if leader == "" {
			return fmt.Errorf("%w: shard %d has no leader", ErrShardUnavailable, shard)
		}
		addr, ok := r.addrOf(leader)
		if !ok {
			return fmt.Errorf("%w: shard %d leader %s has no address", ErrShardUnavailable, shard, leader)
		}
		return r.postApply(addr, shard, bucket, data)
	}
	return r.writeViaMembers(shard, m, bucket, data)
}

// writeViaMembers submits a write from a node that holds no copy of the shard.
// A member that is not the leader names the leader, and the write is retried
// there once; chaining hops on the server side is deliberately avoided so a
// leadership flap cannot make a write loop between two nodes.
func (r *ShardRouter) writeViaMembers(shard int, m *ShardMap, bucket string, data []byte) error {
	members := m.MembersOf(shard)
	if len(members) == 0 {
		return fmt.Errorf("%w: shard %d has no members", ErrShardUnavailable, shard)
	}
	order := members
	if hint := r.hintedLeader(shard); hint != "" {
		order = append([]string{hint}, members...)
	}
	var lastErr error
	tried := make(map[string]bool, len(order))
	for _, id := range order {
		if tried[id] {
			continue
		}
		tried[id] = true
		addr, ok := r.addrOf(id)
		if !ok {
			lastErr = fmt.Errorf("no address for shard %d member %s", shard, id)
			continue
		}
		err := r.postApply(addr, shard, bucket, data)
		if err == nil {
			r.rememberLeader(shard, id)
			return nil
		}
		lastErr = err
		var redirect *shardLeaderRedirect
		if asShardLeaderRedirect(err, &redirect) && redirect.leader != "" && !tried[redirect.leader] {
			tried[redirect.leader] = true
			r.rememberLeader(shard, redirect.leader)
			if leaderAddr, ok := r.addrOf(redirect.leader); ok {
				if err := r.postApply(leaderAddr, shard, bucket, data); err == nil {
					return nil
				} else {
					lastErr = err
				}
			}
		}
	}
	return fmt.Errorf("%w: shard %d write: %v", ErrShardUnavailable, shard, lastErr)
}

// Compile-time check that the cluster router satisfies what the store needs.
var _ metadata.ShardRouter = (*ShardRouter)(nil)
