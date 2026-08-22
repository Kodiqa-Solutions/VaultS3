package cluster

import (
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
)

type testShardNode struct {
	id       string
	mux      *TransportMux
	rt       *ShardRuntime
	provider raft.ServerAddressProvider
}

// newShardCluster starts n nodes, each serving every group from one port, and
// brings up the shards of m on the nodes that hold them.
func newShardCluster(t *testing.T, n int, m *ShardMap) []*testShardNode {
	t.Helper()

	nodes := make([]*testShardNode, n)
	addrs := make(map[string]string, n)
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		id := fmt.Sprintf("node-%d", i)
		mux := NewTransportMux(ln)
		t.Cleanup(func() { mux.Close() })
		nodes[i] = &testShardNode{id: id, mux: mux}
		addrs[id] = ln.Addr().String()
	}

	provider := &staticAddressProvider{addrs: addrs}
	for i, n := range nodes {
		n.provider = provider
		n.rt = NewShardRuntime(n.id, filepath.Join(t.TempDir(), fmt.Sprintf("n%d", i)), n.mux, provider)
		t.Cleanup(func() { n.rt.Close() })
		for _, shard := range m.ShardsFor(n.id) {
			if _, err := n.rt.Start(shard, m); err != nil {
				t.Fatalf("start shard %d on %s: %v", shard, n.id, err)
			}
		}
	}
	return nodes
}

// shardLeader waits for one shard to elect a leader and returns that node.
func shardLeader(t *testing.T, nodes []*testShardNode, shard int) *testShardNode {
	t.Helper()
	var found *testShardNode
	eventually(t, 20*time.Second, fmt.Sprintf("shard %d elects a leader", shard), func() bool {
		found = nil
		for _, n := range nodes {
			g, err := n.rt.Group(shard)
			if err == nil && g.IsLeader() {
				found = n
				return true
			}
		}
		return false
	})
	return found
}

