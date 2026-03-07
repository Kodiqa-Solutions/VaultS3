package cluster

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

// Node represents a single node in the Raft cluster.
type Node struct {
	cfg   ClusterConfig
	raft  *raft.Raft
	fsm   *FSM
	store *metadata.Store
}

// NewNode creates and starts a Raft node.
func NewNode(cfg ClusterConfig, metaStore *metadata.Store) (*Node, error) {
	applyDefaults(&cfg)

	if cfg.NodeID == "" {
		return nil, fmt.Errorf("cluster: node_id is required")
	}

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("cluster: create data dir: %w", err)
	}

	// Raft configuration
	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)
	raftCfg.SnapshotThreshold = uint64(cfg.SnapshotCount)
	raftCfg.LogLevel = "WARN"

	// Transport
	bindAddr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.RaftPort)
	tcpAddr, err := net.ResolveTCPAddr("tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("cluster: resolve bind addr: %w", err)
	}
	transport, err := raft.NewTCPTransport(bindAddr, tcpAddr, 3, raftTimeout, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("cluster: create transport: %w", err)
	}

	// Log store and stable store (BoltDB)
	logStore, err := raftboltdb.New(raftboltdb.Options{
		Path: filepath.Join(cfg.DataDir, "raft-log.db"),
	})
	if err != nil {
		return nil, fmt.Errorf("cluster: create log store: %w", err)
	}

	// Snapshot store
	snapshotStore, err := raft.NewFileSnapshotStore(cfg.DataDir, 3, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("cluster: create snapshot store: %w", err)
	}

	// FSM
	fsm := NewFSM(metaStore)

	// Create Raft instance
	r, err := raft.NewRaft(raftCfg, fsm, logStore, logStore, snapshotStore, transport)
	if err != nil {
		return nil, fmt.Errorf("cluster: create raft: %w", err)
	}

	node := &Node{
		cfg:   cfg,
		raft:  r,
		fsm:   fsm,
		store: metaStore,
	}

	// Bootstrap if this is the first node
	if cfg.Bootstrap {
		servers := []raft.Server{
			{
				ID:      raft.ServerID(cfg.NodeID),
				Address: transport.LocalAddr(),
			},
		}
		future := r.BootstrapCluster(raft.Configuration{Servers: servers})
		if err := future.Error(); err != nil {
			// ErrCantBootstrap means already bootstrapped — not an error
			if err != raft.ErrCantBootstrap {
				return nil, fmt.Errorf("cluster: bootstrap: %w", err)
			}
		}
		slog.Info("cluster: bootstrapped", "node_id", cfg.NodeID, "addr", bindAddr)
	}

	// Join peers if configured
	for _, peer := range cfg.Peers {
		nodeID, addr, ok := ParsePeer(peer)
		if !ok {
			slog.Warn("cluster: invalid peer format, expected nodeID@host:port", "peer", peer)
			continue
		}
		// Only leader can add voters — non-leaders will forward via the leader
		if r.State() == raft.Leader {
			future := r.AddVoter(raft.ServerID(nodeID), raft.ServerAddress(addr), 0, raftTimeout)
			if err := future.Error(); err != nil {
				slog.Warn("cluster: failed to add peer", "peer", peer, "error", err)
			}
		}
	}

	slog.Info("cluster: node started", "node_id", cfg.NodeID, "bind", bindAddr, "peers", len(cfg.Peers))
	return node, nil
}

// Apply submits a command to the Raft log. Must be called on the leader.
func (n *Node) Apply(data []byte) error {
	if n.raft.State() != raft.Leader {
		return ErrNotLeader
	}
	future := n.raft.Apply(data, raftTimeout)
	if err := future.Error(); err != nil {
		return fmt.Errorf("raft apply: %w", err)
	}
	// Check if the FSM returned an error
	if resp := future.Response(); resp != nil {
		if err, ok := resp.(error); ok {
			return err
		}
	}
	return nil
}

// IsLeader returns true if this node is the current Raft leader.
func (n *Node) IsLeader() bool {
	return n.raft.State() == raft.Leader
}

// LeaderAddr returns the address of the current leader.
func (n *Node) LeaderAddr() string {
	addr, _ := n.raft.LeaderWithID()
	return string(addr)
}

// LeaderID returns the node ID of the current leader.
func (n *Node) LeaderID() string {
	_, id := n.raft.LeaderWithID()
	return string(id)
}

// WaitForLeader blocks until a leader is elected or timeout.
func (n *Node) WaitForLeader() error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(leaderWaitTimeout)

	for {
		select {
		case <-ticker.C:
			if leader := n.LeaderAddr(); leader != "" {
				return nil
			}
		case <-timeout:
			return fmt.Errorf("cluster: timed out waiting for leader election")
		}
	}
}

// Join adds a voter to the cluster. Must be called on the leader.
func (n *Node) Join(nodeID, addr string) error {
	if n.raft.State() != raft.Leader {
		return ErrNotLeader
	}
	future := n.raft.AddVoter(raft.ServerID(nodeID), raft.ServerAddress(addr), 0, raftTimeout)
	return future.Error()
}

// Leave removes a voter from the cluster. Must be called on the leader.
func (n *Node) Leave(nodeID string) error {
	if n.raft.State() != raft.Leader {
		return ErrNotLeader
	}
	future := n.raft.RemoveServer(raft.ServerID(nodeID), 0, raftTimeout)
	return future.Error()
}

// Shutdown gracefully shuts down the Raft node.
func (n *Node) Shutdown() error {
	return n.raft.Shutdown().Error()
}

// Stats returns Raft stats.
func (n *Node) Stats() map[string]string {
	return n.raft.Stats()
}

// NodeID returns the node's ID.
func (n *Node) NodeID() string {
	return n.cfg.NodeID
}

// Servers returns the current cluster member list.
func (n *Node) Servers() ([]raft.Server, error) {
	future := n.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return nil, err
	}
	return future.Configuration().Servers, nil
}

// ParsePeer splits "nodeID@host:port" into nodeID and host:port.
func ParsePeer(peer string) (nodeID, addr string, ok bool) {
	parts := strings.SplitN(peer, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ErrNotLeader is returned when a write is attempted on a non-leader node.
var ErrNotLeader = fmt.Errorf("not cluster leader")
