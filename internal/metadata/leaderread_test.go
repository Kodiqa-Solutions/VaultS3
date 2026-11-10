package metadata

import (
	"path/filepath"
	"testing"
	"time"
)

type leaderRaftFake struct{ barriers int }

func (r *leaderRaftFake) Apply([]byte) error              { return nil }
func (r *leaderRaftFake) IsLeader() bool                  { return true }
func (r *leaderRaftFake) ForwardToLeader([]byte) error    { return nil }
func (r *leaderRaftFake) ReadBarrier(time.Duration) error { r.barriers++; return nil }

// Regression (5.0.15 review): a freshly elected leader must catch up (leaderCaughtUp) before it serves a
// consistent read of its own; DistributedStore skips ReadBarrier on the leader.
func TestTheLeaderCatchesUpBeforeAConsistentRead(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	r := &leaderRaftFake{}
	d := NewDistributedStore(s, r)
	_, _ = d.GetObjectMetaConsistent("b", "k")
	if r.barriers == 0 {
		t.Fatalf("a consistent read on the leader did not catch up first")
	}
	before := r.barriers
	_ = d.ReadBarrier()
	if r.barriers == before {
		t.Fatalf("leader served GetObjectMetaConsistent/ReadBarrier without calling node.ReadBarrier (leaderCaughtUp never runs for leader-local reads)")
	}
}
