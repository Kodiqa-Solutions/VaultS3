package backup

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

type Scheduler struct {
	store       *metadata.Store
	engine      storage.Engine
	cfg         config.BackupConfig
	lastRunHour int
	running     atomic.Bool
}

func NewScheduler(store *metadata.Store, engine storage.Engine, cfg config.BackupConfig) *Scheduler {
	return &Scheduler{
		store:       store,
		engine:      engine,
		cfg:         cfg,
		lastRunHour: -1,
	}
}

func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.shouldRun() {
				s.runBackup()
			}
		}
	}
}

// shouldRun checks if the backup should run based on cron schedule.
// Simplified cron: only supports "M H * * *" format.
func (s *Scheduler) shouldRun() bool {
	if s.running.Load() {
		return false
	}

	now := time.Now()
	parts := strings.Fields(s.cfg.ScheduleCron)
	if len(parts) < 2 {
		return false
	}

	minute, _ := strconv.Atoi(parts[0])
	hour, _ := strconv.Atoi(parts[1])

	if now.Hour() == hour && now.Minute() == minute && s.lastRunHour != now.Hour() {
		s.lastRunHour = now.Hour()
		return true
	}
	return false
}

func (s *Scheduler) runBackup() {
	s.running.Store(true)
	defer s.running.Store(false)

	for _, target := range s.cfg.Targets {
		backupType := "full"
		if s.cfg.Incremental {
			backupType = "incremental"
		}

		record := metadata.BackupRecord{
			ID:        fmt.Sprintf("backup-%d", time.Now().UnixNano()),
			Type:      backupType,
			Target:    target.Name,
			StartTime: time.Now().Unix(),
			Status:    "running",
		}
		s.store.PutBackupRecord(record)

		err := s.backupToTarget(target, backupType, &record)
		record.EndTime = time.Now().Unix()
		if err != nil {
			record.Status = "failed"
			record.Error = err.Error()
			slog.Error("backup failed", "target", target.Name, "error", err)
		} else {
			record.Status = "completed"
			slog.Info("backup completed", "target", target.Name, "objects", record.ObjectCount, "bytes", record.TotalSize)
		}
		s.store.PutBackupRecord(record)
	}
}

func (s *Scheduler) backupToTarget(target config.BackupTarget, backupType string, record *metadata.BackupRecord) error {
	t, err := NewTarget(target)
	if err != nil {
		return err
	}
	defer t.Close()

	var lastBackupTime int64
	if backupType == "incremental" {
		records, _ := s.store.ListBackupRecords(1)
		for _, r := range records {
			if r.Target == target.Name && r.Status == "completed" {
				lastBackupTime = r.EndTime
				break
			}
		}
	}

	buckets, err := s.store.ListBuckets()
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}

	for _, bucket := range buckets {
		objects, _, err := s.engine.ListObjects(bucket.Name, "", "", 0)
		if err != nil {
			continue
		}

		for _, obj := range objects {
			if backupType == "incremental" && lastBackupTime > 0 {
				if obj.LastModified <= lastBackupTime {
					continue
				}
			}

			reader, _, err := s.engine.GetObject(bucket.Name, obj.Key)
			if err != nil {
				continue
			}

			err = t.Write(bucket.Name, obj.Key, reader, obj.Size)
			reader.Close()
			if err != nil {
				return fmt.Errorf("write %s/%s: %w", bucket.Name, obj.Key, err)
			}

			record.ObjectCount++
			record.TotalSize += obj.Size
		}
	}

	return nil
}

// TriggerBackup triggers an immediate backup.
func (s *Scheduler) TriggerBackup() string {
	if s.running.Load() {
		return "backup already running"
	}
	go s.runBackup()
	return "backup started"
}

// IsRunning returns whether a backup is currently in progress.
func (s *Scheduler) IsRunning() bool {
	return s.running.Load()
}

// ListRecords returns backup history.
func (s *Scheduler) ListRecords(limit int) ([]metadata.BackupRecord, error) {
	return s.store.ListBackupRecords(limit)
}
