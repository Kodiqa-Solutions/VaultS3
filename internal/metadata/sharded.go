package metadata

import (
	"errors"
	"fmt"
	"time"
)

// Sharded metadata (issue #50).
//
// An unsharded cluster Raft-replicates every metadata record to every node, so
// adding nodes buys data capacity and no metadata capacity at all. A sharded
// cluster splits the OBJECT metadata across independent Raft groups, one per
// shard, and keeps everything else (buckets, IAM, policies, multipart state) in
// the control group that still spans every node.
//
// ShardedStore is the store handlers see in that mode. It answers the
// control-group surface exactly as before, and routes the object surface to the
// group that owns the bucket: locally when this node holds a copy of that shard,
// over one store-level RPC hop when it does not.
//
// The hop lives here, in the store, and not in the request router. Data
// placement and metadata placement are two independent constraints, and the S3
// handler allows exactly one proxy hop, so proxying the whole request to a
// metadata member would land the object bytes where no read path looks for them.

// ErrShardUnavailable means a shard could not be asked: no committed assignment,
// no member reachable, or no leader. It is deliberately distinct from "the shard
// holds no such record".
//
// Metadata is authoritative for reconciliation in this system, so reading "I
// could not ask" as "it does not exist" is precisely how orphan reclaim deletes
// live data (issue #47). Every path that can fail this way must fail loudly.
var ErrShardUnavailable = errors.New("metadata shard unavailable")

// ShardHandle is one metadata shard as seen from the node serving it.
// cluster.ShardGroup satisfies it.
type ShardHandle interface {
	// Store is the shard's own metadata store, holding only its buckets' objects.
	Store() *Store
	// IsLeader reports whether this node currently leads the shard's Raft group.
	IsLeader() bool
}

// ShardRouter is the cluster-side placement and transport a ShardedStore needs.
// It is an interface so this package keeps no dependency on internal/cluster,
// which already depends on it.
type ShardRouter interface {
	// Local returns this node's handle on the shard owning bucket, and false when
	// this node holds no copy of it. False is a routing fact, not an absence of
	// data: the caller asks a member instead.
	Local(bucket string) (ShardHandle, bool)
	// Call runs a request against a member of the shard owning req.Bucket. It
	// must return an error wrapping ErrShardUnavailable when no member could be
	// reached, and never an empty result standing in for one.
	Call(req ShardRequest) (ShardResponse, error)
	// Write commits an already-serialized metadata command to the Raft group
	// owning bucket, on this node when it leads that shard and on the shard's
	// leader otherwise.
	Write(bucket string, data []byte) error
	// Leading returns the shards this node currently leads. Full scans are
	// partitioned across shard leaders: each leader walks its own shards, so the
	// union across the cluster is every object, walked once.
	Leading() []ShardHandle
}

// ShardOp names a routed store operation.
type ShardOp string

const (
	OpGetObjectMeta           ShardOp = "get_object_meta"
	OpGetObjectMetaConsistent ShardOp = "get_object_meta_consistent"
	OpGetObjectVersion        ShardOp = "get_object_version"
	OpLatestObjectVersion     ShardOp = "latest_object_version"
	OpListLatestObjects       ShardOp = "list_latest_objects"
	OpListLatestDelimited     ShardOp = "list_latest_delimited"
	OpListObjectVersions      ShardOp = "list_object_versions"
	OpBucketStats             ShardOp = "bucket_stats"
	OpSetBucketStats          ShardOp = "set_bucket_stats"
	OpBackfillBucketStats     ShardOp = "backfill_bucket_stats"
	OpUpdateLastAccess        ShardOp = "update_last_access"
)

// ShardRequest is one routed store operation. One request type covers the whole
// routed surface so the wire format has a single shape to version.
type ShardRequest struct {
	Op            ShardOp    `json:"op"`
	Bucket        string     `json:"bucket"`
	Key           string     `json:"key,omitempty"`
	VersionID     string     `json:"version_id,omitempty"`
	Prefix        string     `json:"prefix,omitempty"`
	Delimiter     string     `json:"delimiter,omitempty"`
	StartAfter    string     `json:"start_after,omitempty"`
	KeyMarker     string     `json:"key_marker,omitempty"`
	VersionMarker string     `json:"version_marker,omitempty"`
	MaxKeys       int        `json:"max_keys,omitempty"`
	Stat          BucketStat `json:"stat,omitempty"`
	// LeaderOnly marks an operation that only the shard's leader may answer.
	// Listings are the case that matters: a follower can be an entry or two
	// behind, and a listing served from one omits a key the client has just
	// written and been told was stored (issue #37, the same reason bucket
	// listings go to the control leader on an unsharded cluster).
	LeaderOnly bool `json:"leader_only,omitempty"`
}

