package metadata

import (
	"encoding/json"
	"fmt"
	"time"
)

// RaftApplier is the interface the DistributedStore uses to submit writes.
// Implemented by cluster.Node.
type RaftApplier interface {
	// Apply commits a serialized command to the Raft log. Only valid on the leader.
	Apply(data []byte) error
	IsLeader() bool
	// ForwardToLeader sends an already-serialized command to the current leader
	// to be applied, used when this node is a follower.
	ForwardToLeader(data []byte) error
}

// DistributedStore wraps a Store with Raft consensus for writes.
// Reads go directly to the local store. Writes are serialized as
// commands and submitted through Raft so all nodes apply them.
type DistributedStore struct {
	*Store
	raft RaftApplier
}

func NewDistributedStore(store *Store, raft RaftApplier) *DistributedStore {
	return &DistributedStore{Store: store, raft: raft}
}

// ReadYourWritesTimeout bounds how long a follower waits for a just-written key to
// replicate locally before giving up and reporting not-found. Exported so tests
// can shorten it.
var ReadYourWritesTimeout = 2 * time.Second

// waitReplicated polls present() until it reports true, this node is the leader
// (whose store is authoritative — a miss there is genuinely absent), or the
// timeout elapses. This gives read-your-writes on a follower by waiting for the
// write to arrive through normal Raft replication (which reaches every node),
// with NO inter-node RPC — so it is robust regardless of proxy/network topology
// (issue #37).
func (d *DistributedStore) waitReplicated(present func() bool) {
	if d.raft.IsLeader() {
		return
	}
	deadline := time.Now().Add(ReadYourWritesTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
		if present() {
			return
		}
	}
}

// BucketExists waits on a miss for a bucket created on another node to replicate
// here, so a write right after CreateBucket doesn't spuriously get "Bucket does
// not exist" (issue #37). Only the read path waits; writes stay fast.
func (d *DistributedStore) BucketExists(name string) bool {
	if d.Store.BucketExists(name) {
		return true
	}
	d.waitReplicated(func() bool { return d.Store.BucketExists(name) })
	return d.Store.BucketExists(name)
}

// GetObjectMetaConsistent waits on a miss for a just-written object's metadata to
// replicate to this node, so a GET/HEAD right after a PUT on another node doesn't
// spuriously 404 (issue #37). The WRITE path uses plain GetObjectMeta and stays
// fast; a genuine miss returns not-found after the wait.
func (d *DistributedStore) GetObjectMetaConsistent(bucket, key string) (*ObjectMeta, error) {
	if meta, err := d.Store.GetObjectMeta(bucket, key); meta != nil {
		return meta, err
	}
	d.waitReplicated(func() bool { m, _ := d.Store.GetObjectMeta(bucket, key); return m != nil })
	return d.Store.GetObjectMeta(bucket, key)
}

// Command types — must match cluster.CommandType values.
// We duplicate the constants here to avoid an import cycle.
const (
	cmdCreateBucket             uint16 = 1
	cmdDeleteBucket             uint16 = 2
	cmdPutBucketPolicy          uint16 = 3
	cmdDeleteBucketPolicy       uint16 = 4
	cmdUpdateBucketQuota        uint16 = 5
	cmdPutBucketTags            uint16 = 6
	cmdDeleteBucketTags         uint16 = 7
	cmdDeleteBucketObjectMeta   uint16 = 8
	cmdSetBucketVersioning      uint16 = 9
	cmdSetBucketDefaultRet      uint16 = 10
	cmdPutLifecycleRule         uint16 = 11
	cmdDeleteLifecycleRule      uint16 = 12
	cmdPutWebsiteConfig         uint16 = 13
	cmdDeleteWebsiteConfig      uint16 = 14
	cmdPutCORSConfig            uint16 = 15
	cmdDeleteCORSConfig         uint16 = 16
	cmdPutNotificationConfig    uint16 = 17
	cmdDeleteNotificationConfig uint16 = 18
	cmdPutLambdaConfig          uint16 = 19
	cmdDeleteLambdaConfig       uint16 = 20
	cmdPutEncryptionConfig      uint16 = 21
	cmdDeleteEncryptionConfig   uint16 = 22
	cmdPutPublicAccessBlock     uint16 = 23
	cmdDeletePublicAccessBlock  uint16 = 24
	cmdPutLoggingConfig         uint16 = 25
	cmdDeleteLoggingConfig      uint16 = 26

	cmdPutObjectMeta           uint16 = 27
	cmdDeleteObjectMeta        uint16 = 28
	cmdSetObjectTier           uint16 = 29
	cmdPutObjectVersion        uint16 = 30
	cmdDeleteObjectVersion     uint16 = 31
	cmdSetLatestVersion        uint16 = 32
	cmdUpdateObjectVersionMeta uint16 = 33
	cmdPutVersionTag           uint16 = 34
	cmdDeleteVersionTag        uint16 = 35

	cmdCreateMultipartUpload uint16 = 36
	cmdDeleteMultipartUpload uint16 = 37
	cmdPutPart               uint16 = 38

	cmdCreateAccessKey         uint16 = 39
	cmdDeleteAccessKey         uint16 = 40
	cmdDeleteExpiredAccessKeys uint16 = 41

	cmdCreateIAMUser   uint16 = 42
	cmdUpdateIAMUser   uint16 = 43
	cmdDeleteIAMUser   uint16 = 44
	cmdCreateIAMGroup  uint16 = 45
	cmdUpdateIAMGroup  uint16 = 46
	cmdDeleteIAMGroup  uint16 = 47
	cmdCreateIAMPolicy uint16 = 48
	cmdUpdateIAMPolicy uint16 = 49
	cmdDeleteIAMPolicy uint16 = 50

	cmdPutAuditEntry     uint16 = 51
	cmdPruneAuditEntries uint16 = 52

	cmdEnqueueReplication    uint16 = 53
	cmdAckReplication        uint16 = 54
	cmdNackReplication       uint16 = 55
	cmdDeadLetterReplication uint16 = 56
	cmdPutReplicationStatus  uint16 = 57

	cmdPutBackupRecord uint16 = 58

	cmdSetBucketDurability uint16 = 59
)

