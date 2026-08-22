package cluster

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

var (
	// ErrShardUnavailable means this node cannot answer for the shard: the group
	// is not running here, or it has no leader. It is deliberately distinct from
	// "the shard holds nothing". Reporting absence when the truth is "I could not
	// ask" is what makes a reconciler delete live data, so every path that can
	// fail this way must fail loudly.
	//
	// It is the SAME error value the metadata package exposes, not a copy: the
	// store returns it, the S3 layer turns it into a 503 rather than a 404, and
	// two look-alike sentinels would let one of those checks quietly miss.
	ErrShardUnavailable = metadata.ErrShardUnavailable
	// ErrNotShardLeader means the group is running here but another member leads
	// it, so the write has to go there.
	ErrNotShardLeader = errors.New("cluster: not the leader of this metadata shard")
)

// ShardGroup is one metadata shard: an independent Raft group with its own log,
// snapshots and metadata store, sharing this node's Raft port through the mux.
type ShardGroup struct {
	shard int
	raft  *raft.Raft
	fsm   *FSM
	store *metadata.Store
	trans *raft.NetworkTransport

	closeOnce sync.Once
}

// Shard returns which shard this group serves.
func (g *ShardGroup) Shard() int { return g.shard }

// Store is the shard's metadata store. Only the shard's own buckets live here.
func (g *ShardGroup) Store() *metadata.Store { return g.store }

// IsLeader reports whether this node leads the shard.
func (g *ShardGroup) IsLeader() bool { return g.raft.State() == raft.Leader }

// LeaderID returns the shard's current leader, or "" while there is none.
func (g *ShardGroup) LeaderID() string {
	_, id := g.raft.LeaderWithID()
	return string(id)
}

// Apply commits a command to this shard's log. Leader-only: a follower's caller
// is told who to talk to instead, because a metadata write must be ordered by
// the group that owns it.
func (g *ShardGroup) Apply(data []byte, timeout time.Duration) (uint64, error) {
	if g.raft.State() != raft.Leader {
		return 0, ErrNotShardLeader
	}
	future := g.raft.Apply(data, timeout)
	if err := future.Error(); err != nil {
		return 0, fmt.Errorf("shard %d apply: %w", g.shard, err)
	}
	if resp := future.Response(); resp != nil {
		if err, ok := resp.(error); ok {
			return 0, err
		}
	}
	return future.Index(), nil
}

// AddVoter adds a member to this shard's group. Leader-only.
func (g *ShardGroup) AddVoter(nodeID, addr string, timeout time.Duration) error {
	if g.raft.State() != raft.Leader {
		return ErrNotShardLeader
	}
	return g.raft.AddVoter(raft.ServerID(nodeID), raft.ServerAddress(addr), 0, timeout).Error()
}

// RemoveServer removes a member from this shard's group. Leader-only.
func (g *ShardGroup) RemoveServer(nodeID string, timeout time.Duration) error {
	if g.raft.State() != raft.Leader {
		return ErrNotShardLeader
	}
	return g.raft.RemoveServer(raft.ServerID(nodeID), 0, timeout).Error()
}

// Members returns the shard group's current Raft configuration.
func (g *ShardGroup) Members() ([]raft.Server, error) {
	future := g.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return nil, err
	}
	return future.Configuration().Servers, nil
}

// Close shuts the group down. The shared listener is untouched: it belongs to
// the mux, and other groups are still using it.
func (g *ShardGroup) Close() error {
	var err error
	g.closeOnce.Do(func() {
		if e := g.raft.Shutdown().Error(); e != nil {
			err = e
		}
		if g.trans != nil {
			g.trans.Close()
		}
		if g.store != nil {
			if e := g.store.Close(); e != nil && err == nil {
				err = e
			}
		}
	})
	return err
}

// shardGroupDeps are the pluggable Raft backends for one shard group, mirroring
// the control group's. Tests inject in-memory stores while keeping the real
// transport, so the mux is exercised over real sockets.
type shardGroupDeps struct {
	logStore  raft.LogStore
	stable    raft.StableStore
	snapshots raft.SnapshotStore
}

// ShardRuntime owns the metadata shard groups running on this node.
type ShardRuntime struct {
	nodeID   string
	dataDir  string
	mux      *TransportMux
	provider raft.ServerAddressProvider
	timeout  time.Duration

	mu     sync.RWMutex
	groups map[int]*ShardGroup
}

// NewShardRuntime creates the runtime. provider resolves a member id to its
// current address; it must follow the live cluster rather than whatever address
// was recorded when a member joined, because addresses are pod IPs and change on
// every restart.
func NewShardRuntime(nodeID, dataDir string, mux *TransportMux, provider raft.ServerAddressProvider) *ShardRuntime {
	return &ShardRuntime{
		nodeID:   nodeID,
		dataDir:  dataDir,
		mux:      mux,
		provider: provider,
		timeout:  raftTimeout,
		groups:   make(map[int]*ShardGroup),
	}
}

