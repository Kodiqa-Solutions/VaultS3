package api

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
)

// ClusterController is the subset of cluster.Node the admin API needs for
// membership operations. An adapter in server.go implements it so this package
// does not import internal/cluster.
type ClusterController interface {
	SelfID() string
	IsLeader() bool
	LeaderID() string
	Members() []ClusterMember
	Join(nodeID, addr string) error
	Leave(nodeID string) error
	// ShardMap returns the committed metadata shard assignment, or nil when the
	// cluster has not committed one, which is the case for every cluster that
	// runs unsharded (issue #50).
	ShardMap() *ShardAssignment
	// LocalShards reports the metadata shard groups running on this node. Empty
	// when the node runs none, which is normal on an unsharded cluster and is
	// also how an operator sees a node that has not caught up with the map yet.
	LocalShards() []LocalShard
}

// ShardAssignment is the committed metadata shard map as the admin API reports
// it. Object metadata is replicated to every node today, which is what caps a
// cluster's object count; sharding splits it across independent Raft groups.
type ShardAssignment struct {
	Version  uint64     `json:"version"`
	Epoch    uint64     `json:"epoch"`
	Shards   int        `json:"shards"`
	Replicas int        `json:"replicas"`
	Members  [][]string `json:"members"`
	Founders [][]string `json:"founders"`
}

// LocalShard is one metadata shard group running on this node, as the admin API
// reports it. Members is the shard's own Raft configuration, which is what the
// reconciler drives towards the committed assignment: a member listed here but
// not in the assignment is one that is still being removed, and the reverse is
// one still being added.
type LocalShard struct {
	Shard    int      `json:"shard"`
	IsLeader bool     `json:"isLeader"`
	LeaderID string   `json:"leaderId"`
	Members  []string `json:"members"`
}

// ClusterMember is one Raft member as reported by the cluster status endpoint.
type ClusterMember struct {
	NodeID   string `json:"nodeId"`
	Address  string `json:"address"`  // raft address
	Suffrage string `json:"suffrage"` // Voter / Nonvoter
	Leader   bool   `json:"leader"`
}

// SetWritable wires the node-local write gate (shared with the S3 handler). When
// it holds false the node is "drained": S3 object writes are rejected while reads
// continue. nil ⇒ always writable. Enables the drain/undrain admin endpoints.
func (h *APIHandler) SetWritable(w *atomic.Bool) { h.writable = w }

// SetClusterController wires the cluster-membership operations and the rebalance
// trigger for the admin cluster endpoints (not-clustered if unset).
func (h *APIHandler) SetClusterController(ctl ClusterController, triggerRebalance func(), rebalanceRunning func() bool) {
	h.clusterCtl = ctl
	h.triggerRebalance = triggerRebalance
	h.rebalanceRunning = rebalanceRunning
}

// isWritable reports whether this node currently accepts writes.
func (h *APIHandler) isWritable() bool { return h.writable == nil || h.writable.Load() }

// handleClusterShards handles GET /api/v1/cluster/shards: how object metadata is
// distributed. A cluster with no committed map replicates all metadata to every
// node, so it reports sharded=false rather than an empty assignment, which would
// read as "sharded, and holding nothing".
func (h *APIHandler) handleClusterShards(w http.ResponseWriter, _ *http.Request) {
	if h.clusterCtl == nil {
		writeJSON(w, http.StatusOK, map[string]any{"clustered": false, "sharded": false})
		return
	}
	m := h.clusterCtl.ShardMap()
	if m == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"clustered": true,
			"sharded":   false,
			"selfId":    h.clusterCtl.SelfID(),
			"note":      "object metadata is replicated to every node; adding nodes adds data capacity, not metadata capacity",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clustered":   true,
		"sharded":     true,
		"selfId":      h.clusterCtl.SelfID(),
		"shardMap":    m,
		"localShards": h.clusterCtl.LocalShards(),
	})
}