// The point of P2: several Raft groups, on one port per node, that do not see
// each other's traffic. A write committed in one shard must be invisible to the
// other, or sharding would just be one replicated store with extra steps.
func TestShardGroupsAreIndependentOverOnePort(t *testing.T) {
	m, err := BuildShardMap(2, 3, ringOf("node-0", "node-1", "node-2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	nodes := newShardCluster(t, 3, m)

	zeroLeader := shardLeader(t, nodes, 0)
	oneLeader := shardLeader(t, nodes, 1)

	cmd, err := marshalCommand(CmdPutObjectMeta, metadata.ObjectMeta{Bucket: "b", Key: "in-shard-zero"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zeroLeader.rt.ApplyToShard(0, cmd); err != nil {
		t.Fatalf("apply to shard 0: %v", err)
	}

	// Every member of shard 0 converges on the write.
	for _, n := range nodes {
		n := n
		eventually(t, 15*time.Second, fmt.Sprintf("%s applies the shard 0 write", n.id), func() bool {
			g, err := n.rt.Group(0)
			if err != nil {
				return false
			}
			meta, err := g.Store().GetObjectMeta("b", "in-shard-zero")
			return err == nil && meta != nil
		})
	}

	// No member of shard 1 ever sees it, although the bytes crossed the same port.
	time.Sleep(500 * time.Millisecond)
	for _, n := range nodes {
		g, err := n.rt.Group(1)
		if err != nil {
			t.Fatalf("shard 1 not running on %s: %v", n.id, err)
		}
		if meta, _ := g.Store().GetObjectMeta("b", "in-shard-zero"); meta != nil {
			t.Fatalf("%s: a shard 0 write leaked into shard 1", n.id)
		}
	}
	_ = oneLeader

	// The groups lead independently, which is the throughput argument for
	// sharding: each shard orders its own writes.
	cmd, _ = marshalCommand(CmdPutObjectMeta, metadata.ObjectMeta{Bucket: "b", Key: "in-shard-one"})
	if _, err := oneLeader.rt.ApplyToShard(1, cmd); err != nil {
		t.Fatalf("apply to shard 1: %v", err)
	}
	eventually(t, 15*time.Second, "shard 1 commits its own write", func() bool {
		g, _ := oneLeader.rt.Group(1)
		meta, err := g.Store().GetObjectMeta("b", "in-shard-one")
		return err == nil && meta != nil
	})
	if g, _ := zeroLeader.rt.Group(0); g != nil {
		if meta, _ := g.Store().GetObjectMeta("b", "in-shard-one"); meta != nil {
			t.Fatal("a shard 1 write leaked into shard 0")
		}
	}
}

// A write sent to a follower must be refused, not applied locally. Applying it
// would create metadata that the group's log never ordered.
func TestApplyToShardRefusesOnAFollower(t *testing.T) {
	m, err := BuildShardMap(1, 3, ringOf("node-0", "node-1", "node-2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	nodes := newShardCluster(t, 3, m)
	leader := shardLeader(t, nodes, 0)

	cmd, _ := marshalCommand(CmdPutObjectMeta, metadata.ObjectMeta{Bucket: "b", Key: "follower-write"})
	for _, n := range nodes {
		if n == leader {
			continue
		}
		if _, err := n.rt.ApplyToShard(0, cmd); err != ErrNotShardLeader {
			t.Fatalf("%s: follower apply returned %v, want ErrNotShardLeader", n.id, err)
		}
		g, _ := n.rt.Group(0)
		if meta, _ := g.Store().GetObjectMeta("b", "follower-write"); meta != nil {
			t.Fatalf("%s: a refused write was applied locally anyway", n.id)
		}
		// A follower learns who leads a moment after the election, so wait for it
		// rather than race it. Knowing the leader is what lets P3 forward the
		// write instead of failing it.
		n := n
		eventually(t, 10*time.Second, fmt.Sprintf("%s learns the shard leader", n.id), func() bool {
			g, err := n.rt.Group(0)
			return err == nil && g.LeaderID() == leader.id
		})
	}
}

// "I do not serve this shard" must be a loud, distinct error. Answering as if
// the shard were empty is what makes callers treat live metadata as absent.
func TestUnservedShardIsUnavailableNotEmpty(t *testing.T) {
	m, err := BuildShardMap(4, 1, ringOf("node-0", "node-1", "node-2", "node-3"), 1)
	if err != nil {
		t.Fatal(err)
	}
	nodes := newShardCluster(t, 4, m)

	for _, n := range nodes {
		held := map[int]bool{}
		for _, s := range m.ShardsFor(n.id) {
			held[s] = true
		}
		for shard := 0; shard < m.Shards; shard++ {
			_, err := n.rt.Group(shard)
			if held[shard] {
				if err != nil {
					t.Fatalf("%s should serve shard %d: %v", n.id, shard, err)
				}
				continue
			}
			if err == nil {
				t.Fatalf("%s reported serving shard %d, which it does not hold", n.id, shard)
			}
			if _, applyErr := n.rt.ApplyToShard(shard, []byte("x")); applyErr == nil {
				t.Fatalf("%s accepted a write for shard %d, which it does not hold", n.id, shard)
			}
		}
	}
}

// A node that is not in a shard's member list must refuse to start the group at
// all, rather than quietly forming a rival one.
func TestStartRefusesAShardThisNodeDoesNotHold(t *testing.T) {
	m, err := BuildShardMap(4, 1, ringOf("node-0", "node-1", "node-2", "node-3"), 1)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := NewTransportMux(ln)
	defer mux.Close()

	rt := NewShardRuntime("stranger", t.TempDir(), mux, &staticAddressProvider{addrs: map[string]string{}})
	defer rt.Close()
	for shard := 0; shard < m.Shards; shard++ {
		if _, err := rt.Start(shard, m); err == nil {
			t.Fatalf("a non-member started shard %d", shard)
		}
	}
	if _, err := rt.Start(0, nil); err == nil {
		t.Fatal("a shard was started with no committed map")
	}
}

// A member that is not a founder must never bootstrap. It waits to be added to
// the group the founders formed, because a second bootstrap makes a rival group
// that pre-vote hides from the real one.
func TestNonFounderDoesNotBootstrap(t *testing.T) {
	m, err := BuildShardMap(1, 2, ringOf("node-0", "node-1"), 1)
	if err != nil {
		t.Fatal(err)
	}
	// A third node joins the shard later: a member, but not a founder.
	late := "node-2"
	m = m.WithMembers([][]string{append(append([]string{}, m.Members[0]...), late)}, 2)
	if m.IsFounder(0, late) {
		t.Fatal("test setup: the late node must not be a founder")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := NewTransportMux(ln)
	defer mux.Close()
	rt := NewShardRuntime(late, t.TempDir(), mux,
		&staticAddressProvider{addrs: map[string]string{late: ln.Addr().String()}})
	defer rt.Close()

	g, err := rt.Start(0, m)
	if err != nil {
		t.Fatalf("a member should still start its group: %v", err)
	}
	// With no bootstrap there is no configuration, so it cannot elect itself and
	// cannot answer for the shard.
	time.Sleep(2 * time.Second)
	if g.IsLeader() {
		t.Fatal("a non-founder elected itself leader of a shard, forming a rival group")
	}
	if _, err := g.Apply([]byte("x"), time.Second); err != ErrNotShardLeader {
		t.Fatalf("non-founder apply returned %v, want ErrNotShardLeader", err)
	}
	members, err := g.Members()
	if err == nil && len(members) > 0 {
		t.Fatalf("a non-founder created a configuration of its own: %v", members)
	}
}
