package cluster

import (
	"testing"
	"time"
)

// Regression (5.0.15 review): raft.Barrier's timeout bounds only the enqueue, so the leader's first read
// of a term waits for commit (or for the leader to step down), not timeout.
func TestALeadersFirstReadKeepsItsTimeout(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	ld := mustLeader(t, nodes)
	ld.node.catchUp.ready.Store(0) // first read of the term
	ld.trans.DisconnectAll()
	for _, n := range nodes {
		if n != ld {
			n.trans.DisconnectAll()
		}
	}
	start := time.Now()
	err := ld.node.ReadBarrier(50 * time.Millisecond)
	took := time.Since(start)
	t.Logf("ReadBarrier(50ms) took %s, err=%v", took, err)
	if took > 300*time.Millisecond {
		t.Fatalf("leader read barrier blocked %s with a 50ms timeout", took)
	}
}
