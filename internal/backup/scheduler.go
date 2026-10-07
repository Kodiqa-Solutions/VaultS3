package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	store       metadata.StoreAPI
	engine      storage.Engine
	cfg         config.BackupConfig
	lastRunHour int
	running     atomic.Bool
}

func NewScheduler(store metadata.StoreAPI, engine storage.Engine, cfg config.BackupConfig) *Scheduler {
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
	// Claim the running flag atomically. The Load checks in shouldRun/TriggerBackup
	// are only a fast path; this compare-and-swap is the real guard that prevents
	// two concurrent triggers (or a trigger racing the ticker) from both starting a
	// backup and writing the same target directory at once.
	if !s.running.CompareAndSwap(false, true) {
		return
	}
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

// backupPageSize is how many objects one metadata page holds while a backup
// walks a bucket.
const backupPageSize = 1000

// lastCompleted returns the most recent completed backup to target, if any.
// The run that is calling it has already written its own "running" record, so
// looking only at the newest record (as this used to) always found that one,
// never a completed run, and every incremental backup silently became a full
// copy.
func (s *Scheduler) lastCompleted(target string) (metadata.BackupRecord, bool) {
	records, err := s.store.ListBackupRecords(0)
	if err != nil {
		return metadata.BackupRecord{}, false
	}
	for _, r := range records { // newest first
		if r.Target == target && r.Status == "completed" {
			return r, true
		}
	}
	return metadata.BackupRecord{}, false
}

// backupToTarget copies the current version of every object, then a
// consistent copy of the metadata database, into the target.
//
// Objects are enumerated from the metadata store, not from the storage
// engine's file listing. The file listing never saw the current version of an
// object in a versioned bucket (those live under .vs/, which it skips), so
// versioned buckets were not backed up at all. The metadata copy is what makes
// the backup restorable: without it the object files carry no versions, tags,
// ACLs, users or bucket settings.
//
// What a backup still does NOT contain: noncurrent versions and delete
// markers (only their records, inside the metadata copy), objects moved to the
// cold tier (they are not readable through this engine and are reported as
// failures), and incomplete multipart uploads. Object data is read through the
// engine, so it is written to the target DECRYPTED. Protect the target
// accordingly.
func (s *Scheduler) backupToTarget(target config.BackupTarget, backupType string, record *metadata.BackupRecord) error {
	t, err := NewTarget(target)
	if err != nil {
		return err
	}
	defer t.Close()

	// The baseline is when the last completed backup STARTED: an object written
	// while that run was copying may have been copied before it changed, so it
	// has to be picked up again. Records written before StartTime existed only
	// have EndTime.
	var lastBackupTime int64
	if backupType == "incremental" {
		if prev, ok := s.lastCompleted(target.Name); ok {
			lastBackupTime = prev.StartTime
			if lastBackupTime == 0 {
				lastBackupTime = prev.EndTime
			}
		}
	}

	buckets, err := s.store.ListBuckets()
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}

	// A copy that fails is counted and reported, and the run is marked failed,
	// instead of being skipped while the backup reports success.
	var failed int64
	var firstErr error
	fail := func(err error) {
		failed++
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, bucket := range buckets {
		after := ""
		for {
			objects, truncated, err := s.store.ListLatestObjects(bucket.Name, "", after, backupPageSize)
			if err != nil {
				fail(fmt.Errorf("list %s: %w", bucket.Name, err))
				break
			}
			for _, obj := range objects {
				after = obj.Key
				if backupType == "incremental" && lastBackupTime > 0 && obj.LastModified < lastBackupTime {
					continue
				}
				n, err := s.copyObject(t, obj)
				if err != nil {
					if isTargetError(err) {
						return fmt.Errorf("write %s/%s: %w", bucket.Name, obj.Key, err)
					}
					fail(fmt.Errorf("read %s/%s: %w", bucket.Name, obj.Key, err))
					continue
				}
				record.ObjectCount++
				record.TotalSize += n
			}
			if !truncated || len(objects) == 0 {
				break
			}
		}
	}

	if err := t.WriteMetadata(func(w io.Writer) error {
		_, err := s.store.BackupDB(w)
		return err
	}); err != nil {
		return fmt.Errorf("write metadata copy: %w", err)
	}

	if failed > 0 {
		return fmt.Errorf("%d object(s) could not be backed up, first: %w", failed, firstErr)
	}
	return nil
}

// targetError marks a failure to write the target, which stops the run: the
// destination is the problem, so every further object would fail the same way.
type targetError struct{ err error }

func (e targetError) Error() string { return e.err.Error() }
func (e targetError) Unwrap() error { return e.err }

func isTargetError(err error) bool {
	var te targetError
	return errors.As(err, &te)
}

// copyObject reads the current version of one object and writes it to t. An
// object that predates versioning (no version id, or the "null" version) keeps
// its bytes at the ordinary object path. Every other version lives in the
// engine's version store.
func (s *Scheduler) copyObject(t Target, obj metadata.ObjectMeta) (int64, error) {
	var reader storage.ReadSeekCloser
	var size int64
	var err error
	if obj.VersionID == "" {
		reader, size, err = s.engine.GetObject(obj.Bucket, obj.Key)
	} else {
		reader, size, err = s.engine.GetObjectVersion(obj.Bucket, obj.Key, obj.VersionID)
		if err != nil && obj.VersionID == "null" {
			reader, size, err = s.engine.GetObject(obj.Bucket, obj.Key)
		}
	}
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	if err := t.Write(obj.Bucket, obj.Key, reader, size); err != nil {
		return 0, targetError{err}
	}
	return size, nil
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
