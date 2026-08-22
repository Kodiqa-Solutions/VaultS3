package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// ShardMapper decides the cluster's metadata shard assignment once, on the
// leader, and commits it through Raft so every node acts on the same one
// (issue #50).
//
// The first commit is permanent in the parts that matter: the shard count fixes
// which bucket lives in which group, and the founding sets fix who may bootstrap
// each group. So the map is NOT written the moment a leader exists. A cluster
// comes up one node at a time, and a map computed from a half-formed cluster
// would freeze founders that exclude nodes about to arrive, leaving those shards
// under-replicated for the life of the cluster with no way to correct the record.
// The mapper therefore waits for membership to stop changing first.
type ShardMapper struct {
	committer shardMapCommitter
	ring      *HashRing
	read      func() (*ShardMap, error)

	shards   int
	replicas int

	// settle is how long ring membership must hold still before the first map is
	// committed, and interval is how often that is checked.
	settle   time.Duration
	interval time.Duration

	lastMembers []string
	stableSince time.Time
	lastWarn    time.Time

	now func() time.Time
}

// shardMapCommitter is the part of Node the mapper uses. Narrow on purpose: the
// settle logic is the part worth testing, and it should not need a Raft cluster
// to exercise.
type shardMapCommitter interface {
	IsLeader() bool
	Apply(data []byte) error
}

const (
	defaultShardMapSettle   = 30 * time.Second
	defaultShardMapInterval = 2 * time.Second
)

// NewShardMapper builds the mapper. shards <= 1 means the cluster is unsharded
// and the mapper does nothing at all.
func NewShardMapper(committer shardMapCommitter, ring *HashRing, read func() (*ShardMap, error), shards, replicas int) *ShardMapper {
	return &ShardMapper{
		committer: committer,
		ring:      ring,
		read:      read,
		shards:    shards,
		replicas:  replicas,
		settle:    defaultShardMapSettle,
		interval:  defaultShardMapInterval,
		now:       time.Now,
	}
}

// Run watches the cluster until a shard map is committed, then stops. Membership
// changes after that are the reconciler's job, not the mapper's: this decides the
// assignment once and never revisits it.
func (m *ShardMapper) Run(ctx context.Context) {
	if m.shards <= 1 {
		return
	}
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			done, err := m.step()
			if err != nil {
				slog.Warn("cluster: shard map not committed yet", "error", err)
			}
			if done {
				return
			}
		}
	}
}

// step advances the state machine once. It reports done when a map is committed
// (by this node or any other), which is when the mapper's job is finished.
func (m *ShardMapper) step() (bool, error) {
	committed, err := m.read()
	if err != nil {
		return false, fmt.Errorf("read committed shard map: %w", err)
	}
	return m.stepWith(committed)
}

// stepWith is step for a caller that has already read the committed map, so the
// supervisor does not read it twice per tick.
func (m *ShardMapper) stepWith(committed *ShardMap) (bool, error) {
	if committed != nil {
		return true, nil
	}
	// Only the leader proposes. A follower that lost leadership must forget what
	// it had observed, because it cannot know what happened while it was not
	// leading.
	if !m.committer.IsLeader() {
		m.lastMembers = nil
		m.stableSince = time.Time{}
		return false, nil
	}

	members := m.ring.Nodes()
	sort.Strings(members)
	if len(members) < m.replicas {
		m.lastMembers = nil
		m.stableSince = time.Time{}
		m.warn("cluster: waiting for enough nodes to create the metadata shard map",
			"have", len(members), "need", m.replicas)
		return false, nil
	}
	if !sameMembers(members, m.lastMembers) {
		m.lastMembers = members
		m.stableSince = m.now()
		return false, nil
	}
	if m.now().Sub(m.stableSince) < m.settle {
		return false, nil
	}

	// Version 1 is also the epoch: this is the assignment's creation.
	next, err := BuildShardMap(m.shards, m.replicas, m.ring, 1)
	if err != nil {
		return false, fmt.Errorf("build shard map: %w", err)
	}
	data, err := marshalCommand(CmdPutShardMap, next)
	if err != nil {
		return false, fmt.Errorf("encode shard map command: %w", err)
	}
	if err := m.committer.Apply(data); err != nil {
		// Losing leadership mid-proposal is ordinary. The next tick re-reads the
		// committed map and either finds one or starts the wait again.
		return false, fmt.Errorf("commit shard map: %w", err)
	}
	slog.Info("cluster: metadata shard map created",
		"shards", next.Shards, "replicas", next.Replicas, "epoch", next.Epoch,
		"nodes", len(members))
	return true, nil
}

// warn rate-limits a repeating condition to once a minute, so a cluster that
// never reaches its replica count says so without filling the log.
func (m *ShardMapper) warn(msg string, args ...any) {
	if now := m.now(); now.Sub(m.lastWarn) >= time.Minute {
		m.lastWarn = now
		slog.Warn(msg, args...)
	}
}

func sameMembers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CommittedShardMap returns the shard assignment this node has applied, or nil
// when the cluster has not committed one. Every node can answer from its local
// store: the map is control-group state, so it is replicated everywhere.
func (n *Node) CommittedShardMap() (*ShardMap, error) {
	raw, err := n.store.GetShardMap()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var m ShardMap
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("cluster: decode committed shard map: %w", err)
	}
	return &m, nil
}
