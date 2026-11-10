package api

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
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
	// The body is optional, and an empty one means this node. A body that is
	// present but does not parse is refused: it used to be ignored, so a typo in
	// the node id field, or a truncated request, drained the node that happened
	// to receive it instead of the one named.
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

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
		// A key the server refuses names another object's file ("./doc" is
		// doc), so its data is never deleted here: a delete of such a legacy
		// record removes the record only, on every node.
		if bucket != "" && key != "" && h.engine != nil && storage.KeyProblem(key) == "" {
			// best-effort throughout; a missing file is the expected case on the
			// nodes that never held this object
			version := r.URL.Query().Get("version")
			if version != "" {
				_ = h.engine.DeleteObjectVersion(bucket, key, version)
			} else {
				_ = h.engine.DeleteObject(bucket, key)
			}
			h.copies.Forget(bucket, key, version)
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
			if key == "" || storage.KeyProblem(key) != "" {
				continue
			}
			_ = h.engine.DeleteObject(req.Bucket, key)
			h.copies.Forget(req.Bucket, key, "")
		}
		w.WriteHeader(http.StatusOK)
	}
}

// ClusterReplicaPutHandler stores an object's data on THIS node's local engine
// (no metadata write — that arrives via Raft — and no re-fan-out). The primary
// broadcasts here to keep replica copies so a node loss doesn't make an object
// unavailable (issue #37, replica_count > 1). Cluster-secret authed.
// ClusterReplicaPullHandler makes this node fetch a missing copy of an object
// from another cluster member.
//
// The source arrives as a node ID and is resolved against this node's own
// membership, never used as a URL. A repairing peer therefore cannot aim this
// node at an arbitrary host, which a caller-supplied address would allow.
func (h *APIHandler) ClusterReplicaPullHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		bucket := r.URL.Query().Get("bucket")
		key := r.URL.Query().Get("key")
		from := r.URL.Query().Get("from")
		version, ok := replicaVersion(r)
		if bucket == "" || key == "" || from == "" || h.engine == nil || h.peerAddr == nil || !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		addr := h.peerAddr(from)
		if addr == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		u := h.peerScheme + "://" + addr + "/cluster/replica-get?bucket=" +
			url.QueryEscape(bucket) + "&key=" + url.QueryEscape(key)
		if version != "" {
			u += "&version=" + url.QueryEscape(version)
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if secret != "" {
			req.Header.Set("X-Cluster-Secret", secret)
		}
		resp, err := replicaPullClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if !h.bucketCryptoReady(bucket, resp.Header.Get(BucketEncryptedHeader) == "1") {
			w.WriteHeader(http.StatusConflict)
			return
		}
		// A source copy recorded as another object than the metadata names
		// is not taken (see ClusterReplicaPutHandler), and a source that does
		// not say which version it sent (a node still on 5.0.14 sends the
		// plain file) is not taken for a version. The lock keeps the copy
		// from landing inside a client overwrite of the key on this node.
		if version != "" && resp.Header.Get(CopyVersionHeader) != version {
			w.WriteHeader(http.StatusConflict)
			return
		}
		etag := resp.Header.Get(CopyETagHeader)
		sourceIsCurrent := func() bool {
			if etag == "" {
				return true // a source with no record (5.0.14, or an older file): as before
			}
			meta, err := h.currentCopyMeta(bucket, key, version)
			return err == nil && meta != nil && meta.ETag == etag
		}
		if !sourceIsCurrent() {
			w.WriteHeader(http.StatusConflict)
			return
		}
		// Staged without the key's lock, installed under it (see
		// ClusterReplicaPutHandler).
		staged, err := storage.StageCopy(h.engine.DataDir(), resp.Body, resp.ContentLength)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer staged.Close()
		unlock := storage.LockObjectKey(bucket, storage.VersionLockKey(key, version))
		defer unlock()
		if !sourceIsCurrent() {
			w.WriteHeader(http.StatusConflict)
			return
		}
		body, err := staged.Reader()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := h.putLocalCopy(bucket, key, version, body, staged.Size); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		h.copies.Record(bucket, key, version, etag, storage.LocalCopySize(h.engine, bucket, key, version))
		w.WriteHeader(http.StatusOK)
	}
}

