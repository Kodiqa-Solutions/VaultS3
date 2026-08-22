package cluster

import (
	"bytes"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// The store-level hop (issue #50).
//
// A node that does not hold a bucket's metadata shard asks one that does. This
// is a hop inside the metadata store, not a proxy of the S3 request: the request
// itself stays where it landed, because the object bytes are placed by the hash
// ring and the handler gets exactly one proxy hop, which the data placement
// already spends.

const (
	// shardCallPath serves reads against a shard held on this node.
	shardCallPath = "/cluster/shard-call"
	// shardApplyPath commits a write to a shard led by this node.
	shardApplyPath = "/cluster/shard-apply"
	// shardLeaderHeader names the shard's leader when a write reaches a member
	// that is not it, so the caller can retry in one hop instead of the server
	// chaining hops of its own.
	shardLeaderHeader = "X-VaultS3-Shard-Leader"
	// maxShardRPCBody bounds a request body. Shard RPCs carry metadata records,
	// never object payloads.
	maxShardRPCBody = 8 << 20
)

type shardCallEnvelope struct {
	Shard   int                   `json:"shard"`
	Request metadata.ShardRequest `json:"request"`
}

type shardApplyEnvelope struct {
	Shard  int    `json:"shard"`
	Bucket string `json:"bucket"`
	Data   []byte `json:"data"`
}

// shardLeaderRedirect reports that a write reached a member that does not lead
// the shard, and names the one that does.
type shardLeaderRedirect struct {
	shard  int
	leader string
}

func (e *shardLeaderRedirect) Error() string {
	if e.leader == "" {
		return fmt.Sprintf("shard %d has no leader", e.shard)
	}
	return fmt.Sprintf("shard %d is led by %s", e.shard, e.leader)
}

func asShardLeaderRedirect(err error, out **shardLeaderRedirect) bool {
	var r *shardLeaderRedirect
	if errors.As(err, &r) {
		*out = r
		return true
	}
	return false
}

// authorize checks an inter-node shard RPC, and fails CLOSED.
//
// This first shipped copying the control plane's fail-open pattern, which is
// exactly how that hole got there. A shard RPC moves object metadata, so an
// unauthenticated one is an anonymous write to another node's store.
func (r *ShardRouter) authorize(req *http.Request) bool {
	if r.secret == "" {
		return false
	}
	return hmac.Equal([]byte(req.Header.Get(clusterSecretHeader)), []byte(r.secret))
}

func (r *ShardRouter) setAuth(req *http.Request) {
	if r.secret != "" {
		req.Header.Set(clusterSecretHeader, r.secret)
	}
}

// ownsShard reports whether this node's committed assignment agrees that a
// bucket belongs to the shard the caller addressed. A caller routing by a
// different assignment is refused rather than served: writing a bucket's records
// into the wrong group would hide them from every correct reader.
func (r *ShardRouter) ownsShard(bucket string, shard int) error {
	m := r.Map()
	if m == nil {
		return fmt.Errorf("%w: no committed shard assignment on %s", ErrShardUnavailable, r.nodeID)
	}
	if want := ShardForBucket(bucket, m.Shards); want != shard {
		return fmt.Errorf("cluster: bucket %q belongs to shard %d here, caller addressed shard %d",
			bucket, want, shard)
	}
	return nil
}

// CallHandler serves POST /cluster/shard-call: a peer's read against a shard
// this node holds. Inter-node use only.
func (r *ShardRouter) CallHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !r.authorize(req) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var env shardCallEnvelope
		if err := json.NewDecoder(io.LimitReader(req.Body, maxShardRPCBody)).Decode(&env); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := r.ownsShard(env.Request.Bucket, env.Shard); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		g, err := r.runtime.Group(env.Shard)
		if err != nil {
			// Not running here. Unavailable, never an empty result.
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		// A listing must come from the leader, so a follower names it rather than
		// answering from a copy that may be an entry behind.
		if env.Request.LeaderOnly && !g.IsLeader() {
			if leader := g.LeaderID(); leader != "" {
				w.Header().Set(shardLeaderHeader, leader)
			}
			http.Error(w, ErrNotShardLeader.Error(), http.StatusServiceUnavailable)
			return
		}
		resp := metadata.ExecuteShardRequest(g, env.Request)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// ApplyHandler serves POST /cluster/shard-apply: a peer's write to a shard this
// node leads. A member that is not the leader names the leader instead of
// forwarding, so a leadership flap cannot make a write bounce between nodes.
// Inter-node use only.
func (r *ShardRouter) ApplyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !r.authorize(req) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var env shardApplyEnvelope
		if err := json.NewDecoder(io.LimitReader(req.Body, maxShardRPCBody)).Decode(&env); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := r.ownsShard(env.Bucket, env.Shard); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		g, err := r.runtime.Group(env.Shard)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if !g.IsLeader() {
			if leader := g.LeaderID(); leader != "" {
				w.Header().Set(shardLeaderHeader, leader)
			}
			http.Error(w, ErrNotShardLeader.Error(), http.StatusServiceUnavailable)
			return
		}
		if _, err := g.Apply(env.Data, r.timeout); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (r *ShardRouter) post(addr, path string, body []byte, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s%s", addr, path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	r.setAuth(req)
	return InterNodeClient(timeout).Do(req)
}

func (r *ShardRouter) postCall(addr string, shard int, request metadata.ShardRequest) (metadata.ShardResponse, error) {
	body, err := json.Marshal(shardCallEnvelope{Shard: shard, Request: request})
	if err != nil {
		return metadata.ShardResponse{}, err
	}
	resp, err := r.post(addr, shardCallPath, body, r.timeout)
	if err != nil {
		return metadata.ShardResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if resp.StatusCode == http.StatusServiceUnavailable {
			if leader := resp.Header.Get(shardLeaderHeader); leader != "" {
				return metadata.ShardResponse{}, &shardLeaderRedirect{shard: shard, leader: leader}
			}
		}
		return metadata.ShardResponse{}, fmt.Errorf("%s returned %d: %s", addr, resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out metadata.ShardResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxShardRPCBody)).Decode(&out); err != nil {
		return metadata.ShardResponse{}, fmt.Errorf("%s sent an undecodable reply: %w", addr, err)
	}
	return out, nil
}

func (r *ShardRouter) postApply(addr string, shard int, bucket string, data []byte) error {
	body, err := json.Marshal(shardApplyEnvelope{Shard: shard, Bucket: bucket, Data: data})
	if err != nil {
		return err
	}
	resp, err := r.post(addr, shardApplyPath, body, r.timeout)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode == http.StatusServiceUnavailable {
		if leader := resp.Header.Get(shardLeaderHeader); leader != "" {
			return &shardLeaderRedirect{shard: shard, leader: leader}
		}
	}
	return fmt.Errorf("%s returned %d: %s", addr, resp.StatusCode, bytes.TrimSpace(msg))
}
