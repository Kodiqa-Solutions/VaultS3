package lifecycle

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// walkGuardStore fails the test when the worker calls back into the store
// from inside a ScanObjects or ScanObjectVersions callback. Those walks hold a
// read transaction for their whole length, so a nested call deadlocks the
// store once a writer queues behind it.
type walkGuardStore struct {
	*metadata.Store
	t      *testing.T
	inWalk atomic.Bool
	// beforeVisit, when set, runs after the walk has taken its snapshot and
	// before the worker sees it, the way a PUT lands while a long scan runs.
	beforeVisit func()
}

func (g *walkGuardStore) nested(op string) {
	if g.inWalk.Load() {
		g.t.Errorf("store.%s called from inside a scan callback", op)
	}
}

func (g *walkGuardStore) ScanObjects(fn func(metadata.ObjectMeta) bool) error {
	var snap []metadata.ObjectMeta
	if err := g.Store.ScanObjects(func(m metadata.ObjectMeta) bool { snap = append(snap, m); return true }); err != nil {
		return err
	}
	if g.beforeVisit != nil {
		g.beforeVisit()
	}
	g.inWalk.Store(true)
	defer g.inWalk.Store(false)
	for _, m := range snap {
		if !fn(m) {
			break
		}
	}
	return nil
}

func (g *walkGuardStore) ScanObjectVersions(fn func(metadata.ObjectMeta) bool) error {
	g.inWalk.Store(true)
	defer g.inWalk.Store(false)
	return g.Store.ScanObjectVersions(fn)
}

func (g *walkGuardStore) GetBucketVersioning(b string) (string, error) {
	g.nested("GetBucketVersioning")
	return g.Store.GetBucketVersioning(b)
}
func (g *walkGuardStore) GetObjectMeta(b, k string) (*metadata.ObjectMeta, error) {
	g.nested("GetObjectMeta")
	return g.Store.GetObjectMeta(b, k)
}
func (g *walkGuardStore) DeleteObjectMeta(b, k string) error {
	g.nested("DeleteObjectMeta")
	return g.Store.DeleteObjectMeta(b, k)
}
func (g *walkGuardStore) PutObjectMeta(m metadata.ObjectMeta) error {
	g.nested("PutObjectMeta")
	return g.Store.PutObjectMeta(m)
}
func (g *walkGuardStore) PutObjectVersion(m metadata.ObjectMeta) error {
	g.nested("PutObjectVersion")
	return g.Store.PutObjectVersion(m)
}
func (g *walkGuardStore) DeleteObjectVersion(b, k, v string) error {
	g.nested("DeleteObjectVersion")
	return g.Store.DeleteObjectVersion(b, k, v)
}
func (g *walkGuardStore) GetObjectVersion(b, k, v string) (*metadata.ObjectMeta, error) {
	g.nested("GetObjectVersion")
	return g.Store.GetObjectVersion(b, k, v)
}

func newGuardedWorker(t *testing.T, versioning string) (*Worker, *walkGuardStore, *mockEngine) {
	t.Helper()
	g := &walkGuardStore{Store: newTestStore(t), t: t}
	g.Store.CreateBucket("b")
	if versioning != "" {
		g.Store.SetBucketVersioning("b", versioning)
	}
	g.Store.PutLifecycleRule("b", metadata.LifecycleRule{Status: "Enabled", ExpirationDays: 1, NoncurrentVersionExpirationDays: 1})
	eng := &mockEngine{}
	w := NewWorker(g, eng, 3600, 0)
	w.SetReaper(func(bucket, key, versionID string) {
		if g.inWalk.Load() {
			t.Errorf("reaper (a network call) ran inside a scan callback")
		}
	})
	return w, g, eng
}

var old = time.Now().UTC().Unix() - 10*86400

func TestScanMakesNoStoreCallsInsideTheWalk(t *testing.T) {
	w, g, eng := newGuardedWorker(t, "")
	g.Store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "k", ETag: "e1", LastModified: old})
	w.scan()
	if len(eng.deleted) != 1 {
		t.Fatalf("expired object not deleted: %v", eng.deleted)
	}
	if _, err := g.Store.GetObjectMeta("b", "k"); err == nil {
		t.Fatal("expired object's metadata survived")
	}
}

// A PUT that replaced the key after the scan read it must not be expired: the
// scan's snapshot describes the old object, and deleting by key destroyed the
// new one.
func TestScanSkipsObjectOverwrittenSinceSnapshot(t *testing.T) {
	w, g, eng := newGuardedWorker(t, "")
	g.Store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "k", ETag: "old", LastModified: old})
	g.beforeVisit = func() {
		g.Store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "k", ETag: "new", LastModified: time.Now().Unix()})
	}
	w.scan()
	if len(eng.deleted) != 0 {
		t.Fatalf("an object written during the scan was deleted: %v", eng.deleted)
	}
	if m, err := g.Store.GetObjectMeta("b", "k"); err != nil || m.ETag != "new" {
		t.Fatalf("new object's metadata lost (meta %v, err %v)", m, err)
	}
}

