package cluster

import (
	"fmt"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// driveShards runs the supervisor and the reconciler on every node until check
// passes, which is what the shard service does on its ticker in a real cluster.
func driveShards(t *testing.T, nodes []*testShardNode, m *ShardMap, what string, check func() bool) {
	t.Helper()
	sups := make([]*shardSupervisor, len(nodes))
	recs := make([]*shardReconciler, len(nodes))
	for i, n := range nodes {
		router := NewShardRouter(n.id, n.rt, func(string) (string, bool) { return "", false }, "")
		sups[i] = &shardSupervisor{nodeID: n.id, runtime: n.rt, router: router}
		recs[i] = &shardReconciler{nodeID: n.id, runtime: n.rt, provider: n.provider, timeout: 5 * time.Second}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for i := range nodes {
			sups[i].step(m)
			recs[i].step(m)
		}
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func groupMemberIDs(t *testing.T, n *testShardNode, shard int) []string {
	t.Helper()
	g, err := n.rt.Group(shard)
	if err != nil {
		return nil
	}
	servers, err := g.Members()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(servers))
	for _, s := range servers {
		ids = append(ids, string(s.ID))
	}
	return ids
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// P4: a node added to a shard's committed membership must be joined to the group
// that exists and must receive the metadata already in it. Without this, removing
// or adding a node updates the control group only, and every shard configuration
// keeps its original members forever.
func TestShardMembershipReconcilesToTheCommittedMap(t *testing.T) {
	m, err := BuildShardMap(1, 3, ringOf("node-0", "node-1", "node-2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	nodes := newShardCluster(t, 4, m) // node-3 exists but holds nothing yet
	leader := shardLeader(t, nodes, 0)

	cmd, err := marshalCommand(CmdPutObjectMeta, metadata.ObjectMeta{Bucket: "b", Key: "before-join"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leader.rt.ApplyToShard(0, cmd); err != nil {
		t.Fatalf("seed the shard: %v", err)
	}

	var newcomer *testShardNode
	for _, n := range nodes {
		if n.id == "node-3" {
			newcomer = n
		}
	}
	if _, err := newcomer.rt.Group(0); err == nil {
		t.Fatal("node-3 is running shard 0 before it was ever assigned to it")
	}

	grown := m.WithMembers([][]string{{"node-0", "node-1", "node-2", "node-3"}}, 2)
	driveShards(t, nodes, grown, "node-3 to join shard 0 and catch up", func() bool {
		g, err := newcomer.rt.Group(0)
		if err != nil {
			return false
		}
		meta, err := g.Store().GetObjectMeta("b", "before-join")
		return err == nil && meta != nil
	})

	if ids := groupMemberIDs(t, leader, 0); !contains(ids, "node-3") {
		t.Fatalf("shard 0 configuration is %v, node-3 never joined", ids)
	}

	// And the reverse: a node dropped from the committed membership must be
	// removed from the group and stop holding a replica of the shard.
	shrunk := grown.WithMembers([][]string{{"node-1", "node-2", "node-3"}}, 3)
	var departing *testShardNode
	for _, n := range nodes {
		if n.id == "node-0" {
			departing = n
		}
	}
	driveShards(t, nodes, shrunk, "node-0 to leave shard 0", func() bool {
		if _, err := departing.rt.Group(0); err == nil {
			return false // still running here
		}
		for _, n := range nodes {
			if n.id == "node-0" {
				continue
			}
			if ids := groupMemberIDs(t, n, 0); len(ids) > 0 && contains(ids, "node-0") {
				return false
			}
		}
		return true
	})
}

// The planner proposes membership and nothing else. A proposal that changed the
// epoch, the shard count or a founding set would make members of one shard
// disagree about which Raft group they belong to.
func TestPlannerProposesMembershipOnly(t *testing.T) {
	now := time.Unix(2000, 0)
	ring := ringOf("n1", "n2", "n3")
	committed, err := BuildShardMap(4, 2, ring, 1)
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeCommitter{leader: true, committed: committed}
	p := &shardPlanner{committer: c, ring: ring, replicas: 2, settle: 30 * time.Second,
		now: func() time.Time { return now }}

	// A fourth node arrives and the ring moves some shards to it.
	ring.AddNode("n4")
	if err := p.step(committed); err != nil {
		t.Fatal(err)
	}
	if c.committed.Version != committed.Version {
		t.Fatal("the planner committed before the ring had settled")
	}
	now = now.Add(31 * time.Second)
	if err := p.step(committed); err != nil {
		t.Fatal(err)
	}
	next := c.committed
	if next.Version <= committed.Version {
		t.Fatalf("no new version committed: %d", next.Version)
	}
	if next.Epoch != committed.Epoch {
		t.Fatalf("epoch changed from %d to %d", committed.Epoch, next.Epoch)
	}
	if next.Shards != committed.Shards {
		t.Fatalf("shard count changed from %d to %d", committed.Shards, next.Shards)
	}
	for i := range committed.Founders {
		if fmt.Sprint(next.Founders[i]) != fmt.Sprint(committed.Founders[i]) {
			t.Fatalf("founding set of shard %d changed from %v to %v", i, committed.Founders[i], next.Founders[i])
		}
	}
	if err := ValidateSuccession(committed, next); err != nil {
		t.Fatalf("the planner proposed a map the state machine would refuse: %v", err)
	}
}

// Handing a shard to a set of nodes that share no member with the current one
// does not move the metadata, it abandons it: the new group starts empty and
// answers authoritatively that the shard holds nothing.
func TestPlannerRefusesAReassignmentThatKeepsNoCurrentMember(t *testing.T) {
	now := time.Unix(3000, 0)
	ring := ringOf("n1", "n2", "n3")
	committed, err := BuildShardMap(2, 1, ring, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the whole cluster: every original node leaves, three new ones arrive.
	for _, id := range []string{"n1", "n2", "n3"} {
		ring.RemoveNode(id)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		ring.AddNode(id)
	}
	c := &fakeCommitter{leader: true, committed: committed}
	p := &shardPlanner{committer: c, ring: ring, replicas: 1, settle: 0,
		now: func() time.Time { return now }}

	if err := p.step(committed); err != nil {
		t.Fatal(err)
	}
	if err := p.step(committed); err != nil {
		t.Fatal(err)
	}
	if c.committed.Version != committed.Version {
		t.Fatalf("the planner committed a reassignment that keeps no current member: %+v", c.committed.Members)
	}
}
