package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// The run writes its own "running" record before it looks for a baseline, so
// the newest record is never a completed one. Looking only at the newest made
// every incremental run a full copy.
func TestIncrementalRunUsesLastCompletedBackup(t *testing.T) {
	eng, store, base := newBackupRig(t)
	past := time.Now().Add(-time.Hour)
	putObj(t, eng, store, "a.txt", []byte("a"), past)
	putObj(t, eng, store, "b.txt", []byte("b"), past)

	target := config.BackupTarget{Name: "local", Type: "local", Path: filepath.Join(base, "backup")}
	s := NewScheduler(store, eng, config.BackupConfig{Targets: []config.BackupTarget{target}, Incremental: true})

	s.runBackup()
	s.runBackup()
	records, err := store.ListBackupRecords(0)
	if err != nil || len(records) != 2 {
		t.Fatalf("want 2 backup records, got %d (err %v)", len(records), err)
	}
	first, second := records[1], records[0]
	if first.Status != "completed" || first.ObjectCount != 2 {
		t.Fatalf("first run: status %s, copied %d, want completed and 2", first.Status, first.ObjectCount)
	}
	if second.Status != "completed" || second.ObjectCount != 0 {
		t.Fatalf("second incremental run copied %d unchanged objects (status %s), want 0", second.ObjectCount, second.Status)
	}
}

// Backups hold every object's plaintext and the metadata database, so nobody
// but the server's user may read them.
func TestBackupFilesArePrivate(t *testing.T) {
	eng, store, base := newBackupRig(t)
	putObj(t, eng, store, "dir/a.txt", []byte("secret"), time.Now())
	dir := filepath.Join(base, "backup")
	target := config.BackupTarget{Name: "local", Type: "local", Path: dir}
	s := NewScheduler(store, eng, config.BackupConfig{Targets: []config.BackupTarget{target}})
	if err := s.backupToTarget(target, "full", &metadata.BackupRecord{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "b", "dir", "a.txt"), filepath.Join(dir, metadataDir, MetadataFile)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s has mode %v, want no group or other access", p, fi.Mode().Perm())
		}
	}
	for _, d := range []string{dir, filepath.Join(dir, "b"), filepath.Join(dir, "b", "dir")} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("directory %s has mode %v, want no group or other access", d, fi.Mode().Perm())
		}
	}
}

// The current version of an object in a versioned bucket lives under .vs/,
// which the engine's file listing skips, so versioned buckets were not backed
// up at all. The metadata database was not backed up either.
func TestBackupIncludesVersionedObjectsAndMetadata(t *testing.T) {
	eng, store, base := newBackupRig(t)
	data := []byte("versioned current")
	if _, _, err := eng.PutObjectVersion("b", "v.txt", "0001", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	meta := metadata.ObjectMeta{Bucket: "b", Key: "v.txt", VersionID: "0001", IsLatest: true, Size: int64(len(data)), LastModified: time.Now().Unix()}
	store.PutObjectVersion(meta)
	store.PutObjectMeta(meta)

	dir := filepath.Join(base, "backup")
	target := config.BackupTarget{Name: "local", Type: "local", Path: dir}
	s := NewScheduler(store, eng, config.BackupConfig{Targets: []config.BackupTarget{target}})
	if err := s.backupToTarget(target, "full", &metadata.BackupRecord{}); err != nil {
		t.Fatal(err)
	}
	assertFileEquals(t, filepath.Join(dir, "b", "v.txt"), data)

	// The metadata copy opens as a store and holds the object's record.
	copyPath := filepath.Join(t.TempDir(), "restored.db")
	raw, err := os.ReadFile(filepath.Join(dir, metadataDir, MetadataFile))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(copyPath, raw, 0o600)
	restored, err := metadata.NewStore(copyPath)
	if err != nil {
		t.Fatalf("metadata copy does not open: %v", err)
	}
	defer restored.Close()
	if m, err := restored.GetObjectVersion("b", "v.txt", "0001"); err != nil || m.Size != int64(len(data)) {
		t.Fatalf("metadata copy is missing the version record (m %v, err %v)", m, err)
	}
}

// An object that cannot be read must fail the run, not be skipped while the
// backup reports success.
func TestBackupReportsUnreadableObjects(t *testing.T) {
	eng, store, base := newBackupRig(t)
	putObj(t, eng, store, "ok.txt", []byte("ok"), time.Now())
	store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "lost.txt", Size: 4, LastModified: time.Now().Unix()})
	target := config.BackupTarget{Name: "local", Type: "local", Path: filepath.Join(base, "backup")}
	s := NewScheduler(store, eng, config.BackupConfig{Targets: []config.BackupTarget{target}})
	rec := metadata.BackupRecord{}
	if err := s.backupToTarget(target, "full", &rec); err == nil {
		t.Fatal("backup with an unreadable object reported success")
	}
	if rec.ObjectCount != 1 {
		t.Fatalf("copied %d objects, want the readable one", rec.ObjectCount)
	}
}
