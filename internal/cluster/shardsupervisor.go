package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/hashicorp/raft"
)

// Keeping the shards running (issue #50).
//
// Three jobs, each small and each with one owner:
//
//   - the planner, on the control-group leader, decides which nodes SHOULD hold
//     each shard and commits that to the control group;
//   - the supervisor, on every node, starts the shard groups this node is listed
//     for and stops the ones it is not;
//   - the reconciler, on each shard's own leader, drives that shard's Raft
//     configuration towards the committed list, one member at a time.
//
// The split matters because only the shard's leader can change the shard's Raft
// configuration, while only the control group can decide what the configuration
// should be. Without the reconciler a node removed from the cluster would stay a
// voter in every shard group forever, and each shard would lose quorum as soon as
// enough departed nodes accumulated.

// shardPlanner recomputes shard membership from the ring and commits it.
//
// It changes membership only. The shard count, the epoch and the founding sets
// are fixed at creation and the state machine refuses any proposal that touches
// them, so a planner running against a stale map cannot rewrite history.
type shardPlanner struct {
	committer shardMapCommitter
	ring      *HashRing
	replicas  int

	// settle is how long ring membership must hold still before a membership
	// change is proposed. Reacting instantly would rewrite every shard during a
	// rolling restart, and each rewrite makes the shard leaders move data.
	settle time.Duration

	lastMembers []string
	stableSince time.Time
	now         func() time.Time
}

func (p *shardPlanner) step(committed *ShardMap) error {
	if committed == nil {
		return nil
	}
	if !p.committer.IsLeader() {
		p.lastMembers = nil
		p.stableSince = time.Time{}
		return nil
	}
	members := p.ring.Nodes()
	sort.Strings(members)
	if len(members) == 0 {
		return nil
	}
	if !sameMembers(members, p.lastMembers) {
		p.lastMembers = members
		p.stableSince = p.now()
		return nil
	}
	if p.now().Sub(p.stableSince) < p.settle {
		return nil
	}

	desired, changed := p.plan(committed)
	if !changed {
		return nil
	}
	next := committed.WithMembers(desired, committed.Version+1)
	data, err := marshalCommand(CmdPutShardMap, next)
	if err != nil {
		return fmt.Errorf("encode shard map command: %w", err)
	}
	if err := p.committer.Apply(data); err != nil {
		return fmt.Errorf("commit shard membership: %w", err)
	}
	slog.Info("cluster: metadata shard membership updated",
		"version", next.Version, "nodes", len(members))
	return nil
}

