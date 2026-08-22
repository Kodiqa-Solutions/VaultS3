package cluster

import (
	"fmt"
	"sort"

	"github.com/cespare/xxhash/v2"
)

// A metadata shard is an independent Raft group holding the object metadata of
// the buckets assigned to it. Splitting metadata this way is what lets a cluster
// hold more objects than one node can index: with N shards of R members each, a
// node stores roughly N/nodes * R shards instead of every object in the cluster
// (issue #50).
//
// Bucket configuration, IAM and the shard map itself stay in the control group,
// which still spans every node, so any node can authorize and route a request
// without a hop.

// ShardMap is the cluster's committed assignment of shards to nodes. It is
// decided once by the control group and replicated, never recomputed
// independently by a node: every member of a shard must bootstrap that shard's
// Raft group from the SAME member list, or two members could form two groups
// with the same shard id and silently diverge.
type ShardMap struct {
	// Version increments on every committed change, so a node can tell a stale
	// map from a current one.
	Version uint64 `json:"version"`
	// Epoch identifies the assignment's creation. It is written once, with the
	// first committed map, and never changes: it is how a node tells "the shards
	// I already belong to" from "a different set of shards that happens to reuse
	// the same ids".
	Epoch uint64 `json:"epoch"`
	// Shards is fixed for the life of the cluster. Buckets hash to a shard, so
	// changing this number moves buckets between groups and requires migration.
	Shards int `json:"shards"`
	// Replicas is the number of nodes holding each shard.
	Replicas int `json:"replicas"`
	// Members[i] holds the node IDs for shard i, in a stable order. Membership
	// changes as nodes come and go, so this is the only part of a committed map
	// that a later version may alter.
	Members [][]string `json:"members"`
	// Founders[i] is Members[i] as it stood at the creation epoch, frozen.
	//
	// Only a founder may bootstrap a shard's Raft group. Raft's bootstrap check
	// looks at local state alone, and pre-vote makes an established group
	// invisible to a server that is not in its configuration, so a node that
	// joined later and bootstrapped on its own would form a rival, EMPTY group
	// for a shard that already exists. Since metadata is authoritative, that
	// group would then answer, authoritatively, that the shard holds nothing.
	// Every node added after creation must be joined to the existing group
	// instead, which is what the membership reconciler does.
	Founders [][]string `json:"founders"`
}

// shardRingKey is the ring key a shard is placed by. It is deliberately not a
// real bucket name (bucket names cannot contain underscores), so a shard can
// never collide with a bucket's own placement.
func shardRingKey(shard int) string {
	return fmt.Sprintf("__vs3_shard_%d", shard)
}

// ShardForBucket maps a bucket to its shard. Hashing the bucket name means the
// assignment is stable for the life of the bucket and identical on every node,
// with no lookup and nothing to replicate.
//
// A shard count of 0 or 1 means the cluster is unsharded: everything lives in
// the control group, which is the default and the pre-sharding behaviour.
func ShardForBucket(bucket string, shards int) int {
	if shards <= 1 {
		return 0
	}
	return int(xxhash.Sum64String(bucket) % uint64(shards))
}

// BuildShardMap assigns members to every shard from the current ring. Each shard
// is placed independently by consistent hashing, so shards spread evenly and a
// node joining or leaving moves only the shards it actually gains or loses.
//
// Replicas is clamped to the number of nodes available: a three-node cluster asked
// for five replicas gets three, which is every node, rather than an error that
// would block startup.
func BuildShardMap(shards, replicas int, ring *HashRing, version uint64) (*ShardMap, error) {
	if shards < 1 {
		return nil, fmt.Errorf("cluster: shard count must be at least 1, got %d", shards)
	}
	if replicas < 1 {
		return nil, fmt.Errorf("cluster: replica count must be at least 1, got %d", replicas)
	}
	if ring == nil || ring.NodeCount() == 0 {
		return nil, fmt.Errorf("cluster: cannot build a shard map with no nodes")
	}
	if replicas > ring.NodeCount() {
		replicas = ring.NodeCount()
	}

	m := &ShardMap{
		Version:  version,
		Epoch:    version,
		Shards:   shards,
		Replicas: replicas,
		Members:  make([][]string, shards),
		Founders: make([][]string, shards),
	}
	for i := 0; i < shards; i++ {
		members := ring.GetNodes(shardRingKey(i), "", replicas)
		if len(members) < replicas {
			return nil, fmt.Errorf("cluster: shard %d got %d of %d members from a %d-node ring",
				i, len(members), replicas, ring.NodeCount())
		}
		m.Members[i] = members
		m.Founders[i] = append([]string(nil), members...)
	}
	return m, nil
}

// WithMembers returns the map with a new membership assignment at a new version,
// carrying the epoch and the founding sets through unchanged. Membership is the
// only thing a later version may change, and this is the only way to change it.
func (m *ShardMap) WithMembers(members [][]string, version uint64) *ShardMap {
	if m == nil {
		return nil
	}
	next := &ShardMap{
		Version:  version,
		Epoch:    m.Epoch,
		Shards:   m.Shards,
		Replicas: m.Replicas,
		Members:  make([][]string, len(members)),
		Founders: make([][]string, len(m.Founders)),
	}
	for i := range members {
		next.Members[i] = append([]string(nil), members[i]...)
	}
	for i := range m.Founders {
		next.Founders[i] = append([]string(nil), m.Founders[i]...)
	}
	return next
}