// ShardResponse is the result of a routed operation.
//
// Error carries a failure the OWNING shard reported, which includes a record
// that genuinely does not exist, because that is how the local store reports it
// too. It never carries "I could not reach the shard": that is a transport
// failure and comes back as an error wrapping ErrShardUnavailable, so the two
// can never be confused.
type ShardResponse struct {
	Meta       *ObjectMeta        `json:"meta,omitempty"`
	Metas      []ObjectMeta       `json:"metas,omitempty"`
	Prefixes   []CommonPrefixInfo `json:"prefixes,omitempty"`
	Truncated  bool               `json:"truncated,omitempty"`
	NextMarker string             `json:"next_marker,omitempty"`
	Stat       BucketStat         `json:"stat,omitempty"`
	Found      bool               `json:"found,omitempty"`
	Error      string             `json:"error,omitempty"`
}

func (r ShardResponse) err() error {
	if r.Error == "" {
		return nil
	}
	return errors.New(r.Error)
}

func shardErr(err error) ShardResponse {
	if err == nil {
		return ShardResponse{}
	}
	return ShardResponse{Error: err.Error()}
}

// ShardReadYourWritesTimeout bounds how long a shard follower waits for a
// just-written record to replicate to it before reporting the miss. It mirrors
// ReadYourWritesTimeout, which does the same job for the control group.
var ShardReadYourWritesTimeout = 2 * time.Second

// ExecuteShardRequest runs a routed operation against the shard held on this
// node. Both the local fast path and the RPC server call it, so a request is
// served identically wherever it lands.
func ExecuteShardRequest(h ShardHandle, req ShardRequest) ShardResponse {
	s := h.Store()
	switch req.Op {
	case OpGetObjectMeta:
		meta, err := s.GetObjectMeta(req.Bucket, req.Key)
		if err != nil {
			return shardErr(err)
		}
		return ShardResponse{Meta: meta}

	case OpGetObjectMetaConsistent:
		// A shard follower may not have applied a write the shard leader has
		// already acknowledged. Wait for it to arrive by normal replication
		// rather than reporting a 404 the client's own PUT contradicts (#37).
		meta, err := s.GetObjectMeta(req.Bucket, req.Key)
		if meta == nil && !h.IsLeader() {
			deadline := time.Now().Add(ShardReadYourWritesTimeout)
			for time.Now().Before(deadline) {
				time.Sleep(15 * time.Millisecond)
				if meta, err = s.GetObjectMeta(req.Bucket, req.Key); meta != nil {
					break
				}
			}
		}
		if err != nil {
			return shardErr(err)
		}
		return ShardResponse{Meta: meta}

	case OpGetObjectVersion:
		meta, err := s.GetObjectVersion(req.Bucket, req.Key, req.VersionID)
		if err != nil {
			return shardErr(err)
		}
		return ShardResponse{Meta: meta}

	case OpLatestObjectVersion:
		meta, err := s.LatestObjectVersion(req.Bucket, req.Key)
		if err != nil {
			return shardErr(err)
		}
		return ShardResponse{Meta: meta}

	case OpListLatestObjects:
		metas, truncated, err := s.ListLatestObjects(req.Bucket, req.Prefix, req.StartAfter, req.MaxKeys)
		if err != nil {
			return shardErr(err)
		}
		return ShardResponse{Metas: metas, Truncated: truncated}

	case OpListLatestDelimited:
		metas, prefixes, truncated, next, err := s.ListLatestObjectsDelimited(
			req.Bucket, req.Prefix, req.Delimiter, req.StartAfter, req.MaxKeys)
		if err != nil {
			return shardErr(err)
		}
		return ShardResponse{Metas: metas, Prefixes: prefixes, Truncated: truncated, NextMarker: next}

	case OpListObjectVersions:
		metas, truncated, err := s.ListObjectVersions(
			req.Bucket, req.Prefix, req.KeyMarker, req.VersionMarker, req.MaxKeys)
		if err != nil {
			return shardErr(err)
		}
		return ShardResponse{Metas: metas, Truncated: truncated}

	case OpBucketStats:
		stat, found, err := s.BucketStats(req.Bucket)
		if err != nil {
			return shardErr(err)
		}
		return ShardResponse{Stat: stat, Found: found}

	case OpSetBucketStats:
		return shardErr(s.SetBucketStats(req.Bucket, req.Stat))

	case OpBackfillBucketStats:
		stat, err := s.BackfillBucketStats(req.Bucket)
		if err != nil {
			return shardErr(err)
		}
		return ShardResponse{Stat: stat, Found: true}

	case OpUpdateLastAccess:
		s.UpdateLastAccess(req.Bucket, req.Key)
		return ShardResponse{}

	default:
		return shardErr(fmt.Errorf("unknown metadata shard operation %q", req.Op))
	}
}

