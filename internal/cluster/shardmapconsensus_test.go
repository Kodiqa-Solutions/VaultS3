package cluster

import (
	"strings"
	"testing"
	"time"
)

// Every member of a shard must bootstrap that shard's Raft group from the SAME
// member list, so the map cannot be a local decision. This runs the mapper on a
// real three-node cluster and checks that what the leader decided is what every
// node ends up holding, byte for byte.
func TestShardMapReplicatesToEveryNode(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	ld := mustLeader(t, nodes)

	ring := ringOf("node-0", "node-1", "node-2")
	clock := time.Unix(10_000, 0)
	m := NewShardMapper(ld.node, ring, ld.node.CommittedShardMap, 8, 3)
	m.now = func() time.Time { return clock }

	if done, err := m.step(); done || err != nil {
		t.Fatalf("first step should start the settle window, got done=%v err=%v", done, err)
	}
	clock = clock.Add(m.settle + time.Second)
	done, err := m.step()
	if err != nil {
		t.Fatalf("commit shard map: %v", err)
	}
	if !done {
		t.Fatal("mapper did not commit on a settled three-node cluster")
	}

	leaderMap, err := ld.node.CommittedShardMap()
	if err != nil || leaderMap == nil {
		t.Fatalf("leader has no committed map: %v", err)
	}
	for _, n := range nodes {
		n := n
		eventually(t, 10*time.Second, "every node applies the shard map", func() bool {
			got, err := n.node.CommittedShardMap()
			return err == nil && got != nil
		})
		got, _ := n.node.CommittedShardMap()
		if !got.Equal(leaderMap) {
			t.Fatalf("node %s holds a different assignment: %+v vs %+v", n.addr, got.Members, leaderMap.Members)
		}
		if got.Epoch != leaderMap.Epoch || got.Version != leaderMap.Version {
			t.Fatalf("node %s: version/epoch %d/%d, leader %d/%d",
				n.addr, got.Version, got.Epoch, leaderMap.Version, leaderMap.Epoch)
		}
		for i := range leaderMap.Founders {
			if strings.Join(got.Founders[i], ",") != strings.Join(leaderMap.Founders[i], ",") {
				t.Fatalf("node %s disagrees about who founded shard %d: %v vs %v",
					n.addr, i, got.Founders[i], leaderMap.Founders[i])
			}
		}
	}

	// Every node must also agree on where a given bucket's metadata lives,
	// which is the routing question every request will ask.
	for _, bucket := range []string{"tenant-a", "tenant-b", "logs", "backups"} {
		want := strings.Join(leaderMap.MembersForBucket(bucket), ",")
		for _, n := range nodes {
			got, _ := n.node.CommittedShardMap()
			if have := strings.Join(got.MembersForBucket(bucket), ","); have != want {
				t.Fatalf("node %s routes %q to %s, leader routes it to %s", n.addr, bucket, have, want)
			}
		}
	}
}

// A second proposal must be refused by the state machine on every node, not just
// by the proposer, or a node that missed the refusal would diverge.
func TestSecondShardMapIsRefusedClusterWide(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	ld := mustLeader(t, nodes)

	first, err := BuildShardMap(4, 3, ringOf("node-0", "node-1", "node-2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := marshalCommand(CmdPutShardMap, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := ld.node.Apply(cmd); err != nil {
		t.Fatalf("commit the first map: %v", err)
	}

	// A rival assignment: same cluster, different creation. Accepting it would
	// leave two groups believing they own the same shard.
	rival, err := BuildShardMap(4, 3, ringOf("node-0", "node-1", "node-2"), 2)
	if err != nil {
		t.Fatal(err)
	}
	rival.Members[0] = []string{"node-2", "node-1", "node-0"}
	cmd, _ = marshalCommand(CmdPutShardMap, rival)
	if err := ld.node.Apply(cmd); err == nil {
		t.Fatal("a rival map with a new epoch was accepted")
	}

	for _, n := range nodes {
		eventually(t, 10*time.Second, "every node holds the first map", func() bool {
			got, err := n.node.CommittedShardMap()
			return err == nil && got != nil
		})
		got, _ := n.node.CommittedShardMap()
		if got.Epoch != first.Epoch || !got.Equal(first) {
			t.Fatalf("node %s drifted to the rejected assignment: %+v", n.addr, got)
		}
	}
}
