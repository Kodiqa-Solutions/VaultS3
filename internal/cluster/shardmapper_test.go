package cluster

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeCommitter records what a mapper proposes without needing a Raft cluster.
type fakeCommitter struct {
	leader    bool
	applied   [][]byte
	applyErr  error
	committed *ShardMap
}

func (f *fakeCommitter) IsLeader() bool { return f.leader }

func (f *fakeCommitter) Apply(data []byte) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, data)
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return err
	}
	if cmd.Type != CmdPutShardMap {
		return errors.New("unexpected command type")
	}
	var m ShardMap
	if err := json.Unmarshal(cmd.Data, &m); err != nil {
		return err
	}
	f.committed = &m
	return nil
}

func (f *fakeCommitter) read() (*ShardMap, error) { return f.committed, nil }

func newTestMapper(c *fakeCommitter, ring *HashRing, shards, replicas int, clock *time.Time) *ShardMapper {
	m := NewShardMapper(c, ring, c.read, shards, replicas)
	m.settle = 30 * time.Second
	m.now = func() time.Time { return *clock }
	return m
}

// The founding sets are permanent, so committing one from a cluster that is still
// forming would under-replicate those shards forever. The map must wait for
// membership to hold still.
func TestShardMapperWaitsForMembershipToSettle(t *testing.T) {
	now := time.Unix(1000, 0)
	ring := ringOf("n1", "n2", "n3")
	c := &fakeCommitter{leader: true}
	m := newTestMapper(c, ring, 8, 3, &now)

	if done, err := m.step(); done || err != nil {
		t.Fatalf("first observation should only start the clock, got done=%v err=%v", done, err)
	}
	now = now.Add(29 * time.Second)
	if done, _ := m.step(); done {
		t.Fatal("committed before the settle window elapsed")
	}
	if c.committed != nil {
		t.Fatalf("map committed after %v, want nothing before the settle window", 29*time.Second)
	}

	now = now.Add(2 * time.Second)
	done, err := m.step()
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if !done || c.committed == nil {
		t.Fatal("a settled ring should have produced a committed map")
	}
	if c.committed.Epoch != 1 || c.committed.Version != 1 {
		t.Fatalf("first map should be version 1 epoch 1, got version %d epoch %d",
			c.committed.Version, c.committed.Epoch)
	}
	if len(c.committed.Founders) != 8 {
		t.Fatalf("founding sets recorded for %d shards, want 8", len(c.committed.Founders))
	}
}

// A node joining resets the wait. Otherwise a cluster growing one pod at a time
// would freeze the map at whatever size it happened to be at the deadline.
func TestShardMapperRestartsSettleWhenMembershipChanges(t *testing.T) {
	now := time.Unix(2000, 0)
	ring := ringOf("n1", "n2", "n3")
	c := &fakeCommitter{leader: true}
	m := newTestMapper(c, ring, 4, 3, &now)

	m.step()
	now = now.Add(25 * time.Second)
	ring.AddNode("n4")
	if done, _ := m.step(); done {
		t.Fatal("a membership change should restart the wait, not commit")
	}
	now = now.Add(25 * time.Second)
	if done, _ := m.step(); done {
		t.Fatal("committed 25s after the change, but the window is 30s")
	}
	now = now.Add(6 * time.Second)
	if done, _ := m.step(); !done {
		t.Fatal("expected a commit once the larger membership settled")
	}
	if got := len(c.committed.Members[0]); got != 3 {
		t.Fatalf("shard 0 has %d members, want 3", got)
	}
	// The late-joining node must be able to appear: the point of restarting the
	// wait is that the map reflects the whole cluster.
	seen := map[string]bool{}
	for _, members := range c.committed.Members {
		for _, id := range members {
			seen[id] = true
		}
	}
	if !seen["n4"] {
		t.Fatal("n4 joined before the map was committed but holds no shard")
	}
}

func TestShardMapperOnlyLeaderProposes(t *testing.T) {
	now := time.Unix(3000, 0)
	c := &fakeCommitter{leader: false}
	m := newTestMapper(c, ringOf("n1", "n2", "n3"), 4, 3, &now)
	for i := 0; i < 5; i++ {
		now = now.Add(time.Minute)
		if done, _ := m.step(); done {
			t.Fatal("a follower committed a shard map")
		}
	}
	if len(c.applied) != 0 {
		t.Fatalf("follower proposed %d commands, want 0", len(c.applied))
	}
}

// Losing leadership has to clear the observation window: a node that stops
// leading cannot know what happened while it was not leading, so resuming a
// half-elapsed timer would commit on stale evidence.
func TestShardMapperForgetsProgressWhenLeadershipIsLost(t *testing.T) {
	now := time.Unix(4000, 0)
	c := &fakeCommitter{leader: true}
	m := newTestMapper(c, ringOf("n1", "n2", "n3"), 4, 3, &now)

	m.step()
	now = now.Add(29 * time.Second)
	c.leader = false
	m.step()
	c.leader = true
	now = now.Add(2 * time.Second)
	if done, _ := m.step(); done {
		t.Fatal("committed on a window that elapsed while this node was not leader")
	}
	now = now.Add(31 * time.Second)
	if done, _ := m.step(); !done {
		t.Fatal("expected a commit after a full window under leadership")
	}
}

func TestShardMapperWaitsForEnoughNodes(t *testing.T) {
	now := time.Unix(5000, 0)
	ring := ringOf("n1", "n2")
	c := &fakeCommitter{leader: true}
	m := newTestMapper(c, ring, 4, 3, &now)

	for i := 0; i < 3; i++ {
		now = now.Add(time.Minute)
		if done, _ := m.step(); done {
			t.Fatal("committed a 3-replica map on a 2-node cluster")
		}
	}
	ring.AddNode("n3")
	m.step()
	now = now.Add(31 * time.Second)
	if done, _ := m.step(); !done {
		t.Fatal("expected a commit once the third node arrived and settled")
	}
	if c.committed.Replicas != 3 {
		t.Fatalf("committed %d replicas, want 3", c.committed.Replicas)
	}
}

// Once any node has committed a map the mapper is finished, including on a node
// that never led.
func TestShardMapperStopsWhenAMapAlreadyExists(t *testing.T) {
	now := time.Unix(6000, 0)
	existing, err := BuildShardMap(4, 3, ringOf("a", "b", "c"), 1)
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeCommitter{leader: true, committed: existing}
	m := newTestMapper(c, ringOf("n1", "n2", "n3"), 4, 3, &now)
	done, err := m.step()
	if err != nil || !done {
		t.Fatalf("expected the mapper to stop, got done=%v err=%v", done, err)
	}
	if len(c.applied) != 0 {
		t.Fatal("proposed a map although one was already committed")
	}
}

func TestShardMapperDoesNothingWhenUnsharded(t *testing.T) {
	now := time.Unix(7000, 0)
	c := &fakeCommitter{leader: true}
	m := newTestMapper(c, ringOf("n1", "n2", "n3"), 1, 3, &now)
	// Run returns immediately rather than ticking forever.
	done := make(chan struct{})
	go func() { m.Run(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("mapper kept running on an unsharded cluster")
	}
	if len(c.applied) != 0 {
		t.Fatal("proposed a shard map on an unsharded cluster")
	}
}
