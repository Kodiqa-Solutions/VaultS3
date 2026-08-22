package cluster

import (
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// ClusterConfig is an alias for config.ClusterConfig.
type ClusterConfig = config.ClusterConfig

// ApplyDefaults fills in the cluster defaults on a config the caller keeps, so
// the server and the node agree on values like the metadata replica count
// instead of each defaulting its own copy.
func ApplyDefaults(c *ClusterConfig) { applyDefaults(c) }

func applyDefaults(c *ClusterConfig) {
	if c.BindAddr == "" {
		c.BindAddr = "0.0.0.0"
	}
	if c.RaftPort == 0 {
		c.RaftPort = 9001
	}
	if c.DataDir == "" {
		c.DataDir = "./raft-data"
	}
	if c.SnapshotCount == 0 {
		c.SnapshotCount = 8192
	}
	// Unsharded by default: one Raft group holding all metadata on every node.
	if c.MetadataShards < 1 {
		c.MetadataShards = 1
	}
	if c.MetadataReplicas < 1 {
		c.MetadataReplicas = 3
	}
}

const (
	raftTimeout       = 10 * time.Second
	leaderWaitTimeout = 10 * time.Second
)

// MaxMetadataShards bounds the metadata shard count. Every shard is a complete
// Raft group with its own log, snapshots, metadata file and election timers, and
// a node holds one group per shard it is a member of, so the ceiling is what a
// node can run rather than what the wire format could address (issue #50).
const MaxMetadataShards = 256