// replicaVersion reads a replica request's version parameter: "" for the
// plain (unversioned) copy. ok is false for a version ID that could name
// another key's file (storage.VersionIDProblem).
func replicaVersion(r *http.Request) (version string, ok bool) {
	version = r.URL.Query().Get("version")
	return version, version == "" || storage.VersionIDProblem(version) == ""
}

// openLocalCopy opens this node's copy of an object's plain data, or of one
// version. The null version of an object written before versioning was
// enabled is still at the plain path.
func (h *APIHandler) openLocalCopy(bucket, key, version string) (storage.ReadSeekCloser, int64, error) {
	if version == "" {
		return h.engine.GetObject(bucket, key)
	}
	rc, size, err := h.engine.GetObjectVersion(bucket, key, version)
	if err != nil && version == "null" {
		return h.engine.GetObject(bucket, key)
	}
	return rc, size, err
}

// putLocalCopy stores a copy of an object's plain data, or of one version.
func (h *APIHandler) putLocalCopy(bucket, key, version string, body io.Reader, size int64) error {
	if version == "" {
		_, _, err := h.engine.PutObject(bucket, key, body, size)
		return err
	}
	_, _, err := h.engine.PutObjectVersion(bucket, key, version, body, size)
	return err
}

// currentCopyMeta is the record a copy must match, read after catching up
// with the leader: the object's current record, or the version's.
func (h *APIHandler) currentCopyMeta(bucket, key, version string) (*metadata.ObjectMeta, error) {
	meta, err := h.store.GetObjectMetaConsistent(bucket, key)
	if version == "" || errors.Is(err, metadata.ErrShardUnavailable) {
		return meta, err
	}
	return h.store.GetObjectVersion(bucket, key, version)
}

var replicaPullClient = &http.Client{Timeout: 5 * time.Minute}

// SetPeerResolver wires cluster membership so replica pull can turn a node ID
// into an address without trusting one from the caller.
func (h *APIHandler) SetPeerResolver(scheme string, fn func(nodeID string) string) {
	h.peerScheme = scheme
	h.peerAddr = fn
}

// BucketEncryptedHeader carries the SENDING node's view of whether a bucket is
// encrypted at rest, so the receiving node can store its copy the same way.
//
// A node decides how to store an object by reading the bucket's encryption
// config out of its own metadata, and on a cluster that config arrives by Raft.
// A node that has not applied it yet answers "not encrypted" and writes the copy
// in the clear: the bucket's first write is the one that races, and one replica
// of it stays readable on disk forever in a bucket whose whole point is that it
// is not. The sender has just written the object and knows the answer, so it
// says so, and the receiver catches up before deciding rather than guessing.
const BucketEncryptedHeader = "X-Vaults3-Bucket-Encrypted"

// SetBucketCryptoGuard wires the bucket encryption lookup and the metadata read
// barrier that replica writes use to avoid storing a copy in the clear.
func (h *APIHandler) SetBucketCryptoGuard(isEncrypted func(bucket string) bool, barrier func() error) {
	h.bucketEncrypted = isEncrypted
	h.metaBarrier = barrier
}

// bucketCryptoReady reports whether this node can store a copy of an object the
// same way the sender stored it. When the sender says the bucket is encrypted
// and this node does not know that yet, it waits for its metadata to catch up
// and asks again. Saying no is safe: the copy is simply not written, and replica
// repair places it later, by which time the config has certainly arrived.
func (h *APIHandler) bucketCryptoReady(bucket string, senderEncrypted bool) bool {
	if !senderEncrypted || h.bucketEncrypted == nil {
		return true
	}
	if h.bucketEncrypted(bucket) {
		return true
	}
	if h.metaBarrier != nil {
		_ = h.metaBarrier()
	}
	return h.bucketEncrypted(bucket)
}