// IsFounder reports whether a node may bootstrap a shard's Raft group. Any node
// that is a member but not a founder must be joined to the group that already
// exists, never start one of its own. See ShardMap.Founders.
func (m *ShardMap) IsFounder(shard int, nodeID string) bool {
	if m == nil || shard < 0 || shard >= len(m.Founders) {
		return false
	}
	for _, id := range m.Founders[shard] {
		if id == nodeID {
			return true
		}
	}
	return false
}

// ValidateSuccession reports whether next may replace prev. It is enforced where
// the map is applied, so the decision is identical on every node: a map that
// changed the shard count, the epoch or a founding set would make members of the
// same shard disagree about which Raft group they belong to, which is how two
// groups end up serving one shard.
//
// A nil prev means this is the first map, which only has to be internally valid.
func ValidateSuccession(prev, next *ShardMap) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if prev == nil {
		return nil
	}
	if next.Version <= prev.Version {
		return fmt.Errorf("cluster: shard map version %d does not advance the committed version %d",
			next.Version, prev.Version)
	}
	if next.Epoch != prev.Epoch {
		return fmt.Errorf("cluster: shard map epoch cannot change (committed %d, proposed %d)",
			prev.Epoch, next.Epoch)
	}
	if next.Shards != prev.Shards {
		return fmt.Errorf("cluster: shard count cannot change (committed %d, proposed %d): "+
			"buckets hash to a shard, so a different count would move metadata between groups",
			prev.Shards, next.Shards)
	}
	if len(next.Founders) != len(prev.Founders) {
		return fmt.Errorf("cluster: shard map lists founders for %d shards, committed map has %d",
			len(next.Founders), len(prev.Founders))
	}
	for i := range prev.Founders {
		if len(next.Founders[i]) != len(prev.Founders[i]) {
			return fmt.Errorf("cluster: founding set of shard %d cannot change", i)
		}
		for j := range prev.Founders[i] {
			if next.Founders[i][j] != prev.Founders[i][j] {
				return fmt.Errorf("cluster: founding set of shard %d cannot change", i)
			}
		}
	}
	return nil
}

// Validate reports whether the map is self-consistent. A map that fails this must
// never be acted on: starting Raft groups from a malformed assignment is how a
// shard ends up with no members or with two disjoint groups.
func (m *ShardMap) Validate() error {
	if m == nil {
		return fmt.Errorf("cluster: nil shard map")
	}
	if m.Shards < 1 {
		return fmt.Errorf("cluster: shard map has %d shards", m.Shards)
	}
	if len(m.Members) != m.Shards {
		return fmt.Errorf("cluster: shard map declares %d shards but lists %d", m.Shards, len(m.Members))
	}
	if m.Epoch == 0 {
		return fmt.Errorf("cluster: shard map has no epoch")
	}
	if len(m.Founders) != m.Shards {
		return fmt.Errorf("cluster: shard map declares %d shards but lists founders for %d", m.Shards, len(m.Founders))
	}
	for i, founders := range m.Founders {
		if len(founders) == 0 {
			return fmt.Errorf("cluster: shard %d has no founding members", i)
		}
	}
	for i, members := range m.Members {
		if len(members) == 0 {
			return fmt.Errorf("cluster: shard %d has no members", i)
		}
		seen := make(map[string]bool, len(members))
		for _, id := range members {
			if id == "" {
				return fmt.Errorf("cluster: shard %d has an empty member id", i)
			}
			if seen[id] {
				return fmt.Errorf("cluster: shard %d lists node %q twice", i, id)
			}
			seen[id] = true
		}
	}
	return nil
}

// MembersOf returns the nodes holding shard i, or nil if the shard is unknown.
func (m *ShardMap) MembersOf(shard int) []string {
	if m == nil || shard < 0 || shard >= len(m.Members) {
		return nil
	}
	return m.Members[shard]
}

// MembersForBucket returns the nodes holding a bucket's object metadata.
func (m *ShardMap) MembersForBucket(bucket string) []string {
	if m == nil {
		return nil
	}
	return m.MembersOf(ShardForBucket(bucket, m.Shards))
}

// HoldsBucket reports whether a node can answer for a bucket's object metadata
// locally. This is the routing question every request asks: a node that does not
// hold the bucket must send the request to one that does.
func (m *ShardMap) HoldsBucket(nodeID, bucket string) bool {
	for _, id := range m.MembersForBucket(bucket) {
		if id == nodeID {
			return true
		}
	}
	return false
}

// HoldsShard reports whether a node is a member of a shard, which is what
// decides whether it runs that shard's Raft group at all.
func (m *ShardMap) HoldsShard(nodeID string, shard int) bool {
	for _, id := range m.MembersOf(shard) {
		if id == nodeID {
			return true
		}
	}
	return false
}

// ShardsFor returns the shards a node must start a Raft group for, in ascending
// order.
func (m *ShardMap) ShardsFor(nodeID string) []int {
	if m == nil {
		return nil
	}
	var out []int
	for i, members := range m.Members {
		for _, id := range members {
			if id == nodeID {
				out = append(out, i)
				break
			}
		}
	}
	sort.Ints(out)
	return out
}

// Equal reports whether two maps assign identically, ignoring the version. Used
// to decide whether a newly computed assignment is worth committing.
func (m *ShardMap) Equal(other *ShardMap) bool {
	if m == nil || other == nil {
		return m == other
	}
	if m.Shards != other.Shards || m.Replicas != other.Replicas || len(m.Members) != len(other.Members) {
		return false
	}
	for i := range m.Members {
		if len(m.Members[i]) != len(other.Members[i]) {
			return false
		}
		for j := range m.Members[i] {
			if m.Members[i][j] != other.Members[i][j] {
				return false
			}
		}
	}
	return true
}