// handleClusterStatus handles GET /api/v1/cluster/status: Raft membership, the
// current leader, and this node's write (drain) state.
func (h *APIHandler) handleClusterStatus(w http.ResponseWriter, _ *http.Request) {
	if h.clusterCtl == nil {
		writeJSON(w, http.StatusOK, map[string]any{"clustered": false, "writable": h.isWritable()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clustered": true,
		"selfId":    h.clusterCtl.SelfID(),
		"leaderId":  h.clusterCtl.LeaderID(),
		"isLeader":  h.clusterCtl.IsLeader(),
		"writable":  h.isWritable(),
		"members":   h.clusterCtl.Members(),
	})
}

// handleClusterJoin handles POST /api/v1/cluster/join {nodeId, addr}: add a Raft
// member. Must be run against the leader (the node method redirects otherwise).
func (h *APIHandler) handleClusterJoin(w http.ResponseWriter, r *http.Request) {
	if h.clusterCtl == nil {
		writeError(w, http.StatusBadRequest, "this node is not running in cluster mode")
		return
	}
	var req struct {
		NodeID string `json:"nodeId"`
		Addr   string `json:"addr"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NodeID == "" || req.Addr == "" {
		writeError(w, http.StatusBadRequest, "nodeId and addr are required")
		return
	}
	if err := h.clusterCtl.Join(req.NodeID, req.Addr); err != nil {
		writeError(w, http.StatusInternalServerError, "join failed: "+err.Error()+" (run join against the leader node)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": "node " + req.NodeID + " joined"})
}

// handleClusterLeave handles POST /api/v1/cluster/leave {nodeId}: remove a Raft
// member. Removing a node that still holds the only copy of data loses it — drain
// and rebalance first (see docs/SCALING.md).
func (h *APIHandler) handleClusterLeave(w http.ResponseWriter, r *http.Request) {
	if h.clusterCtl == nil {
		writeError(w, http.StatusBadRequest, "this node is not running in cluster mode")
		return
	}
	var req struct {
		NodeID string `json:"nodeId"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "nodeId is required")
		return
	}
	if err := h.clusterCtl.Leave(req.NodeID); err != nil {
		writeError(w, http.StatusInternalServerError, "leave failed: "+err.Error()+" (run leave against the leader node)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": "node " + req.NodeID + " removed"})
}

// handleClusterDrain handles POST /api/v1/cluster/drain and /undrain. Body
// {nodeId} is optional: empty or this node's ID drains the node serving the
// request; another node's ID is forwarded over the cluster channel. Draining
// makes a node reject S3 object writes (503) while still serving reads, so it can
// be evacuated for replacement or maintenance.
func (h *APIHandler) handleClusterDrain(w http.ResponseWriter, r *http.Request, drain bool) {
	if h.writable == nil {
		writeError(w, http.StatusBadRequest, "drain is unavailable on this node")
		return
	}
	var req struct {
		NodeID string `json:"nodeId"`
	}
	_ = readJSON(r, &req) // body optional (defaults to this node)

	self := ""
	if h.clusterCtl != nil {
		self = h.clusterCtl.SelfID()
	}
	if req.NodeID != "" && req.NodeID != self {
		if err := h.forwardDrain(req.NodeID, drain); err != nil {
			writeError(w, http.StatusBadGateway, "forward drain to "+req.NodeID+" failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodeId": req.NodeID, "writable": !drain})
		return
	}

	h.writable.Store(!drain)
	writeJSON(w, http.StatusOK, map[string]any{"nodeId": self, "writable": !drain})
}

// forwardDrain sets the drain state on another node over the cluster channel
// (POST /cluster/drain?state=, cluster-secret authed) using the peer address the
// placement proxy already reaches.
func (h *APIHandler) forwardDrain(nodeID string, drain bool) error {
	if h.clusterNodeAddrs == nil {
		return fmt.Errorf("no peer address map")
	}
	addr := h.clusterNodeAddrs()[nodeID]
	if addr == "" {
		return fmt.Errorf("unknown node %q", nodeID)
	}
	scheme := "http"
	if h.cfg != nil && h.cfg.Server.TLS.Enabled {
		scheme = "https"
	}
	state := "false"
	if !drain {
		state = "true"
	}
	req, err := http.NewRequest(http.MethodPost, scheme+"://"+addr+"/cluster/drain?state="+state, nil)
	if err != nil {
		return err
	}
	if h.clusterSecret != "" {
		req.Header.Set(clusterSecretHeader, h.clusterSecret)
	}
	resp, err := clusterInfoClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// ClusterDrainHandler serves POST /cluster/drain?state=true|false on the cluster
// channel (cluster-secret authed), letting the coordinator set this node's write
// gate. state=true ⇒ writable, state=false ⇒ drained.
func (h *APIHandler) ClusterDrainHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if h.writable != nil {
			h.writable.Store(r.URL.Query().Get("state") != "false")
		}
		w.WriteHeader(http.StatusOK)
	}
}

