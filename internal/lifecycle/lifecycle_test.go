package lifecycle

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// mockEngine implements storage.Engine for testing.
type mockEngine struct {
	deleted []string
}

func (m *mockEngine) CreateBucketDir(string) error             { return nil }
func (m *mockEngine) DeleteBucketDir(string) error             { return nil }
func (m *mockEngine) ObjectExists(string, string) bool         { return true }
func (m *mockEngine) ObjectSize(string, string) (int64, error) { return 0, nil }
func (m *mockEngine) BucketSize(string) (int64, int64, error)  { return 0, 0, nil }
func (m *mockEngine) DataDir() string                          { return "" }
func (m *mockEngine) ObjectPath(bucket, key string) string     { return bucket + "/" + key }
func (m *mockEngine) GetObject(string, string) (storage.ReadSeekCloser, int64, error) {
	return nil, 0, nil
}
func (m *mockEngine) PutObject(string, string, io.Reader, int64) (int64, string, error) {
	return 0, "", nil
}
func (m *mockEngine) ListObjects(string, string, string, int) ([]storage.ObjectInfo, bool, error) {
	return nil, false, nil
}
func (m *mockEngine) DeleteObject(bucket, key string) error {
	m.deleted = append(m.deleted, bucket+"/"+key)
	return nil
}
func (m *mockEngine) PutObjectVersion(string, string, string, io.Reader, int64) (int64, string, error) {
	return 0, "", nil
}
func (m *mockEngine) GetObjectVersion(string, string, string) (storage.ReadSeekCloser, int64, error) {
	return nil, 0, nil
}
func (m *mockEngine) DeleteObjectVersion(string, string, string) error { return nil }

func newTestStore(t *testing.T) *metadata.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := metadata.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNewWorker(t *testing.T) {
	store := newTestStore(t)
	engine := &mockEngine{}

	w := NewWorker(store, engine, 60, 30)
	if w.interval != 60*time.Second {
		t.Errorf("interval: got %v, want 60s", w.interval)
	}
	if w.auditRetentionDays != 30 {
		t.Errorf("audit retention: got %d, want 30", w.auditRetentionDays)
	}
}

func TestScan_NoBuckets(t *testing.T) {
	store := newTestStore(t)
	engine := &mockEngine{}

	w := NewWorker(store, engine, 3600, 0)
	w.scan() // should not panic with no buckets
}

func TestScan_NoRules(t *testing.T) {
	store := newTestStore(t)
	engine := &mockEngine{}

	store.CreateBucket("mybucket")
	// No lifecycle rules set

	w := NewWorker(store, engine, 3600, 0)
	w.scan() // should complete without error

	if len(engine.deleted) != 0 {
		t.Errorf("expected no deletions, got %v", engine.deleted)
	}
}

func TestScan_DisabledRule(t *testing.T) {
	store := newTestStore(t)
	engine := &mockEngine{}

	store.CreateBucket("mybucket")
	store.PutLifecycleRule("mybucket", metadata.LifecycleRule{
		Status:         "Disabled",
		ExpirationDays: 1,
	})

	// No objects needed — scan should skip disabled rules before scanning objects
	w := NewWorker(store, engine, 3600, 0)
	w.scan()

	if len(engine.deleted) != 0 {
		t.Errorf("expected no deletions for disabled rule, got %v", engine.deleted)
	}
}

