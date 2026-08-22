// Package snapshot implements "git-for-buckets": immutable named snapshots of a
// bucket's state, with history, diff against the live bucket, and one-shot
// rollback. It works purely on metadata version pointers — taking or restoring a
// snapshot copies no object data, it just records and re-points which version is
// "latest" for each key. Bucket versioning must be Enabled so the captured
// versions remain restorable.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Manager creates, inspects, and restores bucket snapshots.
type Manager struct {
	store metadata.StoreAPI
}

func NewManager(store metadata.StoreAPI) *Manager {
	return &Manager{store: store}
}

// Create captures the current state of a bucket as a named snapshot. Requires
// versioning Enabled so the captured object versions stay restorable.
func (m *Manager) Create(bucket, message string) (*metadata.BucketSnapshot, error) {
	if !m.store.BucketExists(bucket) {
		return nil, fmt.Errorf("bucket does not exist")
	}
	if ver, _ := m.store.GetBucketVersioning(bucket); ver != "Enabled" {
		return nil, fmt.Errorf("bucket versioning must be Enabled to take snapshots (so versions stay restorable)")
	}

	objs, _, err := m.store.ListLatestObjects(bucket, "", "", 0)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	snap := metadata.BucketSnapshot{Bucket: bucket, Message: message, CreatedAt: now.Unix(), CreatedAtNano: now.UnixNano()}
	for _, o := range objs {
		snap.Entries = append(snap.Entries, metadata.BucketSnapshotEntry{
			Key: o.Key, VersionID: o.VersionID, ETag: o.ETag, Size: o.Size,
		})
		snap.Size += o.Size
	}
	snap.Objects = len(snap.Entries)
	snap.ID = snapshotID(bucket, now.UnixNano(), message)

	if err := m.store.PutBucketSnapshot(snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// List returns a bucket's snapshots, newest first.
func (m *Manager) List(bucket string) ([]metadata.BucketSnapshot, error) {
	return m.store.ListBucketSnapshots(bucket)
}

// Get returns a snapshot with its full manifest.
func (m *Manager) Get(bucket, id string) (*metadata.BucketSnapshot, error) {
	return m.store.GetBucketSnapshot(bucket, id)
}

// Delete removes a snapshot (the object data/versions are untouched).
func (m *Manager) Delete(bucket, id string) error {
	return m.store.DeleteBucketSnapshot(bucket, id)
}

// Change is a single difference between a snapshot and the live bucket.
type Change struct {
	Key  string `json:"key"`
	Kind string `json:"kind"` // "added", "removed", "modified"
}

// DiffResult summarizes how the live bucket differs from a snapshot.
type DiffResult struct {
	Added    int      `json:"added"`
	Removed  int      `json:"removed"`
	Modified int      `json:"modified"`
	Changes  []Change `json:"changes"`
}

// Diff compares the live bucket against a snapshot (what changed since).
func (m *Manager) Diff(bucket, id string) (*DiffResult, error) {
	snap, err := m.store.GetBucketSnapshot(bucket, id)
	if err != nil {
		return nil, err
	}
	cur, _, err := m.store.ListLatestObjects(bucket, "", "", 0)
	if err != nil {
		return nil, err
	}

	snapVer := make(map[string]string, len(snap.Entries))
	for _, e := range snap.Entries {
		snapVer[e.Key] = e.VersionID
	}
	curVer := make(map[string]string, len(cur))
	for _, o := range cur {
		curVer[o.Key] = o.VersionID
	}

	res := &DiffResult{Changes: []Change{}} // never nil — JSON [] not null
	for k, cv := range curVer {
		sv, ok := snapVer[k]
		switch {
		case !ok:
			res.Changes = append(res.Changes, Change{Key: k, Kind: "added"})
			res.Added++
		case sv != cv:
			res.Changes = append(res.Changes, Change{Key: k, Kind: "modified"})
			res.Modified++
		}
	}
	for k := range snapVer {
		if _, ok := curVer[k]; !ok {
			res.Changes = append(res.Changes, Change{Key: k, Kind: "removed"})
			res.Removed++
		}
	}
	sort.Slice(res.Changes, func(i, j int) bool { return res.Changes[i].Key < res.Changes[j].Key })
	return res, nil
}

// RestoreResult summarizes a rollback.
type RestoreResult struct {
	Reverted int `json:"reverted"` // keys re-pointed to the snapshot version
	Removed  int `json:"removed"`  // keys added after the snapshot, now removed
	Skipped  int `json:"skipped"`  // snapshot versions no longer available (e.g. expired)
}

// Restore rolls the bucket back to a snapshot: every captured key is re-pointed
// to its snapshot version (reverting modifications and un-deleting), and any key
// added since the snapshot is removed from the live listing. No object data is
// deleted — versions remain, so a restore is itself reversible by snapshotting
// first.
func (m *Manager) Restore(bucket, id string) (*RestoreResult, error) {
	snap, err := m.store.GetBucketSnapshot(bucket, id)
	if err != nil {
		return nil, err
	}
	cur, _, err := m.store.ListLatestObjects(bucket, "", "", 0)
	if err != nil {
		return nil, err
	}

	res := &RestoreResult{}
	inSnap := make(map[string]bool, len(snap.Entries))
	for _, e := range snap.Entries {
		inSnap[e.Key] = true
		if err := m.store.SetLatestVersion(bucket, e.Key, e.VersionID); err != nil {
			res.Skipped++ // version no longer present
			continue
		}
		res.Reverted++
	}
	for _, o := range cur {
		if !inSnap[o.Key] {
			m.store.DeleteObjectMeta(bucket, o.Key)
			res.Removed++
		}
	}
	return res, nil
}

func snapshotID(bucket string, tsNanos int64, msg string) string {
	h := sha256.Sum256([]byte(bucket + "|" + strconv.FormatInt(tsNanos, 10) + "|" + msg))
	return hex.EncodeToString(h[:])[:12]
}
