package cluster

import (
	"fmt"
	"testing"
)

func ringOf(nodes ...string) *HashRing {
	r := NewHashRing(defaultVnodes)
	for _, n := range nodes {
		r.AddNode(n)
	}
	return r
}

// A bucket must land on the same shard on every node, forever. If this ever
// changes, every object in that bucket becomes unreachable, so the mapping is
// pinned rather than merely "consistent".
func TestShardForBucketIsStableAndSpreads(t *testing.T) {
	const shards = 64
	counts := make([]int, shards)
	for i := 0; i < 10000; i++ {
		b := fmt.Sprintf("customer-%d", i)
		s := ShardForBucket(b, shards)
		if s < 0 || s >= shards {
			t.Fatalf("bucket %q mapped to shard %d, out of range", b, s)
		}
		if again := ShardForBucket(b, shards); again != s {
			t.Fatalf("bucket %q mapped to %d then %d", b, s, again)
		}
		counts[s]++
	}
	// 10000 buckets over 64 shards averages 156. A wide tolerance still catches a
	// hash that collapses onto a few shards.
	for i, c := range counts {
		if c < 80 || c > 260 {
			t.Fatalf("shard %d holds %d of 10000 buckets, distribution is not usable", i, c)
		}
	}
}

// An unsharded cluster is the default and must behave as if sharding did not
// exist, i.e. everything in shard 0.
func TestShardForBucketUnsharded(t *testing.T) {
	for _, shards := range []int{0, 1} {
		if got := ShardForBucket("anything", shards); got != 0 {
			t.Fatalf("with %d shards, bucket mapped to %d, want 0", shards, got)
		}
	}
}

func TestBuildShardMapPlacesEveryShard(t *testing.T) {
	ring := ringOf("a", "b", "c", "d", "e")
	m, err := BuildShardMap(16, 3, ring, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("built an invalid map: %v", err)
	}
	for i := 0; i < 16; i++ {
		if got := len(m.MembersOf(i)); got != 3 {
			t.Fatalf("shard %d has %d members, want 3", i, got)
		}
	}
}

// Rebuilding from the same ring must produce the same assignment, or a node
// restart would start Raft groups that disagree with its peers.
func TestBuildShardMapIsDeterministic(t *testing.T) {
	a, err := BuildShardMap(32, 3, ringOf("n1", "n2", "n3", "n4"), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Same nodes, added in a different order.
	b, err := BuildShardMap(32, 3, ringOf("n4", "n2", "n1", "n3"), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Equal(b) {
		t.Fatal("the same node set produced different shard assignments")
	}
}

// The point of the feature: a node must hold only a fraction of the cluster's
// shards, otherwise nothing has been gained over replicating everything.
func TestShardMapSpreadsLoadAcrossNodes(t *testing.T) {
	nodes := make([]string, 12)
	for i := range nodes {
		nodes[i] = fmt.Sprintf("node-%02d", i)
	}
	const shards = 96
	m, err := BuildShardMap(shards, 3, ringOf(nodes...), 1)
	if err != nil {
		t.Fatal(err)
	}
	// 96 shards * 3 replicas / 12 nodes = 24 shards per node on average.
	for _, n := range nodes {
		held := len(m.ShardsFor(n))
		if held == 0 {
			t.Fatalf("node %s holds no shards", n)
		}
		if held > shards/2 {
			t.Fatalf("node %s holds %d of %d shards, sharding has not reduced its share", n, held, shards)
		}
	}
	// Every shard is accounted for exactly Replicas times.
	total := 0
	for _, n := range nodes {
		total += len(m.ShardsFor(n))
	}
	if total != shards*3 {
		t.Fatalf("shard placements total %d, want %d", total, shards*3)
	}
}

// Asking for more replicas than there are nodes must degrade to "every node"
// rather than refusing to start.
func TestBuildShardMapClampsReplicasToClusterSize(t *testing.T) {
	m, err := BuildShardMap(4, 5, ringOf("only-a", "only-b"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.Replicas != 2 {
		t.Fatalf("replicas = %d, want 2 (clamped to the node count)", m.Replicas)
	}
	for i := 0; i < 4; i++ {
		if got := len(m.MembersOf(i)); got != 2 {
			t.Fatalf("shard %d has %d members, want 2", i, got)
		}
	}
}

func TestBuildShardMapRejectsUnusableInput(t *testing.T) {
	cases := []struct {
		name             string
		shards, replicas int
		ring             *HashRing
	}{
		{"no nodes", 4, 3, NewHashRing(defaultVnodes)},
		{"nil ring", 4, 3, nil},
		{"zero shards", 0, 3, ringOf("a")},
		{"zero replicas", 4, 0, ringOf("a")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildShardMap(tc.shards, tc.replicas, tc.ring, 1); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestShardMapRoutingQuestions(t *testing.T) {
	ring := ringOf("a", "b", "c", "d")
	m, err := BuildShardMap(8, 2, ring, 1)
	if err != nil {
		t.Fatal(err)
	}
	const bucket = "customer-42"
	members := m.MembersForBucket(bucket)
	if len(members) != 2 {
		t.Fatalf("bucket has %d members, want 2", len(members))
	}
	for _, id := range members {
		if !m.HoldsBucket(id, bucket) {
			t.Fatalf("member %s does not report holding the bucket", id)
		}
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		holds := m.HoldsBucket(id, bucket)
		isMember := false
		for _, mem := range members {
			if mem == id {
				isMember = true
			}
		}
		if holds != isMember {
			t.Fatalf("node %s: HoldsBucket=%v but membership=%v", id, holds, isMember)
		}
	}
	if m.HoldsBucket("not-in-cluster", bucket) {
		t.Fatal("an unknown node reported holding the bucket")
	}
}

func TestShardMapValidateCatchesMalformedMaps(t *testing.T) {
	cases := map[string]*ShardMap{
		"nil":              nil,
		"no shards":        {Shards: 0},
		"count mismatch":   {Shards: 2, Members: [][]string{{"a"}}},
		"empty shard":      {Shards: 1, Members: [][]string{{}}},
		"empty member id":  {Shards: 1, Members: [][]string{{""}}},
		"duplicate member": {Shards: 1, Members: [][]string{{"a", "a"}}},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := m.Validate(); err == nil {
				t.Fatal("expected Validate to reject this map")
			}
		})
	}
}

// A shard must never be placed by the same ring key as a real bucket, or a
// bucket named like a shard would share its placement.
func TestShardRingKeysCannotCollideWithBucketNames(t *testing.T) {
	// S3 bucket names cannot contain underscores, so this prefix is unreachable.
	for i := 0; i < 4; i++ {
		k := shardRingKey(i)
		if k == "" {
			t.Fatal("empty shard ring key")
		}
		for _, c := range k {
			if c == '_' {
				return // contains a character no bucket name may hold
			}
		}
		t.Fatalf("shard ring key %q contains no character that bucket names forbid", k)
	}
}