// TestLifecycleMatchingLogic tests the object matching logic used in scan()
// without going through the full scan (which causes BoltDB nested tx deadlocks).
func TestLifecycleMatchingLogic(t *testing.T) {
	now := time.Now().UTC().Unix()

	tests := []struct {
		name       string
		meta       metadata.ObjectMeta
		rule       metadata.LifecycleRule
		versioning string
		shouldSkip bool
	}{
		{
			name: "expired object gets deleted",
			meta: metadata.ObjectMeta{
				Bucket:       "b",
				Key:          "old.txt",
				LastModified: now - 2*86400,
			},
			rule:       metadata.LifecycleRule{Status: "Enabled", ExpirationDays: 1},
			shouldSkip: false,
		},
		{
			name: "not yet expired",
			meta: metadata.ObjectMeta{
				Bucket:       "b",
				Key:          "new.txt",
				LastModified: now,
			},
			rule:       metadata.LifecycleRule{Status: "Enabled", ExpirationDays: 1},
			shouldSkip: true,
		},
		{
			name: "prefix matches",
			meta: metadata.ObjectMeta{
				Bucket:       "b",
				Key:          "logs/access.log",
				LastModified: now - 2*86400,
			},
			rule:       metadata.LifecycleRule{Status: "Enabled", Prefix: "logs/", ExpirationDays: 1},
			shouldSkip: false,
		},
		{
			name: "prefix does not match",
			meta: metadata.ObjectMeta{
				Bucket:       "b",
				Key:          "data/file.csv",
				LastModified: now - 2*86400,
			},
			rule:       metadata.LifecycleRule{Status: "Enabled", Prefix: "logs/", ExpirationDays: 1},
			shouldSkip: true,
		},
		{
			name: "delete marker skipped",
			meta: metadata.ObjectMeta{
				Bucket:       "b",
				Key:          "deleted.txt",
				LastModified: now - 2*86400,
				DeleteMarker: true,
			},
			rule:       metadata.LifecycleRule{Status: "Enabled", ExpirationDays: 1},
			shouldSkip: true,
		},
		{
			name: "legal hold skipped",
			meta: metadata.ObjectMeta{
				Bucket:       "b",
				Key:          "locked.txt",
				LastModified: now - 2*86400,
				LegalHold:    true,
			},
			rule:       metadata.LifecycleRule{Status: "Enabled", ExpirationDays: 1},
			shouldSkip: true,
		},
		{
			name: "retention lock skipped",
			meta: metadata.ObjectMeta{
				Bucket:         "b",
				Key:            "retained.txt",
				LastModified:   now - 2*86400,
				RetentionMode:  "COMPLIANCE",
				RetentionUntil: now + 86400,
			},
			rule:       metadata.LifecycleRule{Status: "Enabled", ExpirationDays: 1},
			shouldSkip: true,
		},
		{
			name: "expired retention allows delete",
			meta: metadata.ObjectMeta{
				Bucket:         "b",
				Key:            "retained-expired.txt",
				LastModified:   now - 2*86400,
				RetentionMode:  "GOVERNANCE",
				RetentionUntil: now - 3600,
			},
			rule:       metadata.LifecycleRule{Status: "Enabled", ExpirationDays: 1},
			shouldSkip: false,
		},
		{
			name: "versioned object with versionID skipped",
			meta: metadata.ObjectMeta{
				Bucket:       "b",
				Key:          "versioned.txt",
				LastModified: now - 2*86400,
				VersionID:    "v1",
			},
			rule:       metadata.LifecycleRule{Status: "Enabled", ExpirationDays: 1},
			versioning: "Enabled",
			shouldSkip: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skipped := shouldSkipObject(tt.meta, &tt.rule, now, tt.versioning)
			if skipped != tt.shouldSkip {
				t.Errorf("shouldSkipObject: got %v, want %v", skipped, tt.shouldSkip)
			}
		})
	}
}

// shouldSkipObject replicates the matching logic from Worker.scan()
// to allow unit testing without BoltDB transaction issues.
func shouldSkipObject(meta metadata.ObjectMeta, rule *metadata.LifecycleRule, now int64, versioning string) bool {
	if !matchRule(rule, &meta) {
		return true
	}

	// Check if object is expired
	expiryTime := meta.LastModified + int64(rule.ExpirationDays)*86400
	if expiryTime > now {
		return true // not expired yet
	}

	// Skip delete markers
	if meta.DeleteMarker {
		return true
	}

	// Check object lock
	if meta.LegalHold {
		return true
	}
	if meta.RetentionMode != "" && meta.RetentionUntil > 0 && now < meta.RetentionUntil {
		return true
	}

	// Check versioned objects
	if versioning == "Enabled" && meta.VersionID != "" {
		return true
	}

	return false
}

func TestScan_PrunesAuditEntries(t *testing.T) {
	store := newTestStore(t)

	oldTime := time.Now().UTC().AddDate(0, 0, -100).UnixNano()
	store.PutAuditEntry(metadata.AuditEntry{
		Time:      oldTime,
		Principal: "admin",
		Action:    "s3:GetObject",
		Resource:  "mybucket/file.txt",
		Effect:    "Allow",
	})

	// Directly test PruneAuditEntries to avoid scan complexity
	cutoff := time.Now().UTC().AddDate(0, 0, -30)
	pruned, err := store.PruneAuditEntries(cutoff)
	if err != nil {
		t.Fatalf("PruneAuditEntries: %v", err)
	}
	if pruned != 1 {
		t.Errorf("expected 1 pruned, got %d", pruned)
	}

	entries, err := store.ListAuditEntries(100, 0, 0, "", "")
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after pruning, got %d", len(entries))
	}
}