// ClusterReplicaGetHandler answers a peer asking about one object's local copy.
// HEAD reports whether this node holds it and how big it is, GET streams it.
//
// Replica repair needs both halves: it probes every node that should hold an
// object, and when the node that found the gap has no copy of its own it pulls
// one from a node that does. The 404 matters as much as the 200. A repairer
// treats "not found" as a missing copy and nothing else as an answer at all,
// so an unreachable peer is never mistaken for one that lost its data.
func (h *APIHandler) ClusterReplicaGetHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodHead && r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		bucket := r.URL.Query().Get("bucket")
		key := r.URL.Query().Get("key")
		version, ok := replicaVersion(r)
		if bucket == "" || key == "" || h.engine == nil || !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// The record is read before and after opening the file, and sent only
		// when both agree: an overwrite on this node between the two (it drops
		// the record before writing) would otherwise label the new bytes with
		// the old object's ETag, or the reverse.
		before, _, known := h.copies.Entry(bucket, key, version)
		reader, size, err := h.openLocalCopy(bucket, key, version)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		after, _ := h.copies.Lookup(bucket, key, version, size)
		defer func() { _ = reader.Close() }()
		if h.bucketEncrypted != nil && h.bucketEncrypted(bucket) {
			w.Header().Set(BucketEncryptedHeader, "1")
		}
		// Which object this copy is, when this node recorded it: replica
		// repair replaces a copy recorded as another object.
		if known && before == after {
			w.Header().Set(CopyETagHeader, before)
		}
		if version != "" {
			w.Header().Set(CopyVersionHeader, version)
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.Copy(w, reader)
	}
}

func (h *APIHandler) ClusterReplicaPutHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clusterAuthOK(r, secret) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		bucket := r.URL.Query().Get("bucket")
		key := r.URL.Query().Get("key")
		version, ok := replicaVersion(r)
		if bucket == "" || key == "" || h.engine == nil || !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !h.bucketCryptoReady(bucket, r.Header.Get(BucketEncryptedHeader) == "1") {
			// Writing now would store this copy in the clear. Refuse instead, and
			// let replica repair place it once the config has arrived.
			w.WriteHeader(http.StatusConflict)
			return
		}
		// A copy names the object it is (its ETag). Copies are sent after each
		// write, in no order, so the copy of the previous object, a 10 MiB
		// multipart upload still streaming, landed after the copy of the small
		// object that replaced it and put the old bytes back. The sending node
		// then read those and spread them: the acknowledged write was lost on
		// every node. A copy is stored only while it is the object the
		// metadata names, read after catching up with the leader.
		// Every sender of a version's copy names it; one that does not is not
		// one this server can check.
		if version != "" && r.Header.Get(ReplicaETagHeader) == "" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		etag := r.Header.Get(ReplicaETagHeader)
		if etag == "" {
			// A node still on 5.0.14 names no object: stored as before, and
			// the record dropped rather than kept wrong.
			if err := h.putLocalCopy(bucket, key, version, r.Body, r.ContentLength); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			h.copies.Forget(bucket, key, version)
			w.WriteHeader(http.StatusOK)
			return
		}
		// Checked once before receiving it (a copy of an older object is
		// refused without being read), and again under the key's lock before
		// it is installed. The bytes are staged on disk in between, without
		// the lock: held across the transfer, it made every client write of
		// the key on this node wait for the network, for good if the sender
		// stalled.
		if !h.copyIsCurrent(w, bucket, key, version, etag, r.ContentLength) {
			return
		}
		staged, err := storage.StageCopy(h.engine.DataDir(), r.Body, r.ContentLength)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer staged.Close()
		unlock := storage.LockObjectKey(bucket, storage.VersionLockKey(key, version))
		defer unlock()
		if !h.copyIsCurrent(w, bucket, key, version, etag, staged.Size) {
			return
		}
		body, err := staged.Reader()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := h.putLocalCopy(bucket, key, version, body, staged.Size); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		h.copies.Record(bucket, key, version, etag, storage.LocalCopySize(h.engine, bucket, key, version))
		w.WriteHeader(http.StatusOK)
	}
}

