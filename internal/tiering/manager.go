package tiering

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

type Manager struct {
	store            metadata.StoreAPI
	hotEngine        storage.Engine
	coldEngine       storage.Engine
	migrateAfterDays int
	scanIntervalSecs int

	// beforeRestoreLink runs just before a restore puts the data in place,
	// so a test can land a PUT there.
	beforeRestoreLink func()
}

func NewManager(store metadata.StoreAPI, hotEngine, coldEngine storage.Engine, migrateAfterDays, scanIntervalSecs int) *Manager {
	return &Manager{
		store:            store,
		hotEngine:        hotEngine,
		coldEngine:       coldEngine,
		migrateAfterDays: migrateAfterDays,
		scanIntervalSecs: scanIntervalSecs,
	}
}

func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(m.scanIntervalSecs) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.scan()
		}
	}
}

func (m *Manager) scan() {
	cutoff := time.Now().Unix() - int64(m.migrateAfterDays*86400)

	// Collect migration candidates inside the read transaction, but perform the
	// actual migration afterwards: migrateToCol writes via SetObjectTier (a write
	// transaction), and bbolt deadlocks if a write txn is opened while the read
	// txn from IterateAllObjects is still in progress on the same goroutine.
	type objRef struct{ bucket, key string }
	var candidates []objRef

	m.store.IterateAllObjects(func(bucket, key string, meta metadata.ObjectMeta) bool {
		if meta.DeleteMarker {
			return true
		}
		tier := meta.Tier
		if tier == "" {
			tier = "hot"
		}
		if tier != "hot" || meta.VersionID != "" {
			return true
		}

		lastAccess := meta.LastAccessTime
		if lastAccess == 0 {
			lastAccess = meta.LastModified
		}
		if lastAccess > cutoff {
			return true
		}

		candidates = append(candidates, objRef{bucket: bucket, key: key})
		return true
	})

	migrated := 0
	for _, c := range candidates {
		if err := m.migrateToCol(c.bucket, c.key); errors.Is(err, errChanged) {
			slog.Info("tiering skipped an object rewritten during migration", "bucket", c.bucket, "key", c.key)
		} else if err != nil {
			slog.Error("tiering failed to migrate to cold", "bucket", c.bucket, "key", c.key, "error", err)
		} else {
			migrated++
		}
	}

	if migrated > 0 {
		slog.Info("tiering migration complete", "objects", migrated)
	}
}

// sameObject reports whether cur still describes the object snap described. A
// PUT replaces the record with a new ETag and modification time.
func sameObject(snap, cur *metadata.ObjectMeta) bool {
	return cur.ETag == snap.ETag && cur.LastModified == snap.LastModified &&
		cur.VersionID == snap.VersionID && cur.DeleteMarker == snap.DeleteMarker
}

// errChanged means the object was rewritten while it was being moved, so the
// move was abandoned and the new object left where its writer put it.
var errChanged = errors.New("object changed during tier migration, left in place")

func (m *Manager) migrateToCol(bucket, key string) error {
	snap, err := m.store.GetObjectMeta(bucket, key)
	if err != nil {
		return err
	}
	if snap.DeleteMarker || (snap.Tier != "" && snap.Tier != "hot") {
		return nil
	}
	if snap.VersionID != "" {
		// The current version of a versioned object is not at the plain
		// object path this manager reads and deletes, so moving it would copy
		// the wrong bytes, or none.
		return fmt.Errorf("versioned objects are not tiered")
	}

	reader, size, err := m.hotEngine.GetObject(bucket, key)
	if err != nil {
		return err
	}
	defer reader.Close()

	m.coldEngine.CreateBucketDir(bucket)
	if _, _, err := m.coldEngine.PutObject(bucket, key, reader, size); err != nil {
		return err
	}

	// A PUT that landed while the bytes were being copied has replaced the hot
	// object and its record. Deleting the hot copy then destroyed the NEW data
	// and marked the new record cold, so reads returned the old bytes. Check
	// before switching the tier, and again after, so a PUT on either side of
	// the switch is caught.
	if cur, err := m.store.GetObjectMeta(bucket, key); err != nil || !sameObject(snap, cur) {
		m.dropColdCopy(bucket, key)
		return errChanged
	}
	if err := m.store.SetObjectTier(bucket, key, "cold"); err != nil {
		m.dropColdCopy(bucket, key)
		return err
	}
	if cur, err := m.store.GetObjectMeta(bucket, key); err != nil || !sameObject(snap, cur) {
		// The tier may have been set on the new record. Put it back to hot,
		// where the new data is, and keep the hot copy.
		if cur != nil && cur.Tier == "cold" {
			m.store.SetObjectTier(bucket, key, "hot")
		}
		m.dropColdCopy(bucket, key)
		return errChanged
	}

	// Reads now go to the cold copy. A PUT landing between the check above and
	// this delete can still lose its data, because the engine cannot delete
	// conditionally. Closing that needs a per-key lock shared with the S3
	// write path.
	if err := m.hotEngine.DeleteObject(bucket, key); err != nil {
		slog.Warn("tiering could not remove the hot copy after moving it", "bucket", bucket, "key", key, "error", err)
	}
	return nil
}

