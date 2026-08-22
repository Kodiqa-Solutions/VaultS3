package cluster

import "encoding/json"

// CommandType identifies the metadata store operation to apply.
type CommandType uint16

const (
	// Bucket operations
	CmdCreateBucket CommandType = iota + 1
	CmdDeleteBucket
	CmdPutBucketPolicy
	CmdDeleteBucketPolicy
	CmdUpdateBucketQuota
	CmdPutBucketTags
	CmdDeleteBucketTags
	CmdDeleteBucketObjectMeta
	CmdSetBucketVersioning
	CmdSetBucketDefaultRetention
	CmdPutLifecycleRule
	CmdDeleteLifecycleRule
	CmdPutWebsiteConfig
	CmdDeleteWebsiteConfig
	CmdPutCORSConfig
	CmdDeleteCORSConfig
	CmdPutNotificationConfig
	CmdDeleteNotificationConfig
	CmdPutLambdaConfig
	CmdDeleteLambdaConfig
	CmdPutEncryptionConfig
	CmdDeleteEncryptionConfig
	CmdPutPublicAccessBlock
	CmdDeletePublicAccessBlock
	CmdPutLoggingConfig
	CmdDeleteLoggingConfig

	// Object operations
	CmdPutObjectMeta
	CmdDeleteObjectMeta
	CmdSetObjectTier
	CmdPutObjectVersion
	CmdDeleteObjectVersion
	CmdSetLatestVersion
	CmdUpdateObjectVersionMeta
	CmdPutVersionTag
	CmdDeleteVersionTag

	// Multipart upload operations
	CmdCreateMultipartUpload
	CmdDeleteMultipartUpload
	CmdPutPart

	// Access key operations
	CmdCreateAccessKey
	CmdDeleteAccessKey
	CmdDeleteExpiredAccessKeys

	// IAM operations
	CmdCreateIAMUser
	CmdUpdateIAMUser
	CmdDeleteIAMUser
	CmdCreateIAMGroup
	CmdUpdateIAMGroup
	CmdDeleteIAMGroup
	CmdCreateIAMPolicy
	CmdUpdateIAMPolicy
	CmdDeleteIAMPolicy

	// Audit operations
	CmdPutAuditEntry
	CmdPruneAuditEntries

	// Replication operations
	CmdEnqueueReplication
	CmdAckReplication
	CmdNackReplication
	CmdDeadLetterReplication
	CmdPutReplicationStatus

	// Backup operations
	CmdPutBackupRecord

	// Per-bucket durability overrides (erasure coding, replica count)
	CmdSetBucketDurability

	// Metadata shard assignment (issue #50)
	CmdPutShardMap
)

// Command is the serialized Raft log entry.
type Command struct {
	Type CommandType     `json:"t"`
	Data json.RawMessage `json:"d"`
}

func marshalCommand(cmdType CommandType, payload interface{}) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Command{Type: cmdType, Data: data})
}
