package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// NodeState represents the health state of a cluster node.
type NodeState int

const (
	NodeHealthy NodeState = iota
	NodeSuspect           // missed heartbeats but not yet declared down
	NodeDown              // confirmed down after threshold
)

func (s NodeState) String() string {
	switch s {
	case NodeHealthy:
		return "healthy"
	case NodeSuspect:
		return "suspect"
	case NodeDown:
		return "down"
	default:
		return "unknown"
	}
}

// NodeHealth tracks health info for a single node.
type NodeHealth struct {
	NodeID    string    `json:"node_id"`
	Addr      string    `json:"addr"`
	State     NodeState `json:"state"`
	LastSeen  time.Time `json:"last_seen"`
	FailCount int       `json:"fail_count"`
	LastError string    `json:"last_error,omitempty"`
}

// FailureDetector monitors cluster nodes via periodic health probes.
type FailureDetector struct {
	mu            sync.RWMutex
	nodes         map[string]*NodeHealth // nodeID → health
	selfID        string
	probInterval  time.Duration
	suspectAfter  int // consecutive failures before suspect
	downAfter     int // consecutive failures before down
	client        *http.Client
	onNodeDown    func(nodeID string) // callback when node transitions to down
	onNodeRecover func(nodeID string) // callback when node recovers
}

// DetectorConfig is an alias for config.DetectorConfig.
type DetectorConfig = config.DetectorConfig

func applyDetectorDefaults(c *DetectorConfig) {
	if c.ProbeIntervalSecs <= 0 {
		c.ProbeIntervalSecs = 5
	}
	if c.SuspectAfter <= 0 {
		c.SuspectAfter = 3
	}
	if c.DownAfter <= 0 {
		c.DownAfter = 6
	}
	if c.ProbeTimeoutSecs <= 0 {
		c.ProbeTimeoutSecs = 2
	}
}

// NewFailureDetector creates a failure detector.
func NewFailureDetector(selfID string, cfg DetectorConfig) *FailureDetector {
	applyDetectorDefaults(&cfg)
	return &FailureDetector{
		nodes:        make(map[string]*NodeHealth),
		selfID:       selfID,
		probInterval: time.Duration(cfg.ProbeIntervalSecs) * time.Second,
		suspectAfter: cfg.SuspectAfter,
		downAfter:    cfg.DownAfter,
		client:       InterNodeClient(time.Duration(cfg.ProbeTimeoutSecs) * time.Second),
	}
}

// SetCallbacks sets the node down/recover callbacks.
func (d *FailureDetector) SetCallbacks(onDown, onRecover func(nodeID string)) {
	d.onNodeDown = onDown
	d.onNodeRecover = onRecover
}

// AddNode registers a node to monitor.
func (d *FailureDetector) AddNode(nodeID, addr string) {
	if nodeID == d.selfID {
		return // don't monitor self
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nodes[nodeID] = &NodeHealth{
		NodeID:   nodeID,
		Addr:     addr,
		State:    NodeHealthy,
		LastSeen: time.Now(),
	}
}

// Reconcile makes the monitored set match a membership snapshot of node ID to
// API address: new members are added, departed ones dropped, and a member whose
// address changed is probed at the new one.
//
// The detector used to learn its peers once, at startup, from the configured
// list. A node that joined later (every node, under Kubernetes auto-join with no
// static peers) was never probed at all, and a peer whose address changed was
// probed at the old one forever and declared DOWN while it was serving. The
// proxy's membership sync calls this on every tick.
//
// A moved node keeps its state and failure count: if it was DOWN, the first
// successful probe at the new address brings it back through the normal recover
// path.
func (d *FailureDetector) Reconcile(members map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, addr := range members {
		if id == d.selfID || addr == "" {
			continue
		}
		nh, ok := d.nodes[id]
		if !ok {
			d.nodes[id] = &NodeHealth{NodeID: id, Addr: addr, State: NodeHealthy, LastSeen: time.Now()}
			slog.Info("failure detector: monitoring new member", "node_id", id, "addr", addr)
			continue
		}
		if nh.Addr != addr {
			slog.Info("failure detector: member address changed", "node_id", id, "from", nh.Addr, "to", addr)
			nh.Addr = addr
		}
	}
	for id := range d.nodes {
		if _, ok := members[id]; !ok {
			delete(d.nodes, id)
		}
	}
}

// RemoveNode stops monitoring a node.
func (d *FailureDetector) RemoveNode(nodeID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.nodes, nodeID)
}