func (m *Manager) dropColdCopy(bucket, key string) {
	if err := m.coldEngine.DeleteObject(bucket, key); err != nil {
		slog.Warn("tiering could not remove an abandoned cold copy", "bucket", bucket, "key", key, "error", err)
	}
}

// ErrNothingToRestore means the object has no cold copy to bring back, or
// already has its data in the hot tier.
var ErrNothingToRestore = errors.New("the object has no data in the cold tier to restore")

// MigrateToHot moves an object's data back from the cold tier. It acts when
// the record says cold, and also when the record says hot but the data is
// still only in the cold directory, as 5.0.10's RestoreObject left objects
// (it marked the record hot without moving anything).
//
// The data is put in place only where no hot file exists, never over one: a
// PUT that lands while the restore runs has written the object's new data,
// which copying the old cold bytes over it would destroy. The cold copy is
// then stale and is dropped.
func (m *Manager) MigrateToHot(bucket, key string) error {
	meta, err := m.store.GetObjectMeta(bucket, key)
	if err != nil || meta == nil || meta.DeleteMarker {
		return ErrNothingToRestore
	}
	// The plain path holds the current object, or the null version below a
	// newer one. With a newer current version and no live null version, a
	// cold file there is an object deleted on purpose: bringing it back
	// would leave its bytes on disk with no record.
	if meta.Tier != "cold" && meta.VersionID != "" && meta.VersionID != "null" {
		null, nerr := m.store.GetObjectVersion(bucket, key, "null")
		if nerr != nil || null == nil || null.DeleteMarker {
			return ErrNothingToRestore
		}
	}
	// A null version written while versioning was suspended lives in the
	// version store, not at the plain path: a cold file there is the object
	// it replaced, and bringing it back would only leave an orphan.
	if meta.VersionID != "" {
		if rc, _, verr := m.hotEngine.GetObjectVersion(bucket, key, "null"); verr == nil {
			rc.Close()
			return ErrNothingToRestore
		}
	}
	coldPath := m.coldEngine.ObjectPath(bucket, key)
	hotPath := m.hotEngine.ObjectPath(bucket, key)
	coldInfo, err := os.Stat(coldPath)
	if err != nil {
		return ErrNothingToRestore
	}

	// A folder marker ("photos/") is a directory, not a file to copy.
	if strings.HasSuffix(key, "/") || coldInfo.IsDir() {
		if err := os.MkdirAll(hotPath, 0o755); err != nil {
			return err
		}
		if meta.Tier == "cold" {
			if err := m.store.SetObjectTier(bucket, key, "hot"); err != nil {
				return err
			}
		}
		m.dropColdCopy(bucket, key)
		return nil
	}

	if fi, err := os.Lstat(hotPath); err == nil {
		if !fi.Mode().IsRegular() {
			// A directory (another key, "a/b", made one at "a") or anything
			// but a file: not this object's data. Keep the cold copy, which
			// is the only one.
			return ErrHotPathBlocked
		}
		if meta.Tier == "cold" {
			// Data is in both tiers and the record says cold: the hot file is
			// newer than the move (written since), so it is the object.
			m.store.SetObjectTier(bucket, key, "hot")
			m.dropColdCopy(bucket, key)
			return nil
		}
		return ErrNothingToRestore
	}

	// Copy next to the hot path, then link into place: a link fails if a
	// file has appeared there since, where a rename would replace it.
	if err := os.MkdirAll(filepath.Dir(hotPath), 0o755); err != nil {
		return err
	}
	src, err := os.Open(coldPath)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(hotPath), ".vaults3-tmp-restore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if m.beforeRestoreLink != nil {
		m.beforeRestoreLink()
	}
	if err := os.Link(tmpName, hotPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			// A writer got there first: its data is the object now.
			if cur, cerr := m.store.GetObjectMeta(bucket, key); cerr == nil && cur != nil && cur.Tier == "cold" {
				m.store.SetObjectTier(bucket, key, "hot")
			}
			m.dropColdCopy(bucket, key)
			return nil
		}
		return err
	}

	// A DELETE that landed while the data was copied removed the record: the
	// restored bytes belong to nothing, so they go, with the cold copy.
	// removeOwn takes the restored file away only while it is still the
	// restore's own (still linked to the temp file): a PUT that landed since
	// has replaced it, and its data must stay.
	removeOwn := func() {
		tfi, terr := os.Stat(tmpName)
		hfi, herr := os.Lstat(hotPath)
		if terr == nil && herr == nil && os.SameFile(tfi, hfi) {
			os.Remove(hotPath)
		}
		m.dropColdCopy(bucket, key)
	}
	cur, cerr := m.store.GetObjectMeta(bucket, key)
	if cerr != nil || cur == nil || cur.DeleteMarker {
		removeOwn()
		return ErrNothingToRestore
	}
	if cur.Tier == "cold" {
		if err := m.store.SetObjectTier(bucket, key, "hot"); err != nil {
			// The record went between the read and the write: a DELETE.
			if again, aerr := m.store.GetObjectMeta(bucket, key); aerr != nil || again == nil || again.DeleteMarker {
				removeOwn()
				return ErrNothingToRestore
			}
			return err
		}
	}
	m.dropColdCopy(bucket, key)
	return nil
}

