package metadata

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Which bolt buckets belong to Raft.
//
// The Raft state machine and the node's own records share one bbolt file. A
// Raft snapshot must carry only the first kind, and installing one must replace
// only the first kind. Before this split, WriteSnapshot dumped every bucket and
// RestoreSnapshot dropped every bucket, and hashicorp/raft restores the latest
// snapshot on every start once one exists. So each restart rebuilt the file from
// the snapshot plus the log and silently lost everything this node had written
// on its own since the snapshot: in-progress multipart uploads (issue #32 made
// them node-local), the active-active change log (whose sequence then went
// backwards), the audit trail, the console signing key. A follower installing
// the leader's snapshot also inherited the leader's copies of all of those.
//
// The list of Raft buckets is an allowlist, deliberately. Every bucket has to be
// classified, and TestEveryBoltBucketIsClassified fails until a new one is, but
// if a bucket ever slips through unlisted the allowlist fails safe. An unlisted
// bucket is left out of snapshots and left alone by a restore, so a node keeps
// what it had and the only cost is that a brand new node does not receive it.
// A denylist fails the other way: an unlisted node-local bucket would be wiped
// on every restart of every node, which is the data loss this exists to stop.

// raftOwnedBuckets hold state written only by the Raft state machine, so every
// node holds the same copy and a snapshot is a complete description of it.
//
// bucket_stats is here although a node can also repair it by hand. Its counters
// are derived from the objects bucket and adjusted by every object write the
// FSM applies, so they are only meaningful next to the objects they describe.
// Keeping a node's own counters across a restore that replaced its objects would
// leave the counters describing a different set of objects for good, whereas a
// counter repair lost to a restore is recomputed by the next backfill.
var raftOwnedBuckets = [][]byte{
	bucketsBucket,
	keysBucket,
	objectsBucket,
	policiesBucket,
	objectVersionsBucket,
	lifecycleBucket,
	websitesBucket,
	iamUsersBucket,
	iamGroupsBucket,
	iamPoliciesBucket,
	corsBucket,
	notificationBucket,
	backupHistoryBucket,
	versionTagsBucket,
	lambdaTriggersBucket,
	encryptionConfigBucket,
	publicAccessBlockBucket,
	loggingConfigBucket,
	bucketStatsBucket,
}

// nodeLocalBuckets hold records this node writes for itself, outside Raft. A
// snapshot never carries them and a restore never touches them.
//
//   - multipart_uploads and multipart_parts: in-progress uploads live on the node
//     that holds their parts (issue #32).
//   - audit_trail: each node records the requests it served.
//   - change_log: the active-active replication log, with its own sequence.
//   - replication_queue and replication_status: push replication is per node.
//     The node that served a write holds its bytes, queues the event and sends
//     it, so the queue and its delivery counters are that node's own. The
//     worker used to dequeue locally but acknowledge through Raft, and event
//     IDs are local sequence numbers, so an ack deleted whatever event had the
//     same ID on every other node.
//   - replication_configs: written straight to the local store by the S3 API.
//   - bucket_snapshots: written straight to the local store by the snapshot API.
//   - _restore_state: the marker of a restore in progress on this node.
var nodeLocalBuckets = [][]byte{
	multipartBucket,
	partsBucket,
	auditBucket,
	changeLogBucket,
	replicationQueueBucket,
	replicationStatusBucket,
	replicationConfigBucket,
	bucketSnapshotsBucket,
	restoreStateBucket,
}

// mixedBuckets hold both kinds of record, told apart by key. server_settings
// carries the committed shard map, which the FSM writes, next to the admin
// credentials and the console signing key, which each node keeps for itself.
// Only the listed keys belong to Raft.
var mixedBuckets = map[string]map[string]bool{
	string(serverSettingsBucket): {string(shardMapKey): true},
}

// bucketScope says how a snapshot treats one bolt bucket.
type bucketScope int