// copyIsCurrent reports whether a copy named etag, of size bytes, is the
// object the metadata names (read after catching up with the leader). It
// answers 409 when it is not.
func (h *APIHandler) copyIsCurrent(w http.ResponseWriter, bucket, key, version, etag string, size int64) bool {
	meta, err := h.currentCopyMeta(bucket, key, version)
	if err != nil || meta == nil || meta.ETag != etag || meta.DeleteMarker ||
		(version == "" && meta.VersionID != "") ||
		(meta.SSECustomerKeyMD5 == "" && size >= 0 && size != meta.Size) {
		current := ""
		if meta != nil {
			current = meta.ETag
		}
		slog.Debug("refused a replica copy that is not the current object",
			"bucket", bucket, "key", key, "version", version, "copy_etag", etag, "current_etag", current, "error", err)
		w.WriteHeader(http.StatusConflict)
		return false
	}
	return true
}

// CopyETagHeader carries, on a replica-get answer, the ETag the sending node
// recorded for its copy (metadata.CopyIndex), when it has one.
const CopyETagHeader = "X-VaultS3-Copy-ETag"

// CopyVersionHeader echoes, on a replica-get answer, the version it is about.
// A node still on 5.0.14 ignores the version parameter and answers about the
// key's plain file; without the echo that answer passed for the version.
const CopyVersionHeader = "X-VaultS3-Copy-Version"

// SetCopyIndex wires the node-local record of which object each local data
// file holds (cluster only).
func (h *APIHandler) SetCopyIndex(c *metadata.CopyIndex) { h.copies = c }

// ReplicaETagHeader carries, on a replica copy, the ETag of the object the
// copy is, so the receiving node can refuse a copy of an older object.
const ReplicaETagHeader = "X-VaultS3-Replica-ETag"

// handleClusterRebalance handles POST /api/v1/cluster/rebalance: trigger a
// background pass that moves objects to their correct hash-ring owner (used after
// membership changes to evacuate or absorb a node's data).
func (h *APIHandler) handleClusterRebalance(w http.ResponseWriter, _ *http.Request) {
	if h.triggerRebalance == nil {
		writeError(w, http.StatusBadRequest, "rebalance is unavailable (node not clustered)")
		return
	}
	// Rebalance no longer moves data: its transfers were refused by every
	// node, and had they worked they would have deleted valid copies. Replica
	// repair does the job, so the endpoint answers with what to use instead.
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "retired",
		"running": false,
		"message": "rebalance no longer moves data: replica repair restores every object's placement and copy count (POST /api/v1/cluster/repair, or wait for the next scheduled scan)",
	})
}

// SetReplicaRepair wires the replica repair trigger and its status reader.
func (h *APIHandler) SetReplicaRepair(trigger func(), status func() any) {
	h.triggerRepair = trigger
	h.repairStatus = status
}

// handleClusterRepair handles POST /api/v1/cluster/repair: run a replica repair
// pass now rather than waiting for the next scheduled scan.
func (h *APIHandler) handleClusterRepair(w http.ResponseWriter, _ *http.Request) {
	if h.triggerRepair == nil {
		writeError(w, http.StatusBadRequest, "replica repair is unavailable (node not clustered)")
		return
	}
	h.triggerRepair()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "triggered"})
}

// handleClusterRepairStatus handles GET /api/v1/cluster/repair: report what the
// last completed scan found.
func (h *APIHandler) handleClusterRepairStatus(w http.ResponseWriter, _ *http.Request) {
	if h.repairStatus == nil {
		writeError(w, http.StatusBadRequest, "replica repair is unavailable (node not clustered)")
		return
	}
	writeJSON(w, http.StatusOK, h.repairStatus())
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
