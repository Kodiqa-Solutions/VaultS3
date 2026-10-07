// Package snapshot implements "git-for-buckets": immutable named snapshots of a
// bucket's state, with history, diff against the live bucket, and one-shot
// rollback. It works purely on metadata version pointers — taking or restoring a
// snapshot copies no object data, it just records and re-points which version is
// "latest" for each key. Bucket versioning must be Enabled so the captured
// versions remain restorable.
package snapshot

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
	Removed  int `json:"removed"`  // keys added after the snapshot, now hidden behind a delete marker
	Skipped  int `json:"skipped"`  // snapshot versions no longer available (e.g. expired)
	// Failed counts keys the restore could not change because a metadata
	// operation failed. They used to be counted as skipped, which made a store
	// failure look like an expired version.
	Failed int      `json:"failed"`
	Errors []string `json:"errors,omitempty"`
}

// maxRestoreErrors bounds how many failures a result spells out.
const maxRestoreErrors = 50

func (res *RestoreResult) fail(key string, err error) {
	res.Failed++
	if len(res.Errors) < maxRestoreErrors {
		res.Errors = append(res.Errors, key+": "+err.Error())
	}
}

// Restore rolls the bucket back to a snapshot: every captured key is re-pointed
// to its snapshot version (reverting modifications and un-deleting), and any key
// added since the snapshot is hidden behind a delete marker. No object data is
// deleted, versions remain, so a restore is itself reversible by snapshotting
// first.
//
// Keys added after the snapshot used to have their latest pointer deleted
// outright, with no delete marker. A key written before versioning was enabled
// has no version record, so that removed the only record of it, and its bytes
// became an orphan for the reclaim scan to delete: the restore destroyed data
// it promised to keep.
//
// The returned error is non-nil when any key failed. The result is returned
// with it, so the caller can say how far the restore got.
func (m *Manager) Restore(bucket, id string) (*RestoreResult, error) {
	snap, err := m.store.GetBucketSnapshot(bucket, id)
	if err != nil {
		return nil, err
	}
	cur, _, err := m.store.ListLatestObjects(bucket, "", "", 0)
	if err != nil {
		return nil, err
	}
	current := make(map[string]metadata.ObjectMeta, len(cur))
	for _, o := range cur {
		current[o.Key] = o
	}

	res := &RestoreResult{}
	inSnap := make(map[string]bool, len(snap.Entries))
	for _, e := range snap.Entries {
		inSnap[e.Key] = true
		if o, ok := current[e.Key]; ok && !o.DeleteMarker && o.VersionID == e.VersionID && o.ETag == e.ETag {
			res.Reverted++ // already the snapshot's version
			continue
		}
		// An object captured before versioning was enabled has no version id.
		// Once something replaced it, it lives on as the "null" version.
		want := e.VersionID
		if want == "" {
			want = "null"
		}
		if _, err := m.store.GetObjectVersion(bucket, e.Key, want); err != nil {
			if versionMissing(err) {
				res.Skipped++ // version no longer present
			} else {
				res.fail(e.Key, err)
			}
			continue
		}
		if err := m.store.SetLatestVersion(bucket, e.Key, want); err != nil {
			res.fail(e.Key, err)
			continue
		}
		res.Reverted++
	}
	for _, o := range cur {
		if inSnap[o.Key] || o.DeleteMarker {
			continue
		}
		if err := m.hideBehindDeleteMarker(bucket, o); err != nil {
			res.fail(o.Key, err)
			continue
		}
		res.Removed++
	}
	if res.Failed > 0 {
		return res, fmt.Errorf("%d of the bucket's keys could not be restored", res.Failed)
	}
	return res, nil
}

// versionMissing tells a version that is gone from a store that could not
// answer. The store reports the first as a plain "not found" error.
func versionMissing(err error) bool {
	if errors.Is(err, metadata.ErrShardUnavailable) {
		return false
	}
	return strings.Contains(err.Error(), "not found")
}

// hideBehindDeleteMarker does what a versioned delete does: the current
// object becomes a non-latest version, adopted as the "null" version when it
// predates versioning, and a delete marker becomes the latest.
func (m *Manager) hideBehindDeleteMarker(bucket string, o metadata.ObjectMeta) error {
	old := o
	old.Bucket = bucket
	if old.VersionID == "" {
		old.VersionID = "null"
	}
	old.IsLatest = false
	if err := m.store.PutObjectVersion(old); err != nil {
		return fmt.Errorf("demote current version: %w", err)
	}
	dm := metadata.ObjectMeta{
		Bucket: bucket, Key: o.Key, VersionID: newVersionID(),
		DeleteMarker: true, IsLatest: true, LastModified: time.Now().UTC().Unix(),
	}
	if err := m.store.PutObjectVersion(dm); err != nil {
		return fmt.Errorf("write delete marker: %w", err)
	}
	if err := m.store.PutObjectMeta(dm); err != nil {
		return fmt.Errorf("write delete marker: %w", err)
	}
	return nil
}

// newVersionID produces a sortable, unique version id in the format the S3
// layer uses: nanosecond timestamp then a random suffix.
func newVersionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%016x%s", time.Now().UnixNano(), hex.EncodeToString(b))
}

func snapshotID(bucket string, tsNanos int64, msg string) string {
	h := sha256.Sum256([]byte(bucket + "|" + strconv.FormatInt(tsNanos, 10) + "|" + msg))
	return hex.EncodeToString(h[:])[:12]
}
