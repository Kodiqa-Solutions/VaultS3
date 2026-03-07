package cluster

import (
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// ClusterConfig is an alias for config.ClusterConfig.
type ClusterConfig = config.ClusterConfig

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
}

const (
	raftTimeout       = 10 * time.Second
	leaderWaitTimeout = 10 * time.Second
)
