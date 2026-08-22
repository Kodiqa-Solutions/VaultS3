package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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
	// mux owns the Raft port and hands connections to the group they name. Nil
	// in tests that inject an in-memory transport.
	mux *TransportMux
}

// TransportMux is the demultiplexer serving this node's Raft port, or nil when
// the node was built with an injected transport. Metadata shard groups take
// their stream layers from it.
func (n *Node) TransportMux() *TransportMux { return n.mux }

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

	// Transport. The Raft *server ID* is the stable NodeID (the StatefulSet pod
	// name in Kubernetes); the *address* is the current pod IP (set via BindAddr).
	// hashicorp/raft requires a concrete *net.TCPAddr advertise, so we resolve it —
	// and AutoJoin re-announces the current address on every boot, so a restart
	// with a new pod IP self-heals.
	bindAddr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.RaftPort)
	tcpAddr, err := net.ResolveTCPAddr("tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("cluster: resolve bind addr: %w", err)
	}
	// Raft records the advertised address in the cluster configuration, so a
	// wildcard bind has to be rejected here rather than handed to peers.
	if tcpAddr.IP == nil || tcpAddr.IP.IsUnspecified() {
		return nil, fmt.Errorf("cluster: local bind address is not advertisable: %s "+
			"(set cluster.bind_addr to this node's routable address)", bindAddr)
	}
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("cluster: listen on %s: %w", bindAddr, err)
	}
	// Every Raft group on this node shares the one port. The control group's
	// connections carry no header, exactly as they always have, so a node running
	// an older build is indistinguishable on the wire and a rolling upgrade does
	// not split the control group (issue #50).
	mux := NewTransportMuxWithAdvertise(ln, tcpAddr)
	transport := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream:  mux.ControlLayer(),
		MaxPool: 3,
		Timeout: raftTimeout,
		Logger:  nil,
	})

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

	node, err := newNodeWithDeps(cfg, metaStore, raftDeps{
		transport: transport,
		logStore:  logStore,
		stable:    logStore,
		snapshots: snapshotStore,
	})
	if err != nil {
		mux.Close()
		return nil, err
	}
	node.mux = mux
	return node, nil
}

// raftDeps bundles the pluggable Raft backends. Production uses a TCP transport
// with BoltDB-backed stores; tests inject in-memory implementations so a full
// multi-node cluster — including network partitions — can run in one process.
type raftDeps struct {
	transport raft.Transport
	logStore  raft.LogStore
	stable    raft.StableStore
	snapshots raft.SnapshotStore
}

// newNodeWithDeps builds and starts a Raft node from explicit dependencies.
// It is shared by the production constructor (NewNode) and by tests.
func newNodeWithDeps(cfg ClusterConfig, metaStore *metadata.Store, deps raftDeps) (*Node, error) {
	applyDefaults(&cfg)

	if cfg.NodeID == "" {
		return nil, fmt.Errorf("cluster: node_id is required")
	}

	// Raft configuration
	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)
	raftCfg.SnapshotThreshold = uint64(cfg.SnapshotCount)
	raftCfg.LogLevel = "WARN"

	fsm := NewFSM(metaStore)

	r, err := raft.NewRaft(raftCfg, fsm, deps.logStore, deps.stable, deps.snapshots, deps.transport)
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
				Address: deps.transport.LocalAddr(),
			},
		}
		future := r.BootstrapCluster(raft.Configuration{Servers: servers})
		if err := future.Error(); err != nil {
			// ErrCantBootstrap means already bootstrapped — not an error
			if err != raft.ErrCantBootstrap {
				return nil, fmt.Errorf("cluster: bootstrap: %w", err)
			}
		}
		slog.Info("cluster: bootstrapped", "node_id", cfg.NodeID, "addr", deps.transport.LocalAddr())
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

	slog.Info("cluster: node started", "node_id", cfg.NodeID, "peers", len(cfg.Peers))
	return node, nil
}

// Apply submits a command to the Raft log. Must be called on the leader.
func (n *Node) Apply(data []byte) error {
	_, err := n.ApplyIndexed(data)
	return err
}

// ApplyIndexed commits data through Raft and returns the log index of the
// committed entry. A follower that forwards a write uses this index to wait for
// its OWN FSM to apply the entry before acking the client, so an immediate local
// read sees the write (read-your-writes, issue #37). Leader-only.
func (n *Node) ApplyIndexed(data []byte) (uint64, error) {
	if n.raft.State() != raft.Leader {
		return 0, ErrNotLeader
	}
	future := n.raft.Apply(data, raftTimeout)
	if err := future.Error(); err != nil {
		return 0, fmt.Errorf("raft apply: %w", err)
	}
	// Check if the FSM returned an error
	if resp := future.Response(); resp != nil {
		if err, ok := resp.(error); ok {
			return 0, err
		}
	}
	// On the leader the entry is already applied to the local FSM once Error()
	// returns, so a subsequent read here is consistent.
	return future.Index(), nil
}

// ReadBarrier blocks until this node's FSM has applied everything the leader had
// applied at call time, making a follower read linearizable (read-your-writes for
// state written on other nodes, e.g. a bucket created elsewhere — issue #37). A
// no-op on the leader (it applies committed entries itself).
func (n *Node) ReadBarrier(timeout time.Duration) error {
	if n.IsLeader() {
		return nil
	}
	idx, err := n.leaderAppliedIndex(timeout)
	if err != nil {
		return err
	}
	return n.WaitForApply(idx, timeout)
}

// leaderAppliedIndex asks the leader for its current FSM applied index over the
// cluster channel (GET /cluster/readindex).
func (n *Node) leaderAppliedIndex(timeout time.Duration) (uint64, error) {
	leaderRaft := n.LeaderAddr()
	if leaderRaft == "" {
		return 0, fmt.Errorf("cluster: no leader for read barrier")
	}
	url := fmt.Sprintf("http://%s/cluster/readindex", apiAddrFromRaft(leaderRaft, n.cfg.APIPort))
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if n.cfg.Secret != "" {
		req.Header.Set(clusterSecretHeader, n.cfg.Secret)
	}
	resp, err := InterNodeClient(timeout).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("cluster: readindex returned %d", resp.StatusCode)
	}
	var out struct {
		Index uint64 `json:"index"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256)).Decode(&out); err != nil {
		return 0, err
	}
	return out.Index, nil
}

// ReadIndexHandler serves GET /cluster/readindex: this node's FSM applied index.
// A follower queries the leader to learn how far to catch up before a consistent
// read (issue #37). Cluster-secret authed.
func (n *Node) ReadIndexHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !n.authOK(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]uint64{"index": n.fsm.AppliedIndex()})
	}
}

// WaitForApply blocks until this node's FSM has applied up to index (or timeout).
// Used after forwarding a write to the leader so the local node — which the same
// client's follow-up read will hit — reflects the write (issue #37).
func (n *Node) WaitForApply(index uint64, timeout time.Duration) error {
	if index == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for n.fsm.AppliedIndex() < index {
		if time.Now().After(deadline) {
			return fmt.Errorf("cluster: local apply lagged index %d (at %d) after %s", index, n.fsm.AppliedIndex(), timeout)
		}
		time.Sleep(2 * time.Millisecond)
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
	err := n.raft.Shutdown().Error()
	// The mux owns the Raft port and every group's stream layer, so it is closed
	// after Raft, and only by the node that created it.
	if n.mux != nil {
		if e := n.mux.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
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
