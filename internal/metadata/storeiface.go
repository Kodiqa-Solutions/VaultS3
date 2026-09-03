package metadata

import (
	"io"
	"time"
)

// StoreAPI is the full metadata-store surface used by the API and S3
// handlers. Both *Store (single-node, direct writes) and *DistributedStore
// (clustered, writes via Raft consensus) satisfy it, so handlers depend on
// the interface and the server injects whichever fits the deployment.
//
// Generated from *Store's exported method set — keep in sync when adding
// public Store methods (the compiler enforces that *DistributedStore and any
// handler usage still satisfy it).
type StoreAPI interface {
	AckReplication(id uint64) error
	AppendChangeLog(data []byte) error
	BucketExists(name string) bool
	ChangeLogSeq() (uint64, error)
	Close() error
	CreateAccessKey(key AccessKey) error
	CreateBucket(name string) error
	CreateIAMGroup(group IAMGroup) error
	CreateIAMPolicy(policy IAMPolicy) error
	CreateIAMUser(user IAMUser) error
	CreateMultipartUpload(upload MultipartUpload) error
	DeadLetterReplication(id uint64) error
	DeleteAccessKey(accessKey string) error
	DeleteBucket(name string) error
	DeleteBucketObjectMeta(bucket string) error
	DeleteBucketPolicy(bucket string) error
	DeleteBucketSnapshot(bucket, id string) error
	DeleteBucketTags(bucket string) error
	DeleteCORSConfig(bucket string) error
	DeleteEncryptionConfig(bucket string) error
	DeleteExpiredAccessKeys() (int, error)
	DeleteIAMGroup(name string) error
	DeleteIAMPolicy(name string) error
	DeleteIAMUser(name string) error
	DeleteLambdaConfig(bucket string) error
	DeleteLifecycleRule(bucket string) error
	DeleteLoggingConfig(bucket string) error
	DeleteMultipartUpload(uploadID string) error
	DeleteNotificationConfig(bucket string) error
	DeleteObjectMeta(bucket, key string) error
	DeleteObjectVersion(bucket, key, versionID string) error
	DeletePublicAccessBlock(bucket string) error
	DeleteReplicationConfig(bucket string) error
	DeleteVersionTag(key string) error
	DeleteWebsiteConfig(bucket string) error
	DequeueReplication(limit int, now int64) ([]ReplicationEvent, error)
	EnqueueReplication(event ReplicationEvent) error
	GetAccessKey(accessKey string) (*AccessKey, error)
	GetAdminCredentials() (accessKey, secretKey string, err error)
	GetBucket(name string) (*BucketInfo, error)
	GetBucketPolicy(bucket string) ([]byte, error)
	GetBucketSnapshot(bucket, id string) (*BucketSnapshot, error)
	GetBucketTags(bucket string) (map[string]string, error)
	GetBucketVersioning(bucket string) (string, error)
	GetCORSConfig(bucket string) (*CORSConfig, error)
	GetEncryptionConfig(bucket string) (*BucketEncryptionConfig, error)
	GetIAMGroup(name string) (*IAMGroup, error)
	GetIAMPolicy(name string) (*IAMPolicy, error)
	GetIAMUser(name string) (*IAMUser, error)
	GetLambdaConfig(bucket string) (*BucketLambdaConfig, error)
	GetLifecycleConfig(bucket string) (*LifecycleConfig, error)
	GetLifecycleRule(bucket string) (*LifecycleRule, error)
	GetLoggingConfig(bucket string) (*BucketLoggingConfig, error)
	GetMultipartUpload(uploadID string) (*MultipartUpload, error)
	GetNotificationConfig(bucket string) (*BucketNotificationConfig, error)
	GetObjectMeta(bucket, key string) (*ObjectMeta, error)
	// GetObjectMetaConsistent is GetObjectMeta with a cluster read-your-writes
	// guarantee (barrier-on-miss); identical to GetObjectMeta on a single node.
	// Used by the object GET/HEAD read path (issue #37).
	GetObjectMetaConsistent(bucket, key string) (*ObjectMeta, error)
	GetObjectVersion(bucket, key, versionID string) (*ObjectMeta, error)
	GetPublicAccessBlock(bucket string) (*PublicAccessBlockConfig, error)
	GetReplicationConfig(bucket string) (string, error)
	GetReplicationStatuses() ([]ReplicationStatus, error)
	GetUserPolicies(userName string) ([]IAMPolicy, error)
	GetVersionTag(key string) ([]byte, error)
	GetWebsiteConfig(bucket string) (*WebsiteConfig, error)
	HasPublicReadPolicy(bucket string) bool
	IsBucketPublicList(bucket string) bool
	IsObjectPublicRead(bucket, key string) bool
	IsBucketWebsite(bucket string) bool
	IterateAllObjects(fn func(bucket, key string, meta ObjectMeta) bool) error
	LatestObjectVersion(bucket, key string) (*ObjectMeta, error)
	ListAccessKeys() ([]AccessKey, error)
	ListAuditEntries(limit int, fromUnixNano, toUnixNano int64, user, bucket string) ([]AuditEntry, error)
	ListBackupRecords(limit int) ([]BackupRecord, error)
	ListBucketSnapshots(bucket string) ([]BucketSnapshot, error)
	ListBuckets() ([]BucketInfo, error)
	ListIAMGroups() ([]IAMGroup, error)
	ListIAMPolicies() ([]IAMPolicy, error)
	ListIAMUsers() ([]IAMUser, error)
	ListLambdaConfigs() (map[string]BucketLambdaConfig, error)
	ListLatestObjects(bucket, prefix, startAfter string, maxKeys int) ([]ObjectMeta, bool, error)
	ListMultipartUploads(bucket string) ([]MultipartUpload, error)
	ListNotificationConfigs() (map[string]BucketNotificationConfig, error)
	ListObjectVersions(bucket, prefix, keyMarker, versionMarker string, maxKeys int) ([]ObjectMeta, bool, error)
	ListParts(uploadID string) ([]PartInfo, error)
	ListReplicationQueue(limit int) ([]ReplicationEvent, error)
	ListVersionTags(prefix string) ([][]byte, error)
	NackReplication(id uint64, retryCount int, nextRetryAt int64) error
	PruneAuditEntries(olderThan time.Time) (int, error)
	PutAuditEntry(entry AuditEntry) error
	PutBackupRecord(record BackupRecord) error
	PutBucketPolicy(bucket string, policyJSON []byte) error
	PutBucketSnapshot(snap BucketSnapshot) error
	PutBucketTags(bucket string, tags map[string]string) error
	PutCORSConfig(bucket string, cfg CORSConfig) error
	PutEncryptionConfig(bucket string, cfg BucketEncryptionConfig) error
	PutLambdaConfig(bucket string, cfg BucketLambdaConfig) error
	PutLifecycleConfig(bucket string, cfg LifecycleConfig) error
	PutLifecycleRule(bucket string, rule LifecycleRule) error
	PutLoggingConfig(bucket string, cfg BucketLoggingConfig) error
	PutNotificationConfig(bucket string, cfg BucketNotificationConfig) error
	PutObjectMeta(meta ObjectMeta) error
	PutObjectVersion(meta ObjectMeta) error
	BucketStats(bucket string) (BucketStat, bool, error)
	SetBucketStats(bucket string, stat BucketStat) error
	BackfillBucketStats(bucket string) (BucketStat, error)
	SetBucketObjectLockEnabled(bucket string, enabled bool) error
	ListLatestObjectsDelimited(bucket, prefix, delimiter, startAfter string, maxKeys int) ([]ObjectMeta, []CommonPrefixInfo, bool, string, error)
	PutPart(uploadID string, part PartInfo) error
	PutPublicAccessBlock(bucket string, cfg PublicAccessBlockConfig) error
	PutReplicationConfig(bucket, data string) error
	PutReplicationStatus(status ReplicationStatus) error
	PutVersionTag(key string, data []byte) error
	PutWebsiteConfig(bucket string, cfg WebsiteConfig) error
	ReadChangeLog(sinceSeq uint64, limit int) ([]ChangeLogRawEntry, error)
	ReplicationQueueDepth() (int, error)
	RestoreSnapshot(r io.Reader) error
	ScanObjectVersions(fn func(ObjectMeta) bool) error
	ScanObjects(fn func(ObjectMeta) bool) error
	SetAdminCredentials(accessKey, secretKey string) error
	SetBucketDefaultRetention(bucket, mode string, days int) error
	SetBucketVersioning(bucket, status string) error
	SetLatestVersion(bucket, key, versionID string) error
	SetObjectTier(bucket, key, tier string) error
	TrimChangeLog(beforeSeq uint64) error
	UpdateBucketQuota(name string, maxSizeBytes, maxObjects int64) error
	SetBucketDurability(name string, erasure *bool, replicas *int) error
	BucketDurability(name string, defaultErasure bool, defaultReplicas int) Durability
	UpdateIAMGroup(group IAMGroup) error
	UpdateIAMPolicy(policy IAMPolicy) error
	UpdateIAMUser(user IAMUser) error
	UpdateLastAccess(bucket, key string)
	UpdateObjectVersionMeta(meta ObjectMeta) error
	WriteSnapshot(w io.Writer) error
}

// Compile-time checks that both implementations satisfy StoreAPI.
var (
	_ StoreAPI = (*Store)(nil)
	_ StoreAPI = (*DistributedStore)(nil)
)