type raftCommand struct {
	Type uint16          `json:"t"`
	Data json.RawMessage `json:"d"`
}

// marshalRaftCommand serializes one metadata command in the wire format the
// cluster FSM decodes. Shared with the ShardedStore, which sends the same
// commands to a shard group instead of the control group.
func marshalRaftCommand(cmdType uint16, payload interface{}) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal command payload: %w", err)
	}
	cmdData, err := json.Marshal(raftCommand{Type: cmdType, Data: data})
	if err != nil {
		return nil, fmt.Errorf("marshal command: %w", err)
	}
	return cmdData, nil
}

func (d *DistributedStore) apply(cmdType uint16, payload interface{}) error {
	cmdData, err := marshalRaftCommand(cmdType, payload)
	if err != nil {
		return err
	}
	// Writes must be committed on the leader. If this node is a follower, forward
	// the command to the leader rather than failing — so a client can write to
	// any node in the cluster.
	if d.raft.IsLeader() {
		return d.raft.Apply(cmdData)
	}
	return d.raft.ForwardToLeader(cmdData)
}

// --- Bucket operations (override Store methods to go through Raft) ---

func (d *DistributedStore) CreateBucket(name string) error {
	return d.apply(cmdCreateBucket, struct{ Name string }{name})
}

func (d *DistributedStore) DeleteBucket(name string) error {
	return d.apply(cmdDeleteBucket, struct{ Name string }{name})
}

func (d *DistributedStore) PutBucketPolicy(bucket string, policyJSON []byte) error {
	return d.apply(cmdPutBucketPolicy, struct {
		Bucket string
		Policy []byte
	}{bucket, policyJSON})
}

func (d *DistributedStore) DeleteBucketPolicy(bucket string) error {
	return d.apply(cmdDeleteBucketPolicy, struct{ Bucket string }{bucket})
}

// SetBucketDurability replicates a bucket's erasure/replica overrides, so every
// node makes the same placement decision for that bucket (issue #39).
func (d *DistributedStore) SetBucketDurability(name string, erasure *bool, replicas *int) error {
	return d.apply(cmdSetBucketDurability, struct {
		Name           string
		ErasureEnabled *bool
		ReplicaCount   *int
	}{name, erasure, replicas})
}

func (d *DistributedStore) UpdateBucketQuota(name string, maxSizeBytes, maxObjects int64) error {
	return d.apply(cmdUpdateBucketQuota, struct {
		Name         string
		MaxSizeBytes int64
		MaxObjects   int64
	}{name, maxSizeBytes, maxObjects})
}

func (d *DistributedStore) PutBucketTags(bucket string, tags map[string]string) error {
	return d.apply(cmdPutBucketTags, struct {
		Bucket string
		Tags   map[string]string
	}{bucket, tags})
}

func (d *DistributedStore) DeleteBucketTags(bucket string) error {
	return d.apply(cmdDeleteBucketTags, struct{ Bucket string }{bucket})
}

func (d *DistributedStore) DeleteBucketObjectMeta(bucket string) error {
	return d.apply(cmdDeleteBucketObjectMeta, struct{ Bucket string }{bucket})
}

func (d *DistributedStore) SetBucketVersioning(bucket, status string) error {
	return d.apply(cmdSetBucketVersioning, struct {
		Bucket string
		Status string
	}{bucket, status})
}

