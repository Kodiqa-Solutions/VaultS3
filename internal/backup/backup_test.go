package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

func newBackupRig(t *testing.T) (*storage.FileSystem, *metadata.Store, string) {
	t.Helper()
	base := t.TempDir()

	eng, err := storage.NewFileSystem(filepath.Join(base, "data"))
	if err != nil {
		t.Fatalf("data fs: %v", err)
	}
	store, err := metadata.NewStore(filepath.Join(base, "meta.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if err := eng.CreateBucketDir("b"); err != nil {
		t.Fatalf("create bucket dir: %v", err)
	}
	if err := store.CreateBucket("b"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return eng, store, base
}

// putObj stores an object the way the S3 handler does: data through the
// engine and a metadata record, which is what a backup enumerates.
func putObj(t *testing.T, eng *storage.FileSystem, store *metadata.Store, key string, data []byte, modTime time.Time) {
	t.Helper()
	_, etag, err := eng.PutObject("b", key, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: key, ETag: etag, Size: int64(len(data)), LastModified: modTime.Unix()}); err != nil {
		t.Fatalf("put meta %s: %v", key, err)
	}
}

func assertFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backed-up file %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("backed-up file %s is not byte-identical to source", path)
	}
}

// TestLocalTargetRestoreRoundTrip: a LocalTarget write is recoverable
// byte-for-byte by reading the file back (restore is a manual file copy).
func TestLocalTargetRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tgt, err := NewLocalTarget(dir)
	if err != nil {
		t.Fatalf("NewLocalTarget: %v", err)
	}
	defer tgt.Close()

	data := []byte("the quick brown fox")
	if err := tgt.Write("b", "nested/path/obj.bin", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertFileEquals(t, filepath.Join(dir, "b", "nested/path/obj.bin"), data)
}

// TestLocalTargetRejectsTraversal: keys that escape the base path are refused.
func TestLocalTargetRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	tgt, _ := NewLocalTarget(dir)
	defer tgt.Close()

	// Key must escape the base path entirely to trip the guard.
	err := tgt.Write("b", "../../escape", bytes.NewReader([]byte("x")), 1)
	if err == nil {
		t.Fatal("expected path-traversal write to be rejected")
	}
}

// TestFullBackupCopiesEverything: a full backup writes every object, and each
// backed-up copy restores byte-identically.
func TestFullBackupCopiesEverything(t *testing.T) {
	eng, store, base := newBackupRig(t)
	alpha, bravo := []byte("alpha-contents"), []byte("bravo-contents")
	putObj(t, eng, store, "a.txt", alpha, time.Now())
	putObj(t, eng, store, "dir/b.txt", bravo, time.Now())

	targetDir := filepath.Join(base, "backup")
	target := config.BackupTarget{Name: "local", Type: "local", Path: targetDir}
	s := NewScheduler(store, eng, config.BackupConfig{Targets: []config.BackupTarget{target}})

	rec := metadata.BackupRecord{}
	if err := s.backupToTarget(target, "full", &rec); err != nil {
		t.Fatalf("full backup: %v", err)
	}

	if rec.ObjectCount != 2 {
		t.Fatalf("ObjectCount = %d, want 2", rec.ObjectCount)
	}
	assertFileEquals(t, filepath.Join(targetDir, "b", "a.txt"), alpha)
	assertFileEquals(t, filepath.Join(targetDir, "b", "dir/b.txt"), bravo)
}

// TestIncrementalBackupCopiesOnlyChanged: an incremental backup copies only
// objects modified after the last completed backup, not the unchanged ones.
func TestIncrementalBackupCopiesOnlyChanged(t *testing.T) {
	eng, store, base := newBackupRig(t)
	now := time.Now()

	// "old" predates the last backup; "new" was modified after it.
	putObj(t, eng, store, "old.txt", []byte("old data"), now.Add(-2*time.Hour))
	putObj(t, eng, store, "new.txt", []byte("new data"), now)

	targetDir := filepath.Join(base, "backup")
	target := config.BackupTarget{Name: "local", Type: "local", Path: targetDir}
	s := NewScheduler(store, eng, config.BackupConfig{
		Targets:     []config.BackupTarget{target},
		Incremental: true,
	})

	// A prior completed backup, finished one hour ago, is the incremental baseline.
	if err := store.PutBackupRecord(metadata.BackupRecord{
		ID:      "prev",
		Type:    "full",
		Target:  "local",
		Status:  "completed",
		EndTime: now.Add(-1 * time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("seed backup record: %v", err)
	}

	rec := metadata.BackupRecord{}
	if err := s.backupToTarget(target, "incremental", &rec); err != nil {
		t.Fatalf("incremental backup: %v", err)
	}

	if rec.ObjectCount != 1 {
		t.Fatalf("incremental copied %d objects, want 1 (only the changed one)", rec.ObjectCount)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "b", "old.txt")); err == nil {
		t.Fatal("unchanged object was copied by incremental backup")
	}
	assertFileEquals(t, filepath.Join(targetDir, "b", "new.txt"), []byte("new data"))
}

// TestListRecordsRoundTrip: backup records persist and are retrievable.
func TestListRecordsRoundTrip(t *testing.T) {
	eng, store, _ := newBackupRig(t)
	s := NewScheduler(store, eng, config.BackupConfig{})

	if err := store.PutBackupRecord(metadata.BackupRecord{
		ID: "rec-1", Target: "local", Status: "completed", EndTime: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("put record: %v", err)
	}

	records, err := s.ListRecords(10)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(records) != 1 || records[0].ID != "rec-1" {
		t.Fatalf("ListRecords returned %+v, want one record rec-1", records)
	}
}