// ShardedStore is the metadata store of a sharded cluster: the control group for
// everything cluster-wide, the owning shard for object metadata.
type ShardedStore struct {
	*DistributedStore
	router ShardRouter
}

// NewShardedStore wraps the control-group store with shard routing.
func NewShardedStore(control *DistributedStore, router ShardRouter) *ShardedStore {
	return &ShardedStore{DistributedStore: control, router: router}
}

// call runs a routed operation, locally when this node holds the shard and over
// one RPC hop when it does not. A hop that cannot be made is an error, never an
// empty result.
func (s *ShardedStore) call(req ShardRequest) (ShardResponse, error) {
	if h, ok := s.router.Local(req.Bucket); ok && (!req.LeaderOnly || h.IsLeader()) {
		return ExecuteShardRequest(h, req), nil
	}
	resp, err := s.router.Call(req)
	if err != nil {
		return ShardResponse{}, err
	}
	return resp, nil
}

// write commits an object-metadata command to the owning shard's Raft group.
func (s *ShardedStore) write(bucket string, cmdType uint16, payload interface{}) error {
	data, err := marshalRaftCommand(cmdType, payload)
	if err != nil {
		return err
	}
	return s.router.Write(bucket, data)
}

func (s *ShardedStore) meta(req ShardRequest) (*ObjectMeta, error) {
	resp, err := s.call(req)
	if err != nil {
		return nil, err
	}
	if err := resp.err(); err != nil {
		return nil, err
	}
	return resp.Meta, nil
}

// --- Object reads ---

func (s *ShardedStore) GetObjectMeta(bucket, key string) (*ObjectMeta, error) {
	return s.meta(ShardRequest{Op: OpGetObjectMeta, Bucket: bucket, Key: key})
}

func (s *ShardedStore) GetObjectMetaConsistent(bucket, key string) (*ObjectMeta, error) {
	return s.meta(ShardRequest{Op: OpGetObjectMetaConsistent, Bucket: bucket, Key: key})
}

func (s *ShardedStore) GetObjectVersion(bucket, key, versionID string) (*ObjectMeta, error) {
	return s.meta(ShardRequest{Op: OpGetObjectVersion, Bucket: bucket, Key: key, VersionID: versionID})
}

func (s *ShardedStore) LatestObjectVersion(bucket, key string) (*ObjectMeta, error) {
	return s.meta(ShardRequest{Op: OpLatestObjectVersion, Bucket: bucket, Key: key})
}

func (s *ShardedStore) ListLatestObjects(bucket, prefix, startAfter string, maxKeys int) ([]ObjectMeta, bool, error) {
	resp, err := s.call(ShardRequest{
		Op: OpListLatestObjects, LeaderOnly: true, Bucket: bucket, Prefix: prefix, StartAfter: startAfter, MaxKeys: maxKeys,
	})
	if err != nil {
		return nil, false, err
	}
	if err := resp.err(); err != nil {
		return nil, false, err
	}
	return resp.Metas, resp.Truncated, nil
}

func (s *ShardedStore) ListLatestObjectsDelimited(bucket, prefix, delimiter, startAfter string, maxKeys int) ([]ObjectMeta, []CommonPrefixInfo, bool, string, error) {
	resp, err := s.call(ShardRequest{
		Op: OpListLatestDelimited, LeaderOnly: true, Bucket: bucket, Prefix: prefix, Delimiter: delimiter,
		StartAfter: startAfter, MaxKeys: maxKeys,
	})
	if err != nil {
		return nil, nil, false, "", err
	}
	if err := resp.err(); err != nil {
		return nil, nil, false, "", err
	}
	return resp.Metas, resp.Prefixes, resp.Truncated, resp.NextMarker, nil
}

func (s *ShardedStore) ListObjectVersions(bucket, prefix, keyMarker, versionMarker string, maxKeys int) ([]ObjectMeta, bool, error) {
	resp, err := s.call(ShardRequest{
		Op: OpListObjectVersions, LeaderOnly: true, Bucket: bucket, Prefix: prefix,
		KeyMarker: keyMarker, VersionMarker: versionMarker, MaxKeys: maxKeys,
	})
	if err != nil {
		return nil, false, err
	}
	if err := resp.err(); err != nil {
		return nil, false, err
	}
	return resp.Metas, resp.Truncated, nil
}

// BucketStats reads the cached size and object count from the shard that holds
// the bucket's objects, since that is where the counters are maintained.
func (s *ShardedStore) BucketStats(bucket string) (BucketStat, bool, error) {
	resp, err := s.call(ShardRequest{Op: OpBucketStats, Bucket: bucket})
	if err != nil {
		return BucketStat{}, false, err
	}
	if err := resp.err(); err != nil {
		return BucketStat{}, false, err
	}
	return resp.Stat, resp.Found, nil
}