// ErrHotPathBlocked means something other than the object's file is at its
// hot path (a directory another key made), so its data cannot be put back.
// The cold copy is kept.
var ErrHotPathBlocked = errors.New("the object's path in the hot tier is taken by a directory, so it cannot be restored; its cold copy is kept")

// GetObject transparently reads from the correct tier.
func (m *Manager) GetObject(bucket, key string) (storage.ReadSeekCloser, int64, error) {
	meta, err := m.store.GetObjectMeta(bucket, key)
	if err != nil {
		return m.hotEngine.GetObject(bucket, key)
	}

	tier := meta.Tier
	if tier == "" || tier == "hot" {
		reader, size, err := m.hotEngine.GetObject(bucket, key)
		if err == nil {
			return reader, size, nil
		}
		// Fallback to cold if hot doesn't have it
		return m.coldEngine.GetObject(bucket, key)
	}

	// Cold tier — read from cold
	reader, size, err := m.coldEngine.GetObject(bucket, key)
	if err != nil {
		return nil, 0, err
	}

	// Promote back to hot on access (async, with safety checks)
	go func() {
		// Re-check tier before promoting — another goroutine may have already done it
		currentMeta, err := m.store.GetObjectMeta(bucket, key)
		if err != nil || currentMeta.Tier == "hot" || currentMeta.Tier == "" {
			return // already promoted or deleted
		}
		promoteReader, promoteSize, err := m.coldEngine.GetObject(bucket, key)
		if err != nil {
			return
		}
		defer promoteReader.Close()
		if _, _, err := m.hotEngine.PutObject(bucket, key, promoteReader, promoteSize); err != nil {
			slog.Error("tiering async promote failed", "bucket", bucket, "key", key, "error", err)
			return
		}
		// Only delete the cold copy once the record points at the hot one. If
		// the tier could not be updated, reads still go to cold, so the cold
		// copy is the one that must survive.
		if err := m.store.SetObjectTier(bucket, key, "hot"); err != nil {
			slog.Error("tiering async promote could not update the tier", "bucket", bucket, "key", key, "error", err)
			return
		}
		m.coldEngine.DeleteObject(bucket, key)
	}()

	return reader, size, nil
}

// Status returns hot/cold counts and sizes.
func (m *Manager) Status() map[string]interface{} {
	var hotCount, coldCount int64
	var hotSize, coldSize int64

	m.store.IterateAllObjects(func(bucket, key string, meta metadata.ObjectMeta) bool {
		if meta.DeleteMarker {
			return true
		}
		tier := meta.Tier
		if tier == "" || tier == "hot" {
			hotCount++
			hotSize += meta.Size
		} else {
			coldCount++
			coldSize += meta.Size
		}
		return true
	})

	return map[string]interface{}{
		"enabled":            true,
		"hot_count":          hotCount,
		"hot_size":           hotSize,
		"cold_count":         coldCount,
		"cold_size":          coldSize,
		"migrate_after_days": m.migrateAfterDays,
		"scan_interval_secs": m.scanIntervalSecs,
	}
}

// ManualMigrate allows manual migration of a specific object.
func (m *Manager) ManualMigrate(bucket, key, direction string) error {
	if direction == "cold" {
		return ErrColdWritesDisabled
	}
	return m.MigrateToHot(bucket, key)
}

// ErrColdWritesDisabled refuses moving an object to the cold tier. Neither the
// S3 API nor the dashboard reads from it, and the files are moved below the
// encryption and compression layers, so every object moved there became
// unreadable (NoSuchKey) until it was moved back. Until reads go through the
// tier, objects are only moved back: MigrateToHot and RestoreObject.
var ErrColdWritesDisabled = errors.New("moving objects to the cold tier is disabled in this version: objects there cannot be read; move existing ones back with RestoreObject or the hot direction")

// ColdReader returns a reader for a cold-tier object without promoting it.
func (m *Manager) ColdReader(bucket, key string) (io.ReadCloser, int64, error) {
	reader, size, err := m.coldEngine.GetObject(bucket, key)
	return reader, size, err
}