// plan computes the membership the ring implies, and reports whether it differs
// from what is committed.
//
// A shard whose new set shares NO member with the committed one is left alone.
// That case means handing the shard to a group of nodes none of which holds a
// copy of its metadata, which does not move the data, it abandons it. It happens
// when the ring changes faster than the reconciler can add members, and the
// right response is to wait for the reconciler rather than to commit the loss.
func (p *shardPlanner) plan(committed *ShardMap) ([][]string, bool) {
	replicas := p.replicas
	if n := p.ring.NodeCount(); replicas > n {
		replicas = n
	}
	desired := make([][]string, committed.Shards)
	changed := false
	for i := 0; i < committed.Shards; i++ {
		current := committed.MembersOf(i)
		next := p.ring.GetNodes(shardRingKey(i), "", replicas)
		if len(next) == 0 || !overlaps(current, next) {
			if len(next) > 0 {
				slog.Warn("cluster: refusing a metadata shard reassignment that would drop every current member",
					"shard", i, "current", current, "proposed", next)
			}
			desired[i] = append([]string(nil), current...)
			continue
		}
		desired[i] = next
		if !sameMembers(sortedCopy(current), sortedCopy(next)) {
			changed = true
		}
	}
	return desired, changed
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// shardSupervisor keeps the groups running on this node in step with the map.
type shardSupervisor struct {
	nodeID  string
	runtime *ShardRuntime
	router  *ShardRouter
}

func (s *shardSupervisor) step(m *ShardMap) {
	if m == nil {
		return
	}
	// Install the map before starting anything: routing by a map this node has
	// not adopted would send writes to a group it is about to stop serving.
	s.router.SetMap(m)

	mine := m.ShardsFor(s.nodeID)
	// Register every stream layer this node will serve BEFORE starting any of
	// the groups. A peer that started its group first dials immediately, and a
	// connection for a shard with no layer yet is closed as unknown: harmless,
	// because Raft retries, but it makes a cluster start slowly and fills the log
	// with warnings that read like a fault.
	if s.runtime.mux != nil {
		for _, shard := range mine {
			s.runtime.mux.ShardLayer(shard)
		}
	}

	for _, shard := range mine {
		if _, err := s.runtime.Group(shard); err == nil {
			continue
		}
		if _, err := s.runtime.Start(shard, m); err != nil {
			slog.Warn("cluster: could not start a metadata shard group",
				"shard", shard, "error", err)
		}
	}

	for _, shard := range s.runtime.Shards() {
		if m.HoldsShard(s.nodeID, shard) {
			continue
		}
		// Only stop once the shard's own leader has actually removed this node.
		// Stopping while still a voter would take a vote out of the group without
		// the group knowing, which is how a three-member shard loses quorum.
		g, err := s.runtime.Group(shard)
		if err != nil {
			continue
		}
		servers, err := g.Members()
		if err != nil {
			continue
		}
		stillAVoter := false
		for _, srv := range servers {
			if string(srv.ID) == s.nodeID {
				stillAVoter = true
				break
			}
		}
		if stillAVoter {
			continue
		}
		if err := s.runtime.Stop(shard); err != nil {
			slog.Warn("cluster: could not stop a metadata shard group", "shard", shard, "error", err)
			continue
		}
		slog.Info("cluster: stopped a metadata shard group this node no longer holds", "shard", shard)
	}
}

// shardReconciler drives each shard this node LEADS towards its committed
// membership, one change per round.
//
// One at a time, and adds before removes: a group that gains its replacement
// before losing a member never drops below the quorum of members that hold the
// data. Doing both at once, or several at once, is how a membership change turns
// into an outage.
type shardReconciler struct {
	nodeID   string
	runtime  *ShardRuntime
	provider raft.ServerAddressProvider
	timeout  time.Duration
}

func (rc *shardReconciler) step(m *ShardMap) {
	if m == nil {
		return
	}
	for _, shard := range rc.runtime.Shards() {
		g, err := rc.runtime.Group(shard)
		if err != nil || !g.IsLeader() {
			continue
		}
		want := m.MembersOf(shard)
		if len(want) == 0 {
			continue
		}
		have, err := g.Members()
		if err != nil {
			continue
		}
		present := make(map[string]bool, len(have))
		for _, srv := range have {
			present[string(srv.ID)] = true
		}

		added := false
		for _, id := range want {
			if present[id] {
				continue
			}
			addr, err := rc.provider.ServerAddr(raft.ServerID(id))
			if err != nil {
				slog.Warn("cluster: cannot add a member to a metadata shard yet",
					"shard", shard, "node_id", id, "error", err)
				break
			}
			if err := g.AddVoter(id, string(addr), rc.timeout); err != nil {
				slog.Warn("cluster: adding a member to a metadata shard failed",
					"shard", shard, "node_id", id, "error", err)
			} else {
				slog.Info("cluster: added a member to a metadata shard", "shard", shard, "node_id", id)
			}
			added = true
			break
		}
		if added {
			continue // one change per shard per round
		}

		// Every wanted member is in the configuration, so shrinking to the wanted
		// set can only remove nodes whose replicas are surplus.
		if len(have) <= 1 {
			continue
		}
		wanted := make(map[string]bool, len(want))
		for _, id := range want {
			wanted[id] = true
		}
		extra := ""
		for _, srv := range have {
			id := string(srv.ID)
			if wanted[id] {
				continue
			}
			// Prefer removing another node first: removing the leader costs an
			// election, so it is done last, when it is the only surplus left.
			if id == rc.nodeID && extra == "" {
				extra = id
				continue
			}
			if id != rc.nodeID {
				extra = id
				break
			}
		}
		if extra == "" {
			continue
		}
		if err := g.RemoveServer(extra, rc.timeout); err != nil {
			slog.Warn("cluster: removing a member from a metadata shard failed",
				"shard", shard, "node_id", extra, "error", err)
			continue
		}
		slog.Info("cluster: removed a member from a metadata shard", "shard", shard, "node_id", extra)
	}
}

// ShardService is the metadata-shard control loop: one ticker driving creation,
// membership planning, group supervision and per-shard reconciliation.
type ShardService struct {
	read     func() (*ShardMap, error)
	mapper   *ShardMapper
	planner  *shardPlanner
	sup      *shardSupervisor
	rec      *shardReconciler
	shards   int
	interval time.Duration
}

// NewShardService wires the loop. shards <= 1 means the cluster is unsharded and
// the service does nothing.
func NewShardService(nodeID string, committer shardMapCommitter, read func() (*ShardMap, error),
	ring *HashRing, runtime *ShardRuntime, router *ShardRouter,
	provider raft.ServerAddressProvider, shards, replicas int) *ShardService {
	return &ShardService{
		read:   read,
		mapper: NewShardMapper(committer, ring, read, shards, replicas),
		planner: &shardPlanner{
			committer: committer,
			ring:      ring,
			replicas:  replicas,
			settle:    defaultShardMapSettle,
			now:       time.Now,
		},
		sup: &shardSupervisor{nodeID: nodeID, runtime: runtime, router: router},
		rec: &shardReconciler{
			nodeID:   nodeID,
			runtime:  runtime,
			provider: provider,
			timeout:  raftTimeout,
		},
		shards:   shards,
		interval: defaultShardMapInterval,
	}
}

// Run drives the loop until ctx is cancelled.
func (s *ShardService) Run(ctx context.Context) {
	if s.shards <= 1 {
		return
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.step()
		}
	}
}

func (s *ShardService) step() {
	committed, err := s.read()
	if err != nil {
		slog.Warn("cluster: cannot read the committed metadata shard map", "error", err)
		return
	}
	if committed == nil {
		if _, err := s.mapper.stepWith(nil); err != nil {
			slog.Warn("cluster: shard map not committed yet", "error", err)
		}
		return
	}
	s.sup.step(committed)
	s.rec.step(committed)
	if err := s.planner.step(committed); err != nil {
		slog.Warn("cluster: shard membership not updated", "error", err)
	}
}