const (
	// scopeUnknown is a bucket in neither list. It is treated as node-local,
	// which is the safe reading (see above).
	scopeUnknown bucketScope = iota
	scopeRaft
	scopeLocal
	scopeMixed
)

func bucketScopeOf(name []byte) bucketScope {
	if _, ok := mixedBuckets[string(name)]; ok {
		return scopeMixed
	}
	for _, b := range raftOwnedBuckets {
		if string(b) == string(name) {
			return scopeRaft
		}
	}
	for _, b := range nodeLocalBuckets {
		if string(b) == string(name) {
			return scopeLocal
		}
	}
	return scopeUnknown
}

// raftOwnsKey reports whether one key of a mixed bucket belongs to Raft.
func raftOwnsKey(bucket, key []byte) bool {
	return mixedBuckets[string(bucket)][string(key)]
}

// CreateBucketAt creates a bucket with a creation time the caller chose.
//
// The Raft state machine uses it so that every node records the same time. The
// time used to be read from the clock inside the apply, so each node stamped its
// own, and a bucket's CreationDate depended on which node answered the listing,
// and changed again whenever a node replayed its log.
func (s *Store) CreateBucketAt(name string, createdAt time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketsBucket)
		if b.Get([]byte(name)) != nil {
			return fmt.Errorf("bucket already exists: %s", name)
		}
		info := BucketInfo{Name: name, CreatedAt: createdAt.UTC()}
		data, err := json.Marshal(info)
		if err != nil {
			return err
		}
		return b.Put([]byte(name), data)
	})
}

// DeleteExpiredAccessKeysAt removes the keys that had expired at the given unix
// time. Like CreateBucketAt it exists so the Raft state machine applies the same
// cut-off on every node: reading the clock during the apply let a node that
// applied later, or replayed its log after a restart, delete keys that the
// others kept.
func (s *Store) DeleteExpiredAccessKeysAt(now int64) (int, error) {
	deleted := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(keysBucket)
		var expired []AccessKey
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var key AccessKey
			if err := json.Unmarshal(v, &key); err != nil {
				continue
			}
			if key.ExpiresAt > 0 && key.ExpiresAt <= now {
				expired = append(expired, key)
			}
		}
		for _, key := range expired {
			if err := b.Delete([]byte(key.AccessKey)); err != nil {
				return err
			}
			// A scoped session lives in a synthetic user and policy named after
			// its key. They outlived the key, and kept accumulating as hidden
			// IAM users and policies for every session ever issued.
			if synthetic := "sts-" + key.AccessKey; key.SessionToken != "" && key.UserID == synthetic {
				if err := tx.Bucket(iamUsersBucket).Delete([]byte(synthetic)); err != nil {
					return err
				}
				if err := tx.Bucket(iamPoliciesBucket).Delete([]byte(synthetic)); err != nil {
					return err
				}
			}
			deleted++
		}
		return nil
	})
	return deleted, err
}

// EnqueueReplicationWithID stores a replication event under an ID the caller
// chose, replacing any event already stored under it.
//
// EnqueueReplication numbers events with the bucket's own sequence, which is
// fine on one node and wrong under Raft. A replayed log entry, after a restart
// or on top of a snapshot taken slightly later than its index, ran the sequence
// again and stored a second copy of the event under a new ID, and the next
// acknowledgement by ID then removed a different event than the one delivered.
// The state machine passes an ID derived from the log entry, so applying the
// same entry twice writes the same record twice.
//
// The bucket sequence is moved past the ID so a later EnqueueReplication on this
// store cannot hand out a number that is already in use.
func (s *Store) EnqueueReplicationWithID(event ReplicationEvent, id uint64) error {
	if id == 0 {
		return fmt.Errorf("replication event ID must not be zero")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(replicationQueueBucket)
		event.ID = id
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if b.Sequence() < id {
			if err := b.SetSequence(id); err != nil {
				return err
			}
		}
		return b.Put(replicationKey(id), data)
	})
}
