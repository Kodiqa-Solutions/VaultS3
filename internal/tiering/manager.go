package tiering

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

type Manager struct {
	store            *metadata.Store
	hotEngine        storage.Engine
	coldEngine       storage.Engine
	migrateAfterDays int
	scanIntervalSecs int
}

func NewManager(store *metadata.Store, hotEngine, coldEngine storage.Engine, migrateAfterDays, scanIntervalSecs int) *Manager {
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
	migrated := 0

	m.store.IterateAllObjects(func(bucket, key string, meta metadata.ObjectMeta) bool {
		if meta.DeleteMarker {
			return true
		}
		tier := meta.Tier
		if tier == "" {
			tier = "hot"
		}
		if tier != "hot" {
			return true
		}

		lastAccess := meta.LastAccessTime
		if lastAccess == 0 {
			lastAccess = meta.LastModified
		}
		if lastAccess > cutoff {
			return true
		}

		if err := m.migrateToCol(bucket, key); err != nil {
			slog.Error("tiering failed to migrate to cold", "bucket", bucket, "key", key, "error", err)
		} else {
			migrated++
		}
		return true
	})

	if migrated > 0 {
		slog.Info("tiering migration complete", "objects", migrated)
	}
}

func (m *Manager) migrateToCol(bucket, key string) error {
	reader, size, err := m.hotEngine.GetObject(bucket, key)
	if err != nil {
		return err
	}
	defer reader.Close()

	m.coldEngine.CreateBucketDir(bucket)
	if _, _, err := m.coldEngine.PutObject(bucket, key, reader, size); err != nil {
		return err
	}

	if err := m.hotEngine.DeleteObject(bucket, key); err != nil {
		return err
	}

	return m.store.SetObjectTier(bucket, key, "cold")
}

func (m *Manager) MigrateToHot(bucket, key string) error {
	reader, size, err := m.coldEngine.GetObject(bucket, key)
	if err != nil {
		return err
	}
	defer reader.Close()

	if _, _, err := m.hotEngine.PutObject(bucket, key, reader, size); err != nil {
		return err
	}

	if err := m.coldEngine.DeleteObject(bucket, key); err != nil {
		slog.Error("tiering failed to delete cold copy", "bucket", bucket, "key", key, "error", err)
	}

	return m.store.SetObjectTier(bucket, key, "hot")
}

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
		m.store.SetObjectTier(bucket, key, "hot")
		// Only delete cold copy after tier metadata is updated
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
		return m.migrateToCol(bucket, key)
	}
	return m.MigrateToHot(bucket, key)
}

// ColdReader returns a reader for a cold-tier object without promoting it.
func (m *Manager) ColdReader(bucket, key string) (io.ReadCloser, int64, error) {
	reader, size, err := m.coldEngine.GetObject(bucket, key)
	return reader, size, err
}