// Run starts the probe loop. Blocks until ctx is cancelled.
func (d *FailureDetector) Run(ctx context.Context) {
	ticker := time.NewTicker(d.probInterval)
	defer ticker.Stop()

	slog.Info("failure detector started",
		"interval", d.probInterval,
		"suspect_after", d.suspectAfter,
		"down_after", d.downAfter,
	)

	for {
		select {
		case <-ctx.Done():
			slog.Info("failure detector stopped")
			return
		case <-ticker.C:
			d.probeAll()
		}
	}
}

func (d *FailureDetector) probeAll() {
	d.mu.RLock()
	nodes := make([]*NodeHealth, 0, len(d.nodes))
	for _, nh := range d.nodes {
		nodes = append(nodes, nh)
	}
	d.mu.RUnlock()

	for _, nh := range nodes {
		d.probeNode(nh)
	}
}

func (d *FailureDetector) probeNode(nh *NodeHealth) {
	// The address is read under the lock because Reconcile may move it.
	d.mu.RLock()
	addr := nh.Addr
	d.mu.RUnlock()
	resp, err := d.client.Get(interNodeURL(addr, "/health"))

	d.mu.Lock()
	defer d.mu.Unlock()

	prevState := nh.State

	if err != nil {
		nh.FailCount++
		nh.LastError = err.Error()

		if nh.FailCount >= d.downAfter && nh.State != NodeDown {
			nh.State = NodeDown
			slog.Warn("node declared DOWN",
				"node_id", nh.NodeID,
				"addr", nh.Addr,
				"fail_count", nh.FailCount,
			)
		} else if nh.FailCount >= d.suspectAfter && nh.State == NodeHealthy {
			nh.State = NodeSuspect
			slog.Warn("node SUSPECT",
				"node_id", nh.NodeID,
				"addr", nh.Addr,
				"fail_count", nh.FailCount,
			)
		}
	} else {
		resp.Body.Close()
		if resp.StatusCode < 500 {
			nh.FailCount = 0
			nh.State = NodeHealthy
			nh.LastSeen = time.Now()
			nh.LastError = ""
		} else {
			nh.FailCount++
			nh.LastError = fmt.Sprintf("HTTP %d", resp.StatusCode)
			if nh.FailCount >= d.downAfter {
				nh.State = NodeDown
			} else if nh.FailCount >= d.suspectAfter {
				nh.State = NodeSuspect
			}
		}
	}

	// Fire callbacks on state transitions
	if prevState != NodeDown && nh.State == NodeDown && d.onNodeDown != nil {
		go d.onNodeDown(nh.NodeID)
	}
	if prevState == NodeDown && nh.State == NodeHealthy && d.onNodeRecover != nil {
		go d.onNodeRecover(nh.NodeID)
	}
}

// NodeStates returns the current health of all monitored nodes.
func (d *FailureDetector) NodeStates() []NodeHealth {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]NodeHealth, 0, len(d.nodes))
	for _, nh := range d.nodes {
		result = append(result, *nh)
	}
	return result
}

// IsNodeDown checks if a specific node is marked as down.
func (d *FailureDetector) IsNodeDown(nodeID string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if nh, ok := d.nodes[nodeID]; ok {
		return nh.State == NodeDown
	}
	return false
}

// HealthyNodes returns the set of node IDs currently considered healthy.
func (d *FailureDetector) HealthyNodes() map[string]bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	healthy := make(map[string]bool, len(d.nodes)+1)
	healthy[d.selfID] = true // self is always healthy
	for _, nh := range d.nodes {
		if nh.State == NodeHealthy {
			healthy[nh.NodeID] = true
		}
	}
	return healthy
}
