package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
)

// FSM implements raft.FSM using the metadata.Store as the state machine.
type FSM struct {
	store        *metadata.Store
	appliedIndex atomic.Uint64 // last log index whose store mutation has completed (issue #37)
	// objectsOnly marks a metadata shard's state machine. A shard group holds
	// object metadata and nothing else, so a command outside that set arriving
	// here means a routing bug, and applying it would put a cluster-wide record
	// (a bucket, an IAM user) into one shard where no other node can see it.
	// Refusing is loud and reversible; applying is silent and is not (issue #50).
	objectsOnly bool
}

func NewFSM(store *metadata.Store) *FSM {
	return &FSM{store: store}
}

// NewShardFSM builds the state machine of a metadata shard group.
func NewShardFSM(store *metadata.Store) *FSM {
	return &FSM{store: store, objectsOnly: true}
}

// isObjectCommand reports whether a command belongs in a metadata shard.
func isObjectCommand(t CommandType) bool {
	switch t {
	case CmdPutObjectMeta, CmdDeleteObjectMeta, CmdSetObjectTier,
		CmdPutObjectVersion, CmdDeleteObjectVersion, CmdSetLatestVersion,
		CmdUpdateObjectVersionMeta, CmdDeleteBucketObjectMeta:
		return true
	}
	return false
}

// AppliedIndex returns the last log index this FSM has finished applying to the
// store. Unlike raft.AppliedIndex() (which advances when an entry is dispatched
// to the FSM goroutine), this only moves AFTER the store mutation completes, so a
// caller can wait on it for read-your-writes (issue #37).
func (f *FSM) AppliedIndex() uint64 { return f.appliedIndex.Load() }

// Apply is called by Raft when a log entry is committed.
func (f *FSM) Apply(log *raft.Log) interface{} {
	// Record the index only after the store mutation below has run, so a reader
	// waiting on AppliedIndex() is guaranteed to see this entry's effect (#37).
	defer f.appliedIndex.Store(log.Index)
	var cmd Command
	if err := json.Unmarshal(log.Data, &cmd); err != nil {
		slog.Error("fsm: failed to unmarshal command", "index", log.Index, "error", err)
		return fmt.Errorf("unmarshal command: %w", err)
	}
	resp := f.applyCommand(cmd, log)
	reportApplyResult(cmd.Type, log.Index, resp)
	return resp
}

// reportApplyResult logs a command the state machine did not apply cleanly.
//
// Raft hands an apply's result only to the future of the node that proposed the
// entry, which is the leader. On every other node it went nowhere, so a follower
// whose bolt write failed (a full disk, an I/O error) still advanced its applied
// index past the entry and carried on without the record, permanently, and
// nothing anywhere said so. Logging here is the only place that failure can be
// seen.
//
// Results that are an answer rather than a failure, a bucket that already exists
// or a key that is not there, are what the leader returns to the client, and are
// logged at debug so a busy cluster does not log every one of them per node.
func reportApplyResult(t CommandType, index uint64, resp interface{}) {
	err, ok := resp.(error)
	if !ok || err == nil {
		return
	}
	if isSemanticApplyError(err) {
		slog.Debug("fsm: command not applied", "type", t, "index", index, "reason", err)
		return
	}
	slog.Error("fsm: apply failed, this node does not hold the record the cluster committed",
		"type", t, "index", index, "error", err)
}

// isSemanticApplyError reports whether an apply error describes the request
// (the thing it names is missing or already there) rather than a failure of
// this node's store.
func isSemanticApplyError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"not found", "already exists", "does not exist", "no such"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// ApplyBatch applies a batch of committed entries, satisfying raft.BatchingFSM.
// Raft hands the FSM up to MaxAppendEntries (64) committed entries at once;
// applying each in its own BoltDB transaction cost one fsync per object on every
// node, which is what capped clustered ingest (issue #50). Consecutive object
// metadata writes are coalesced into a single transaction instead.
//
// Only RUNS of consecutive object writes are merged, so log order is preserved
// exactly: any other command breaks the run and applies on its own.
func (f *FSM) ApplyBatch(logs []*raft.Log) []interface{} {
	if len(logs) == 0 {
		return nil
	}
	// The last index is recorded only after every mutation below has run, so a
	// reader waiting on AppliedIndex() sees the whole batch's effect (#37).
	defer f.appliedIndex.Store(logs[len(logs)-1].Index)

	resps := make([]interface{}, len(logs))
	cmds := make([]*Command, len(logs))
	// batchable[i] holds the decoded record when entry i can join a coalesced
	// run. Decoding happens here, once, so the pass below only has to group.
	batchable := make([]*metadata.ObjectMeta, len(logs))
	for i, l := range logs {
		// Raft also sends configuration entries; this FSM has no state for them.
		if l.Type != raft.LogCommand {
			continue
		}
		var cmd Command
		if err := json.Unmarshal(l.Data, &cmd); err != nil {
			slog.Error("fsm: failed to unmarshal command", "index", l.Index, "error", err)
			resps[i] = fmt.Errorf("unmarshal command: %w", err)
			continue
		}
		cmds[i] = &cmd
		if cmd.Type != CmdPutObjectMeta {
			continue
		}
		var m metadata.ObjectMeta
		if err := json.Unmarshal(cmd.Data, &m); err != nil {
			// Leave it unbatchable: applyCommand reports the same error below.
			continue
		}
		batchable[i] = &m
	}

	for i := 0; i < len(logs); {
		if batchable[i] == nil {
			if cmds[i] != nil {
				resps[i] = f.applyCommand(*cmds[i], logs[i])
				reportApplyResult(cmds[i].Type, logs[i].Index, resps[i])
			}
			i++
			continue
		}
		// Gather the run of consecutive object-metadata writes.
		j := i
		metas := make([]metadata.ObjectMeta, 0, len(logs)-i)
		for ; j < len(logs) && batchable[j] != nil; j++ {
			metas = append(metas, *batchable[j])
		}
		if err := f.store.PutObjectMetaBatch(metas); err != nil {
			// The batch is atomic, so a failure discarded records Raft has already
			// committed. Replay them one at a time: each then either applies or is
			// reported individually, and none is silently dropped.
			slog.Warn("fsm: batched metadata write failed, applying individually",
				"error", err, "count", len(metas))
			for k := range metas {
				resps[i+k] = f.store.PutObjectMeta(metas[k])
				reportApplyResult(CmdPutObjectMeta, logs[i+k].Index, resps[i+k])
			}
		}
		i = j
	}
	return resps
}

