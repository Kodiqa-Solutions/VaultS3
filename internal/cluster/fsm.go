package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
)

// FSM implements raft.FSM using the metadata.Store as the state machine.
type FSM struct {
	store *metadata.Store
}

func NewFSM(store *metadata.Store) *FSM {
	return &FSM{store: store}
}

// Apply is called by Raft when a log entry is committed.
func (f *FSM) Apply(log *raft.Log) interface{} {
	var cmd Command
	if err := json.Unmarshal(log.Data, &cmd); err != nil {
		slog.Error("fsm: failed to unmarshal command", "error", err)
		return fmt.Errorf("unmarshal command: %w", err)
	}
	return f.applyCommand(cmd)
}

// Snapshot returns a snapshot of the current state for Raft snapshotting.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	return &fsmSnapshot{store: f.store}, nil
}

// Restore replaces the store state from a snapshot.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	return f.store.RestoreSnapshot(rc)
}

func (f *FSM) applyCommand(cmd Command) interface{} {
	switch cmd.Type {

	// --- Bucket operations ---
	case CmdCreateBucket:
		var p struct{ Name string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.CreateBucket(p.Name)

	case CmdDeleteBucket:
		var p struct{ Name string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteBucket(p.Name)

	case CmdPutBucketPolicy:
		var p struct {
			Bucket string
			Policy []byte
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutBucketPolicy(p.Bucket, p.Policy)

	case CmdDeleteBucketPolicy:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteBucketPolicy(p.Bucket)

	case CmdUpdateBucketQuota:
		var p struct {
			Name         string
			MaxSizeBytes int64
			MaxObjects   int64
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.UpdateBucketQuota(p.Name, p.MaxSizeBytes, p.MaxObjects)

	case CmdPutBucketTags:
		var p struct {
			Bucket string
			Tags   map[string]string
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutBucketTags(p.Bucket, p.Tags)

	case CmdDeleteBucketTags:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteBucketTags(p.Bucket)

	case CmdDeleteBucketObjectMeta:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteBucketObjectMeta(p.Bucket)

	case CmdSetBucketVersioning:
		var p struct {
			Bucket string
			Status string
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.SetBucketVersioning(p.Bucket, p.Status)

	case CmdSetBucketDefaultRetention:
		var p struct {
			Bucket string
			Mode   string
			Days   int
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.SetBucketDefaultRetention(p.Bucket, p.Mode, p.Days)

	case CmdPutLifecycleRule:
		var p struct {
			Bucket string
			Rule   metadata.LifecycleRule
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutLifecycleRule(p.Bucket, p.Rule)

	case CmdDeleteLifecycleRule:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteLifecycleRule(p.Bucket)

	case CmdPutWebsiteConfig:
		var p struct {
			Bucket string
			Config metadata.WebsiteConfig
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutWebsiteConfig(p.Bucket, p.Config)

	case CmdDeleteWebsiteConfig:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteWebsiteConfig(p.Bucket)

	case CmdPutCORSConfig:
		var p struct {
			Bucket string
			Config metadata.CORSConfig
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutCORSConfig(p.Bucket, p.Config)

	case CmdDeleteCORSConfig:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteCORSConfig(p.Bucket)

	case CmdPutNotificationConfig:
		var p struct {
			Bucket string
			Config metadata.BucketNotificationConfig
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutNotificationConfig(p.Bucket, p.Config)

	case CmdDeleteNotificationConfig:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteNotificationConfig(p.Bucket)

	case CmdPutLambdaConfig:
		var p struct {
			Bucket string
			Config metadata.BucketLambdaConfig
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutLambdaConfig(p.Bucket, p.Config)

	case CmdDeleteLambdaConfig:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteLambdaConfig(p.Bucket)

	case CmdPutEncryptionConfig:
		var p struct {
			Bucket string
			Config metadata.BucketEncryptionConfig
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutEncryptionConfig(p.Bucket, p.Config)

	case CmdDeleteEncryptionConfig:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteEncryptionConfig(p.Bucket)

	case CmdPutPublicAccessBlock:
		var p struct {
			Bucket string
			Config metadata.PublicAccessBlockConfig
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutPublicAccessBlock(p.Bucket, p.Config)

	case CmdDeletePublicAccessBlock:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeletePublicAccessBlock(p.Bucket)

	case CmdPutLoggingConfig:
		var p struct {
			Bucket string
			Config metadata.BucketLoggingConfig
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutLoggingConfig(p.Bucket, p.Config)

	case CmdDeleteLoggingConfig:
		var p struct{ Bucket string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteLoggingConfig(p.Bucket)

	// --- Object operations ---
	case CmdPutObjectMeta:
		var p metadata.ObjectMeta
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutObjectMeta(p)

	case CmdDeleteObjectMeta:
		var p struct {
			Bucket string
			Key    string
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteObjectMeta(p.Bucket, p.Key)

	case CmdSetObjectTier:
		var p struct {
			Bucket string
			Key    string
			Tier   string
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.SetObjectTier(p.Bucket, p.Key, p.Tier)

	case CmdPutObjectVersion:
		var p metadata.ObjectMeta
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutObjectVersion(p)

	case CmdDeleteObjectVersion:
		var p struct {
			Bucket    string
			Key       string
			VersionID string
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteObjectVersion(p.Bucket, p.Key, p.VersionID)

	case CmdSetLatestVersion:
		var p struct {
			Bucket    string
			Key       string
			VersionID string
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.SetLatestVersion(p.Bucket, p.Key, p.VersionID)

	case CmdUpdateObjectVersionMeta:
		var p metadata.ObjectMeta
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.UpdateObjectVersionMeta(p)

	case CmdPutVersionTag:
		var p struct {
			Key  string
			Data []byte
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutVersionTag(p.Key, p.Data)

	case CmdDeleteVersionTag:
		var p struct{ Key string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteVersionTag(p.Key)

	// --- Multipart upload operations ---
	case CmdCreateMultipartUpload:
		var p metadata.MultipartUpload
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.CreateMultipartUpload(p)

	case CmdDeleteMultipartUpload:
		var p struct{ UploadID string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteMultipartUpload(p.UploadID)

	case CmdPutPart:
		var p struct {
			UploadID string
			Part     metadata.PartInfo
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutPart(p.UploadID, p.Part)

	// --- Access key operations ---
	case CmdCreateAccessKey:
		var p metadata.AccessKey
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.CreateAccessKey(p)

	case CmdDeleteAccessKey:
		var p struct{ AccessKey string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteAccessKey(p.AccessKey)

	case CmdDeleteExpiredAccessKeys:
		_, err := f.store.DeleteExpiredAccessKeys()
		return err

	// --- IAM operations ---
	case CmdCreateIAMUser:
		var p metadata.IAMUser
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.CreateIAMUser(p)

	case CmdUpdateIAMUser:
		var p metadata.IAMUser
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.UpdateIAMUser(p)

	case CmdDeleteIAMUser:
		var p struct{ Name string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteIAMUser(p.Name)

	case CmdCreateIAMGroup:
		var p metadata.IAMGroup
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.CreateIAMGroup(p)

	case CmdUpdateIAMGroup:
		var p metadata.IAMGroup
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.UpdateIAMGroup(p)

	case CmdDeleteIAMGroup:
		var p struct{ Name string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteIAMGroup(p.Name)

	case CmdCreateIAMPolicy:
		var p metadata.IAMPolicy
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.CreateIAMPolicy(p)

	case CmdUpdateIAMPolicy:
		var p metadata.IAMPolicy
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.UpdateIAMPolicy(p)

	case CmdDeleteIAMPolicy:
		var p struct{ Name string }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeleteIAMPolicy(p.Name)

	// --- Audit operations ---
	case CmdPutAuditEntry:
		var p metadata.AuditEntry
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutAuditEntry(p)

	case CmdPruneAuditEntries:
		var p struct{ OlderThan int64 } // unix nano
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		_, err := f.store.PruneAuditEntries(time.Unix(0, p.OlderThan))
		return err

	// --- Replication operations ---
	case CmdEnqueueReplication:
		var p metadata.ReplicationEvent
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.EnqueueReplication(p)

	case CmdAckReplication:
		var p struct{ ID uint64 }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.AckReplication(p.ID)

	case CmdNackReplication:
		var p struct {
			ID          uint64
			RetryCount  int
			NextRetryAt int64
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.NackReplication(p.ID, p.RetryCount, p.NextRetryAt)

	case CmdDeadLetterReplication:
		var p struct{ ID uint64 }
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.DeadLetterReplication(p.ID)

	case CmdPutReplicationStatus:
		var p metadata.ReplicationStatus
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutReplicationStatus(p)

	// --- Backup operations ---
	case CmdPutBackupRecord:
		var p metadata.BackupRecord
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.PutBackupRecord(p)

	default:
		return fmt.Errorf("unknown command type: %d", cmd.Type)
	}
}

// fsmSnapshot implements raft.FSMSnapshot.
type fsmSnapshot struct {
	store *metadata.Store
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := s.store.WriteSnapshot(sink); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