func (d *DistributedStore) SetBucketDefaultRetention(bucket, mode string, days int) error {
	return d.apply(cmdSetBucketDefaultRet, struct {
		Bucket string
		Mode   string
		Days   int
	}{bucket, mode, days})
}

func (d *DistributedStore) PutLifecycleRule(bucket string, rule LifecycleRule) error {
	return d.apply(cmdPutLifecycleRule, struct {
		Bucket string
		Rule   LifecycleRule
	}{bucket, rule})
}

func (d *DistributedStore) DeleteLifecycleRule(bucket string) error {
	return d.apply(cmdDeleteLifecycleRule, struct{ Bucket string }{bucket})
}

func (d *DistributedStore) PutWebsiteConfig(bucket string, cfg WebsiteConfig) error {
	return d.apply(cmdPutWebsiteConfig, struct {
		Bucket string
		Config WebsiteConfig
	}{bucket, cfg})
}

func (d *DistributedStore) DeleteWebsiteConfig(bucket string) error {
	return d.apply(cmdDeleteWebsiteConfig, struct{ Bucket string }{bucket})
}

func (d *DistributedStore) PutCORSConfig(bucket string, cfg CORSConfig) error {
	return d.apply(cmdPutCORSConfig, struct {
		Bucket string
		Config CORSConfig
	}{bucket, cfg})
}

func (d *DistributedStore) DeleteCORSConfig(bucket string) error {
	return d.apply(cmdDeleteCORSConfig, struct{ Bucket string }{bucket})
}

func (d *DistributedStore) PutNotificationConfig(bucket string, cfg BucketNotificationConfig) error {
	return d.apply(cmdPutNotificationConfig, struct {
		Bucket string
		Config BucketNotificationConfig
	}{bucket, cfg})
}

func (d *DistributedStore) DeleteNotificationConfig(bucket string) error {
	return d.apply(cmdDeleteNotificationConfig, struct{ Bucket string }{bucket})
}

func (d *DistributedStore) PutLambdaConfig(bucket string, cfg BucketLambdaConfig) error {
	return d.apply(cmdPutLambdaConfig, struct {
		Bucket string
		Config BucketLambdaConfig
	}{bucket, cfg})
}

func (d *DistributedStore) DeleteLambdaConfig(bucket string) error {
	return d.apply(cmdDeleteLambdaConfig, struct{ Bucket string }{bucket})
}

func (d *DistributedStore) PutEncryptionConfig(bucket string, cfg BucketEncryptionConfig) error {
	return d.apply(cmdPutEncryptionConfig, struct {
		Bucket string
		Config BucketEncryptionConfig
	}{bucket, cfg})
}

func (d *DistributedStore) DeleteEncryptionConfig(bucket string) error {
	return d.apply(cmdDeleteEncryptionConfig, struct{ Bucket string }{bucket})
}

func (d *DistributedStore) PutPublicAccessBlock(bucket string, cfg PublicAccessBlockConfig) error {
	return d.apply(cmdPutPublicAccessBlock, struct {
		Bucket string
		Config PublicAccessBlockConfig
	}{bucket, cfg})
}

func (d *DistributedStore) DeletePublicAccessBlock(bucket string) error {
	return d.apply(cmdDeletePublicAccessBlock, struct{ Bucket string }{bucket})
}

func (d *DistributedStore) PutLoggingConfig(bucket string, cfg BucketLoggingConfig) error {
	return d.apply(cmdPutLoggingConfig, struct {
		Bucket string
		Config BucketLoggingConfig
	}{bucket, cfg})
}

func (d *DistributedStore) DeleteLoggingConfig(bucket string) error {
	return d.apply(cmdDeleteLoggingConfig, struct{ Bucket string }{bucket})
}

// --- Object operations ---

func (d *DistributedStore) PutObjectMeta(meta ObjectMeta) error {
	return d.apply(cmdPutObjectMeta, meta)
}

func (d *DistributedStore) DeleteObjectMeta(bucket, key string) error {
	return d.apply(cmdDeleteObjectMeta, struct {
		Bucket string
		Key    string
	}{bucket, key})
}

func (d *DistributedStore) SetObjectTier(bucket, key, tier string) error {
	return d.apply(cmdSetObjectTier, struct {
		Bucket string
		Key    string
		Tier   string
	}{bucket, key, tier})
}

func (d *DistributedStore) PutObjectVersion(meta ObjectMeta) error {
	return d.apply(cmdPutObjectVersion, meta)
}

func (d *DistributedStore) DeleteObjectVersion(bucket, key, versionID string) error {
	return d.apply(cmdDeleteObjectVersion, struct {
		Bucket    string
		Key       string
		VersionID string
	}{bucket, key, versionID})
}