// ClusterObjectDeleteHandler removes a single object's data file from THIS node's
// local engine only (no metadata, no proxy). The delete coordinator broadcasts it
// to every node to reap replica/orphan copies left after a delete, so deleted
// data doesn't linger on disk (issue #34 layer 2, every delete path since #47).
// A `version` param targets one version instead of the plain object. Cluster-secret
// authed; best-effort — a missing file is not an error.
func (h *APIHandler) ClusterObjectDeleteHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		bucket := r.URL.Query().Get("bucket")
		key := r.URL.Query().Get("key")
		if bucket != "" && key != "" && h.engine != nil {
			// best-effort throughout; a missing file is the expected case on the
			// nodes that never held this object
			if version := r.URL.Query().Get("version"); version != "" {
				_ = h.engine.DeleteObjectVersion(bucket, key, version)
			} else {
				_ = h.engine.DeleteObject(bucket, key)
			}
		}
		w.WriteHeader(http.StatusOK)
	}
}

// ClusterObjectDeleteBatchHandler is the multi-key form of
// ClusterObjectDeleteHandler: it removes many objects' data files from THIS node's
// local engine in one request. The multi-object delete broadcasts here so a
// thousand-key batch costs one request per peer rather than one per peer per key
// (issue #47). Body is a JSON {"bucket": "...", "keys": [...]}. Cluster-secret
// authed; best-effort — missing files are the expected case on nodes that never
// held these objects.
func (h *APIHandler) ClusterObjectDeleteBatchHandler(secret string) http.HandlerFunc {
	// Bounded so a malformed or hostile body cannot make a node allocate without
	// limit; a well-behaved batch is at most 1000 keys (the S3 cap).
	const maxBatchBody = 4 << 20
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			Bucket string   `json:"bucket"`
			Keys   []string `json:"keys"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBatchBody)).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Bucket == "" || h.engine == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, key := range req.Keys {
			if key == "" {
				continue
			}
			_ = h.engine.DeleteObject(req.Bucket, key)
		}
		w.WriteHeader(http.StatusOK)
	}
}

// ClusterReplicaPutHandler stores an object's data on THIS node's local engine
// (no metadata write — that arrives via Raft — and no re-fan-out). The primary
// broadcasts here to keep replica copies so a node loss doesn't make an object
// unavailable (issue #37, replica_count > 1). Cluster-secret authed.
func (h *APIHandler) ClusterReplicaPutHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		bucket := r.URL.Query().Get("bucket")
		key := r.URL.Query().Get("key")
		if bucket == "" || key == "" || h.engine == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, _, err := h.engine.PutObject(bucket, key, r.Body, r.ContentLength); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// handleClusterRebalance handles POST /api/v1/cluster/rebalance: trigger a
// background pass that moves objects to their correct hash-ring owner (used after
// membership changes to evacuate or absorb a node's data).
func (h *APIHandler) handleClusterRebalance(w http.ResponseWriter, _ *http.Request) {
	if h.triggerRebalance == nil {
		writeError(w, http.StatusBadRequest, "rebalance is unavailable (node not clustered)")
		return
	}
	h.triggerRebalance()
	running := false
	if h.rebalanceRunning != nil {
		running = h.rebalanceRunning()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "triggered", "running": running})
}

// clusterAuthOK authorizes an inter-node request, and fails CLOSED.
//
// These handlers used to authorize only when a secret was configured, so a
// cluster running the shipped default, which set none, served every one of them
// to anonymous callers on the public S3 port: object deletion, reclaim, an
// existence oracle, multipart state and node inventory. An unset secret is now a
// refusal rather than a bypass, and the server will not start clustered without
// one, so reaching this branch means a misconfiguration that has already been
// reported loudly (security assessment finding 7).
func clusterAuthOK(r *http.Request, secret string) bool {
	if secret == "" {
		return false
	}
	return hmac.Equal([]byte(r.Header.Get(clusterSecretHeader)), []byte(secret))
}