// Start brings up the Raft group for one shard, per the committed map.
//
// Bootstrapping is gated on the founding set. A node that joined after creation
// must be added to the group that exists, never start one of its own: Raft's
// bootstrap check looks only at local state, and pre-vote makes an established
// group invisible to a server outside its configuration, so a second bootstrap
// would produce a rival group that answers, authoritatively, that the shard is
// empty.
func (rt *ShardRuntime) Start(shard int, m *ShardMap) (*ShardGroup, error) {
	if m == nil {
		return nil, fmt.Errorf("cluster: cannot start shard %d without a committed map", shard)
	}
	if !m.HoldsShard(rt.nodeID, shard) {
		return nil, fmt.Errorf("cluster: this node is not a member of shard %d", shard)
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if g, ok := rt.groups[shard]; ok {
		return g, nil
	}

	dir := filepath.Join(rt.dataDir, fmt.Sprintf("shard-%d", shard))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("cluster: create shard %d dir: %w", shard, err)
	}
	logStore, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(dir, "raft-log.db")})
	if err != nil {
		return nil, fmt.Errorf("cluster: shard %d log store: %w", shard, err)
	}
	snapshots, err := raft.NewFileSnapshotStore(dir, 3, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("cluster: shard %d snapshot store: %w", shard, err)
	}
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		return nil, fmt.Errorf("cluster: shard %d metadata store: %w", shard, err)
	}

	g, err := rt.startLocked(shard, m, store, shardGroupDeps{logStore: logStore, stable: logStore, snapshots: snapshots})
	if err != nil {
		store.Close()
		return nil, err
	}
	return g, nil
}

// startLocked builds the group from explicit dependencies. rt.mu must be held.
func (rt *ShardRuntime) startLocked(shard int, m *ShardMap, store *metadata.Store, deps shardGroupDeps) (*ShardGroup, error) {
	layer, err := rt.mux.ShardLayer(shard)
	if err != nil {
		return nil, err
	}
	trans := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream:                layer,
		MaxPool:               3,
		Timeout:               rt.timeout,
		Logger:                nil,
		ServerAddressProvider: rt.provider,
	})

	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(rt.nodeID)
	cfg.LogLevel = "WARN"

	fsm := NewShardFSM(store)
	r, err := raft.NewRaft(cfg, fsm, deps.logStore, deps.stable, deps.snapshots, trans)
	if err != nil {
		trans.Close()
		return nil, fmt.Errorf("cluster: shard %d raft: %w", shard, err)
	}

	// Only a founder bootstraps, and only when the group has no state yet.
	if m.IsFounder(shard, rt.nodeID) {
		hasState, err := raft.HasExistingState(deps.logStore, deps.stable, deps.snapshots)
		if err != nil {
			r.Shutdown()
			trans.Close()
			return nil, fmt.Errorf("cluster: shard %d state check: %w", shard, err)
		}
		if !hasState {
			servers := make([]raft.Server, 0, len(m.Founders[shard]))
			for _, id := range m.Founders[shard] {
				addr, err := rt.provider.ServerAddr(raft.ServerID(id))
				if err != nil {
					r.Shutdown()
					trans.Close()
					return nil, fmt.Errorf("cluster: shard %d: address of founder %s: %w", shard, id, err)
				}
				servers = append(servers, raft.Server{ID: raft.ServerID(id), Address: addr})
			}
			// Every founder bootstraps the SAME configuration, taken from the
			// committed map, so the group they form is one group.
			if err := r.BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil &&
				!errors.Is(err, raft.ErrCantBootstrap) {
				r.Shutdown()
				trans.Close()
				return nil, fmt.Errorf("cluster: shard %d bootstrap: %w", shard, err)
			}
			slog.Info("cluster: metadata shard group bootstrapped",
				"shard", shard, "node_id", rt.nodeID, "founders", m.Founders[shard])
		}
	}

	g := &ShardGroup{shard: shard, raft: r, fsm: fsm, store: store, trans: trans}
	rt.groups[shard] = g
	return g, nil
}

// Group returns the running group for a shard, or ErrShardUnavailable when this
// node does not serve it.
func (rt *ShardRuntime) Group(shard int) (*ShardGroup, error) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	g, ok := rt.groups[shard]
	if !ok {
		return nil, fmt.Errorf("%w: shard %d is not running on node %s", ErrShardUnavailable, shard, rt.nodeID)
	}
	return g, nil
}

// ApplyToShard commits a command to a shard's group. It never silently succeeds
// elsewhere: a caller that is not on the shard's leader gets ErrNotShardLeader
// and must route the write to the leader.
func (rt *ShardRuntime) ApplyToShard(shard int, data []byte) (uint64, error) {
	g, err := rt.Group(shard)
	if err != nil {
		return 0, err
	}
	return g.Apply(data, rt.timeout)
}

// Shards lists the shards running on this node, for diagnostics.
func (rt *ShardRuntime) Shards() []int {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	out := make([]int, 0, len(rt.groups))
	for s := range rt.groups {
		out = append(out, s)
	}
	return out
}

// Close stops every group. The mux is left alone: its owner closes it.
func (rt *ShardRuntime) Close() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var firstErr error
	for shard, g := range rt.groups {
		if err := g.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("shard %d: %w", shard, err)
		}
		delete(rt.groups, shard)
	}
	return firstErr
}

// Stop shuts one shard group down and forgets it, so a node that has been
// removed from a shard stops holding a replica of metadata it no longer serves.
// It is not an error to stop a shard that is not running.
func (rt *ShardRuntime) Stop(shard int) error {
	rt.mu.Lock()
	g, ok := rt.groups[shard]
	delete(rt.groups, shard)
	rt.mu.Unlock()
	if !ok {
		return nil
	}
	return g.Close()
}
