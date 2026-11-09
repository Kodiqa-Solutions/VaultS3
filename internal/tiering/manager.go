package tiering

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// beforeColdRemove runs just before a move to cold removes the hot file.
	beforeColdRemove func()

	// coldMovesOff, when set, says why objects are not moved to the cold
	// tier on this server. Restores still run.
	coldMovesOff string

	// keyLocks serializes moves and restores of one key. Two of them
	// overlapping (the scan and a manual move, or a RestoreObject arriving
	// during a move) each judged the files by the state before the other's
	// change, and between them removed both copies.
	keyLocks [64]sync.Mutex
}

func (m *Manager) lockKey(bucket, key string) func() {
	h := fnv.New32a()
	h.Write([]byte(bucket))
	h.Write([]byte{0})
	h.Write([]byte(key))
	l := &m.keyLocks[h.Sum32()%uint32(len(m.keyLocks))]
	l.Lock()
	return l.Unlock
}

// ErrColdMovesUnsupported refuses a move to cold on a server whose storage
// layout the move cannot handle.
var ErrColdMovesUnsupported = errors.New("moving objects to the cold tier is not supported with this storage configuration")

// DisableColdMoves stops the scan and refuses manual moves to cold, for a
// storage layout in which an object is not one file at its object path:
// erasure coding keeps shards, and packing keeps small objects inside volume
// files, so the move would find nothing to copy or copy stale bytes. Objects
// already cold can still be restored.
func (m *Manager) DisableColdMoves(reason string) { m.coldMovesOff = reason }

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
	if m.coldMovesOff != "" {
		slog.Warn("tiering does not move objects to the cold tier", "reason", m.coldMovesOff)
		return
	}
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
	var candidates, stale []objRef

	m.store.IterateAllObjects(func(bucket, key string, meta metadata.ObjectMeta) bool {
		if meta.DeleteMarker {
			return true
		}
		tier := meta.Tier
		if tier == "" {
			tier = "hot"
		}
		if tier == "cold" && meta.VersionID == "" {
			stale = append(stale, objRef{bucket, key}) // checked below
			return true
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

	// A record still marked cold whose data a rewrite put back in the hot
	// tier (an engine-level rewrite of a cold object writes hot and drops the
	// cold copy) would never be moved again. It is hot.
	for _, s := range stale {
		m.repairColdRecord(s.bucket, s.key)
	}

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
	defer m.lockKey(bucket, key)()
	defer m.invalidateSizes(bucket)
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
	// The file being copied, to tell it apart from one a PUT renames into
	// place while the copy runs.
	var copied os.FileInfo
	if f, ok := reader.(*os.File); ok {
		copied, _ = f.Stat()
	}

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

	// Reads now go to the cold copy. The hot file is removed only while it is
	// still the one that was copied: a PUT renames its new file into place, so
	// a different file there is newer data, which stays, and the record goes
	// back to hot. Removing whatever was at the path deleted a PUT that landed
	// in this window.
	//
	// The file is first moved aside, atomically, and only then compared, so a
	// PUT cannot land between the comparison and the removal. A file that is
	// not the copied one is put back, unless a newer one has appeared since.
	hotPath := m.hotEngine.ObjectPath(bucket, key)
	if m.beforeColdRemove != nil {
		m.beforeColdRemove()
	}
	aside := filepath.Join(filepath.Dir(hotPath), ".vaults3-tmp-tier-"+filepath.Base(hotPath))
	// Only a regular file is this key's copy. A folder there belongs to other
	// keys ("a" deleted and "a/x" written meanwhile): moving it aside, it
	// could not be linked back, and those objects were lost.
	if fi, lerr := os.Lstat(hotPath); lerr == nil && !fi.Mode().IsRegular() {
		m.revertToHot(bucket, key)
		return errChanged
	}
	if err := os.Rename(hotPath, aside); err != nil {
		// No hot file to remove. If the record still names the object that
		// was copied, the copy in the cold tier is now its only copy: keep it,
		// and the record says cold. Dropping it here lost the object when
		// something else had already removed the hot file.
		if cur, gerr := m.store.GetObjectMeta(bucket, key); gerr == nil && cur != nil && sameObject(snap, cur) && os.IsNotExist(err) {
			return nil
		}
		m.revertToHot(bucket, key)
		return errChanged
	}
	moved, err := os.Lstat(aside)
	if err == nil && copied != nil && os.SameFile(copied, moved) {
		if err := os.Remove(aside); err != nil {
			slog.Warn("tiering could not remove the hot copy after moving it", "bucket", bucket, "key", key, "error", err)
		}
		// As the engine's own delete does: folders the key leaves empty go,
		// or a later object named after one ("a" once "a/b" moved) cannot be
		// written.
		removeEmptyParents(filepath.Dir(hotPath), filepath.Dir(m.hotEngine.ObjectPath(bucket, "x")))
		return nil
	}
	// What was moved aside is not the copied file. Anything but a regular
	// file (the path became another key's folder after the check above) goes
	// back whole, by rename, which a folder allows and a link does not.
	if err == nil && !moved.Mode().IsRegular() {
		if rerr := os.Rename(aside, hotPath); rerr != nil {
			slog.Error("tiering could not put back a folder it moved aside", "bucket", bucket, "path", hotPath, "error", rerr)
			return rerr
		}
		m.revertToHot(bucket, key)
		return errChanged
	}
	// A PUT replaced the file while it was copied: its data is the object.
	if lerr := os.Link(aside, hotPath); lerr != nil && !os.IsExist(lerr) {
		slog.Error("tiering could not put back a file a PUT wrote during the move", "bucket", bucket, "key", key, "error", lerr)
		return lerr
	}
	if err := os.Remove(aside); err != nil {
		slog.Warn("tiering could not remove the name it moved a file aside to", "path", aside, "error", err)
	}
	m.revertToHot(bucket, key)
	return errChanged
}

// repairColdRecord marks a cold record hot when only the hot tier holds its
// data.
func (m *Manager) repairColdRecord(bucket, key string) {
	defer m.lockKey(bucket, key)()
	cur, err := m.store.GetObjectMeta(bucket, key)
	if err != nil || cur == nil || cur.Tier != "cold" || cur.VersionID != "" {
		return
	}
	hp := m.hotEngine.ObjectPath(bucket, key)
	if fi, err := os.Lstat(hp); err != nil || !fi.Mode().IsRegular() {
		return
	}
	if err := m.store.SetObjectTier(bucket, key, "hot"); err == nil {
		slog.Info("tiering: a record marked cold had its data in the hot tier, marked hot", "bucket", bucket, "key", key)
		m.dropColdCopy(bucket, key)
	}
}

// invalidateSizes has both tiers measure the bucket again: a move renames,
// links and removes files directly, which the engines' cached bucket sizes do
// not see.
func (m *Manager) invalidateSizes(bucket string) {
	for _, e := range []storage.Engine{m.hotEngine, m.coldEngine} {
		if i, ok := e.(interface{ InvalidateBucketSize(string) }); ok {
			i.InvalidateBucketSize(bucket)
		}
	}
}

// removeEmptyParents removes dir and its parents while they are empty, up to
// but not including stop (the bucket directory).
func removeEmptyParents(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// revertToHot undoes a move whose hot file turned out to be newer data: the
// record goes back to hot, where that data is, and the cold copy goes.
func (m *Manager) revertToHot(bucket, key string) {
	if c, err := m.store.GetObjectMeta(bucket, key); err == nil && c != nil && c.Tier == "cold" {
		if err := m.store.SetObjectTier(bucket, key, "hot"); err != nil {
			// The cold copy is kept: the record still says cold, and the
			// scan's repair (repairColdRecord) marks it hot and drops the
			// cold copy, since reads take the hot file first.
			slog.Error("tiering could not mark an object hot again, keeping its cold copy", "bucket", bucket, "key", key, "error", err)
			return
		}
	}
	m.dropColdCopy(bucket, key)
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
	defer m.lockKey(bucket, key)()
	defer m.invalidateSizes(bucket)
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
		if m.coldMovesOff != "" {
			return fmt.Errorf("%w: %s", ErrColdMovesUnsupported, m.coldMovesOff)
		}
		return m.migrateToCol(bucket, key)
	}
	return m.MigrateToHot(bucket, key)
}

// ColdObjectPath is where key's file is in the cold tier.
func (m *Manager) ColdObjectPath(bucket, key string) string {
	return m.coldEngine.ObjectPath(bucket, key)
}

// ColdReader returns a reader for a cold-tier object without promoting it.
func (m *Manager) ColdReader(bucket, key string) (io.ReadCloser, int64, error) {
	reader, size, err := m.coldEngine.GetObject(bucket, key)
	return reader, size, err
}