// SetBucketStats and BackfillBucketStats repair the counters of the node that
// serves them, exactly as they do on an unsharded cluster where they are also
// unreplicated local repairs. Routing them means they repair a node that holds
// the objects rather than one that holds none.
func (s *ShardedStore) SetBucketStats(bucket string, stat BucketStat) error {
	resp, err := s.call(ShardRequest{Op: OpSetBucketStats, Bucket: bucket, Stat: stat})
	if err != nil {
		return err
	}
	return resp.err()
}

func (s *ShardedStore) BackfillBucketStats(bucket string) (BucketStat, error) {
	resp, err := s.call(ShardRequest{Op: OpBackfillBucketStats, Bucket: bucket})
	if err != nil {
		return BucketStat{}, err
	}
	if err := resp.err(); err != nil {
		return BucketStat{}, err
	}
	return resp.Stat, nil
}

// UpdateLastAccess is best effort and unreplicated, as it is on a single node.
// A shard that cannot be reached simply does not get the touch.
func (s *ShardedStore) UpdateLastAccess(bucket, key string) {
	s.call(ShardRequest{Op: OpUpdateLastAccess, Bucket: bucket, Key: key})
}

// --- Object writes ---

func (s *ShardedStore) PutObjectMeta(meta ObjectMeta) error {
	return s.write(meta.Bucket, cmdPutObjectMeta, meta)
}

func (s *ShardedStore) DeleteObjectMeta(bucket, key string) error {
	return s.write(bucket, cmdDeleteObjectMeta, struct {
		Bucket string
		Key    string
	}{bucket, key})
}

func (s *ShardedStore) SetObjectTier(bucket, key, tier string) error {
	return s.write(bucket, cmdSetObjectTier, struct {
		Bucket string
		Key    string
		Tier   string
	}{bucket, key, tier})
}

func (s *ShardedStore) PutObjectVersion(meta ObjectMeta) error {
	return s.write(meta.Bucket, cmdPutObjectVersion, meta)
}

func (s *ShardedStore) DeleteObjectVersion(bucket, key, versionID string) error {
	return s.write(bucket, cmdDeleteObjectVersion, struct {
		Bucket    string
		Key       string
		VersionID string
	}{bucket, key, versionID})
}

func (s *ShardedStore) SetLatestVersion(bucket, key, versionID string) error {
	return s.write(bucket, cmdSetLatestVersion, struct {
		Bucket    string
		Key       string
		VersionID string
	}{bucket, key, versionID})
}

func (s *ShardedStore) UpdateObjectVersionMeta(meta ObjectMeta) error {
	return s.write(meta.Bucket, cmdUpdateObjectVersionMeta, meta)
}

// DeleteBucketObjectMeta clears a bucket's object metadata from the shard that
// owns it. Deleting the bucket record itself stays a control-group write, so a
// bucket delete is two commits: this one first, then the bucket.
func (s *ShardedStore) DeleteBucketObjectMeta(bucket string) error {
	return s.write(bucket, cmdDeleteBucketObjectMeta, struct{ Bucket string }{bucket})
}

// --- Scans ---
//
// A full scan is partitioned across shard leaders: this node walks the shards it
// leads, and every other shard is walked by whichever node leads it. The union
// across the cluster is every object, visited once, instead of every node
// walking everything.

func (s *ShardedStore) ScanObjects(fn func(ObjectMeta) bool) error {
	for _, h := range s.router.Leading() {
		if err := h.Store().ScanObjects(fn); err != nil {
			return err
		}
	}
	return nil
}

func (s *ShardedStore) ScanObjectVersions(fn func(ObjectMeta) bool) error {
	for _, h := range s.router.Leading() {
		if err := h.Store().ScanObjectVersions(fn); err != nil {
			return err
		}
	}
	return nil
}

func (s *ShardedStore) IterateAllObjects(fn func(bucket, key string, meta ObjectMeta) bool) error {
	for _, h := range s.router.Leading() {
		if err := h.Store().IterateAllObjects(fn); err != nil {
			return err
		}
	}
	return nil
}

// Compile-time check that the sharded store is a drop-in for the others.
var _ StoreAPI = (*ShardedStore)(nil)

// updateLastAccessBatch stamps a flush of last-access times. The records live in
// different Raft groups, so there is no single transaction to put them in: each
// is routed to its own shard, best effort, exactly as a single stamp is.
func (s *ShardedStore) updateLastAccessBatch(stamps map[string]int64) {
	for composite := range stamps {
		bucket, key, ok := splitAccessKey(composite)
		if !ok {
			continue
		}
		s.UpdateLastAccess(bucket, key)
	}
}
