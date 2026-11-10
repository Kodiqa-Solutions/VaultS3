package cluster

import (
	"testing"
	"time"
)

// A new leader catches up with every write committed before it took over
// before it serves a read or tells a follower how far to catch up. It used
// to skip the barrier, and in the 5.0.15 soak a read straight after the
// leader restarted answered an existing object as missing.
func TestANewLeaderCatchesUpBeforeServingReads(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	old := mustLeader(t, nodes)
	for i := 0; i < 20; i++ {
		if err := createBucketOn(t, old.node, "lc"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.node.Shutdown(); err != nil {
		t.Fatal(err)
	}
	var rest []*testNode
	for _, n := range nodes {
		if n != old {
			rest = append(rest, n)
		}
	}
	leader := mustLeader(t, rest)
	if err := leader.node.ReadBarrier(3 * time.Second); err != nil {
		t.Fatalf("read barrier on the new leader: %v", err)
	}
	if got, want := leader.node.catchUp.ready.Load(), leader.node.raft.CurrentTerm(); got != want {
		t.Fatalf("the new leader did not catch up in its term (ready %d, term %d)", got, want)
	}
	// Raft's own entries (the barrier, the no-op of the election) never reach
	// the FSM, so Raft's applied index is the one to compare.
	if applied, committed := leader.node.raft.AppliedIndex(), leader.node.raft.CommitIndex(); applied < committed {
		t.Errorf("applied %d < committed %d after the barrier", applied, committed)
	}
	for i := 0; i < 20; i++ {
		if !leader.store.BucketExists("lc" + string(rune('a'+i))) {
			t.Errorf("bucket %d written before the failover is missing on the new leader", i)
		}
	}
}