// In a versioning-enabled bucket an object that predates versioning (no version
// id) must be hidden behind a delete marker and kept as the "null" version, not
// destroyed.
func TestExpiryInEnabledBucketWritesDeleteMarker(t *testing.T) {
	w, g, eng := newGuardedWorker(t, "Enabled")
	// Expiry only: a noncurrent rule in the same pass would then (correctly)
	// remove the null version this test is looking for.
	g.Store.PutLifecycleRule("b", metadata.LifecycleRule{Status: "Enabled", ExpirationDays: 1})
	g.Store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "k", ETag: "e", LastModified: old})
	w.scan()
	if len(eng.deleted) != 0 {
		t.Fatalf("data of a versioned bucket's object was destroyed: %v", eng.deleted)
	}
	cur, err := g.Store.GetObjectMeta("b", "k")
	if err != nil || !cur.DeleteMarker {
		t.Fatalf("latest pointer is not a delete marker (meta %v, err %v)", cur, err)
	}
	if v, err := g.Store.GetObjectVersion("b", "k", "null"); err != nil || v.ETag != "e" || v.IsLatest {
		t.Fatalf("object not kept as the noncurrent null version (v %v, err %v)", v, err)
	}
}

func TestExpiryInSuspendedBucketWritesNullMarker(t *testing.T) {
	w, g, eng := newGuardedWorker(t, "Suspended")
	g.Store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "k", ETag: "e", LastModified: old})
	w.scan()
	if len(eng.deleted) != 1 {
		t.Fatalf("suspended bucket: the null version's data should go, deleted %v", eng.deleted)
	}
	cur, err := g.Store.GetObjectMeta("b", "k")
	if err != nil || !cur.DeleteMarker || cur.VersionID != "null" {
		t.Fatalf("latest pointer is not a null delete marker (meta %v, err %v)", cur, err)
	}
}

// Object lock protects noncurrent versions too: a legal hold or unexpired
// retention must stop lifecycle from destroying them.
func TestNoncurrentExpiryRespectsObjectLock(t *testing.T) {
	w, g, _ := newGuardedWorker(t, "Enabled")
	now := time.Now().UTC().Unix()
	g.Store.PutObjectVersion(metadata.ObjectMeta{Bucket: "b", Key: "k", VersionID: "v1", ETag: "1", LastModified: old, LegalHold: true})
	g.Store.PutObjectVersion(metadata.ObjectMeta{Bucket: "b", Key: "k", VersionID: "v2", ETag: "2", LastModified: old + 1, RetentionMode: "GOVERNANCE", RetentionUntil: now + 86400})
	g.Store.PutObjectVersion(metadata.ObjectMeta{Bucket: "b", Key: "k", VersionID: "v3", ETag: "3", LastModified: old + 2})
	latest := metadata.ObjectMeta{Bucket: "b", Key: "k", VersionID: "v4", ETag: "4", LastModified: now, IsLatest: true}
	g.Store.PutObjectVersion(latest)
	g.Store.PutObjectMeta(latest)
	w.scan()
	for _, v := range []string{"v1", "v2"} {
		if _, err := g.Store.GetObjectVersion("b", "k", v); err != nil {
			t.Fatalf("locked noncurrent version %s was deleted", v)
		}
	}
	if _, err := g.Store.GetObjectVersion("b", "k", "v3"); err == nil {
		t.Fatal("unlocked expired noncurrent version v3 was kept")
	}
}

// failingVersionEngine cannot delete version data.
type failingVersionEngine struct{ mockEngine }

func (f *failingVersionEngine) DeleteObjectVersion(string, string, string) error {
	return errors.New("disk refused")
}

// When the data cannot be deleted the version record must stay, or the bytes
// are orphaned with nothing pointing at them, and it must not count as done.
func TestNoncurrentExpiryKeepsRecordWhenDataDeleteFails(t *testing.T) {
	g := &walkGuardStore{Store: newTestStore(t), t: t}
	g.Store.CreateBucket("b")
	g.Store.SetBucketVersioning("b", "Enabled")
	g.Store.PutLifecycleRule("b", metadata.LifecycleRule{Status: "Enabled", NoncurrentVersionExpirationDays: 1})
	g.Store.PutObjectVersion(metadata.ObjectMeta{Bucket: "b", Key: "k", VersionID: "v1", ETag: "1", LastModified: old})
	latest := metadata.ObjectMeta{Bucket: "b", Key: "k", VersionID: "v2", ETag: "2", LastModified: time.Now().Unix(), IsLatest: true}
	g.Store.PutObjectVersion(latest)
	g.Store.PutObjectMeta(latest)

	w := NewWorker(g, &failingVersionEngine{}, 3600, 0)
	w.scan()
	if _, err := g.Store.GetObjectVersion("b", "k", "v1"); err != nil {
		t.Fatal("version record deleted although its data could not be")
	}
}