// applyShardMap commits a metadata shard assignment, refusing any change to the
// parts of it that must never move (issue #50).
//
// The check runs here, in the state machine, rather than at the proposing node,
// so every node reaches the same verdict from the same committed log. A map whose
// shard count, epoch or founding sets differed from the committed one would leave
// members of a shard disagreeing about which Raft group they belong to, and two
// groups serving one shard is unrecoverable: both would be authoritative for
// metadata that the other cannot see.
func (f *FSM) applyShardMap(data []byte) interface{} {
	var next ShardMap
	if err := json.Unmarshal(data, &next); err != nil {
		return fmt.Errorf("shard map: decode proposed map: %w", err)
	}
	current, err := f.currentShardMap()
	if err != nil {
		return err
	}
	if err := ValidateSuccession(current, &next); err != nil {
		return err
	}
	// Re-encode rather than storing the caller's bytes, so what is persisted is
	// exactly what was validated.
	encoded, err := json.Marshal(&next)
	if err != nil {
		return fmt.Errorf("shard map: encode: %w", err)
	}
	if err := f.store.PutShardMap(encoded); err != nil {
		return err
	}
	slog.Info("cluster: shard map committed",
		"version", next.Version, "epoch", next.Epoch, "shards", next.Shards, "replicas", next.Replicas)
	return nil
}

// currentShardMap reads the committed assignment, or nil when there is none.
func (f *FSM) currentShardMap() (*ShardMap, error) {
	raw, err := f.store.GetShardMap()
	if err != nil {
		return nil, fmt.Errorf("shard map: read committed map: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var m ShardMap
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("shard map: decode committed map: %w", err)
	}
	return &m, nil
}

// Snapshot captures the current state for Raft snapshotting.
//
// Raft calls this on the goroutine that applies entries and records the last
// applied index as the snapshot's index, then writes the snapshot out on another
// goroutine while applying carries on. The read transaction is therefore opened
// here, so what is written is the state at that index and not whatever the
// store holds by the time the write gets to it.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	sn, err := f.store.BeginRaftSnapshot()
	if err != nil {
		return nil, err
	}
	return &fsmSnapshot{snap: sn}, nil
}

// Restore replaces the store state from a snapshot.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	return f.store.RestoreSnapshot(rc)
}

// applyTime is the time a command was proposed at, for commands that carry one,
// and otherwise the time the leader appended the entry. Both are the same on
// every node and on every replay, which the local clock is not. The clock is the
// last resort, for an entry that has neither.
func applyTime(proposed int64, l *raft.Log) time.Time {
	if proposed != 0 {
		return time.Unix(0, proposed)
	}
	if l != nil && !l.AppendedAt.IsZero() {
		return l.AppendedAt
	}
	return time.Now()
}

func (f *FSM) applyCommand(cmd Command, l *raft.Log) interface{} {
	if f.objectsOnly && !isObjectCommand(cmd.Type) {
		return fmt.Errorf("cluster: command type %d does not belong in a metadata shard", cmd.Type)
	}
	switch cmd.Type {

	// --- Bucket operations ---
	case CmdCreateBucket:
		var p struct {
			Name      string
			CreatedAt int64 // unix nano, set by the proposer
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.CreateBucketAt(p.Name, applyTime(p.CreatedAt, l))

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

	case CmdSetBucketDurability:
		var p struct {
			Name           string
			ErasureEnabled *bool
			ReplicaCount   *int
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return f.store.SetBucketDurability(p.Name, p.ErasureEnabled, p.ReplicaCount)

	case CmdPutShardMap:
		return f.applyShardMap(cmd.Data)

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
		var p struct{ Now int64 } // unix seconds, set by the proposer
		if len(cmd.Data) > 0 {
			if err := json.Unmarshal(cmd.Data, &p); err != nil {
				return err
			}
		}
		now := p.Now
		if now == 0 {
			now = applyTime(0, l).Unix()
		}
		_, err := f.store.DeleteExpiredAccessKeysAt(now)
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
		if p.CreatedAt == 0 {
			p.CreatedAt = applyTime(0, l).Unix()
		}
		// The event's ID is the index of the log entry that enqueued it. That is
		// unique, identical on every node, and the same when the entry is applied
		// again after a restart, so a replay rewrites the event instead of adding a
		// second copy under a fresh number. An ID chosen by the proposer would need
		// a counter that survives leader changes, which the log index already is.
		if l == nil || l.Index == 0 {
			return f.store.EnqueueReplication(p)
		}
		return f.store.EnqueueReplicationWithID(p, l.Index)

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
	snap *metadata.RaftSnapshot
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	// Release as soon as the bytes are out rather than waiting for Raft to call
	// Release, since the open transaction holds up any write that needs the file
	// to grow.
	defer s.snap.Release()
	if err := s.snap.Encode(sink); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() { s.snap.Release() }