func (d *DistributedStore) SetLatestVersion(bucket, key, versionID string) error {
	return d.apply(cmdSetLatestVersion, struct {
		Bucket    string
		Key       string
		VersionID string
	}{bucket, key, versionID})
}

func (d *DistributedStore) UpdateObjectVersionMeta(meta ObjectMeta) error {
	return d.apply(cmdUpdateObjectVersionMeta, meta)
}

func (d *DistributedStore) PutVersionTag(key string, data []byte) error {
	return d.apply(cmdPutVersionTag, struct {
		Key  string
		Data []byte
	}{key, data})
}

func (d *DistributedStore) DeleteVersionTag(key string) error {
	return d.apply(cmdDeleteVersionTag, struct{ Key string }{key})
}

// --- Multipart upload operations ---

func (d *DistributedStore) CreateMultipartUpload(upload MultipartUpload) error {
	return d.apply(cmdCreateMultipartUpload, upload)
}

func (d *DistributedStore) DeleteMultipartUpload(uploadID string) error {
	return d.apply(cmdDeleteMultipartUpload, struct{ UploadID string }{uploadID})
}

func (d *DistributedStore) PutPart(uploadID string, part PartInfo) error {
	return d.apply(cmdPutPart, struct {
		UploadID string
		Part     PartInfo
	}{uploadID, part})
}

// --- Access key operations ---

func (d *DistributedStore) CreateAccessKey(key AccessKey) error {
	return d.apply(cmdCreateAccessKey, key)
}

func (d *DistributedStore) DeleteAccessKey(accessKey string) error {
	return d.apply(cmdDeleteAccessKey, struct{ AccessKey string }{accessKey})
}

func (d *DistributedStore) DeleteExpiredAccessKeys() (int, error) {
	err := d.apply(cmdDeleteExpiredAccessKeys, struct{}{})
	return 0, err // count not available through Raft
}

// --- IAM operations ---

func (d *DistributedStore) CreateIAMUser(user IAMUser) error {
	return d.apply(cmdCreateIAMUser, user)
}

func (d *DistributedStore) UpdateIAMUser(user IAMUser) error {
	return d.apply(cmdUpdateIAMUser, user)
}

func (d *DistributedStore) DeleteIAMUser(name string) error {
	return d.apply(cmdDeleteIAMUser, struct{ Name string }{name})
}

func (d *DistributedStore) CreateIAMGroup(group IAMGroup) error {
	return d.apply(cmdCreateIAMGroup, group)
}

func (d *DistributedStore) UpdateIAMGroup(group IAMGroup) error {
	return d.apply(cmdUpdateIAMGroup, group)
}

func (d *DistributedStore) DeleteIAMGroup(name string) error {
	return d.apply(cmdDeleteIAMGroup, struct{ Name string }{name})
}

func (d *DistributedStore) CreateIAMPolicy(policy IAMPolicy) error {
	return d.apply(cmdCreateIAMPolicy, policy)
}

func (d *DistributedStore) UpdateIAMPolicy(policy IAMPolicy) error {
	return d.apply(cmdUpdateIAMPolicy, policy)
}

func (d *DistributedStore) DeleteIAMPolicy(name string) error {
	return d.apply(cmdDeleteIAMPolicy, struct{ Name string }{name})
}

// --- Audit operations ---

func (d *DistributedStore) PutAuditEntry(entry AuditEntry) error {
	return d.apply(cmdPutAuditEntry, entry)
}

func (d *DistributedStore) PruneAuditEntries(olderThan time.Time) (int, error) {
	err := d.apply(cmdPruneAuditEntries, struct{ OlderThan int64 }{olderThan.UnixNano()})
	return 0, err
}

// --- Replication operations ---

func (d *DistributedStore) EnqueueReplication(event ReplicationEvent) error {
	return d.apply(cmdEnqueueReplication, event)
}

func (d *DistributedStore) AckReplication(id uint64) error {
	return d.apply(cmdAckReplication, struct{ ID uint64 }{id})
}

func (d *DistributedStore) NackReplication(id uint64, retryCount int, nextRetryAt int64) error {
	return d.apply(cmdNackReplication, struct {
		ID          uint64
		RetryCount  int
		NextRetryAt int64
	}{id, retryCount, nextRetryAt})
}

func (d *DistributedStore) DeadLetterReplication(id uint64) error {
	return d.apply(cmdDeadLetterReplication, struct{ ID uint64 }{id})
}

func (d *DistributedStore) PutReplicationStatus(status ReplicationStatus) error {
	return d.apply(cmdPutReplicationStatus, status)
}

// --- Backup operations ---

func (d *DistributedStore) PutBackupRecord(record BackupRecord) error {
	return d.apply(cmdPutBackupRecord, record)
}
