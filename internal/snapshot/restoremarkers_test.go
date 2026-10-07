package snapshot

import (
	"errors"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Keys added after a snapshot used to have their latest pointer deleted with
// no delete marker. For an object with no version record, one written before
// versioning was enabled, that removed the only record naming it.
func TestRestoreHidesNewKeysBehindDeleteMarkers(t *testing.T) {
	store := newStore(t)
	versionedBucket(t, store, "b")
	m := NewManager(store)
	putVer(t, store, "b", "kept", "v1", "e1")
	snap, err := m.Create("b", "before")
	if err != nil {
		t.Fatal(err)
	}

	// Added after the snapshot: one versioned key, and one with no version id.
	putVer(t, store, "b", "added", "v2", "e2")
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "plain", ETag: "e3", Size: 4, LastModified: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}

	res, err := m.Restore("b", snap.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Removed != 2 || res.Reverted != 1 || res.Failed != 0 {
		t.Fatalf("result %+v, want 2 removed, 1 reverted", res)
	}
	if got := latest(t, store, "b"); len(got) != 1 || got["kept"] != "v1" {
		t.Fatalf("live listing after restore: %v, want only kept@v1", got)
	}
	for _, key := range []string{"added", "plain"} {
		cur, err := store.GetObjectMeta("b", key)
		if err != nil || cur == nil || !cur.DeleteMarker {
			t.Fatalf("%s: latest is %+v (%v), want a delete marker", key, cur, err)
		}
	}
	if v, err := store.GetObjectVersion("b", "added", "v2"); err != nil || v.IsLatest {
		t.Fatalf("added@v2 after restore: %+v %v, want kept as a non-latest version", v, err)
	}
	if v, err := store.GetObjectVersion("b", "plain", "null"); err != nil || v.ETag != "e3" {
		t.Fatalf("the unversioned object was not kept as the null version: %+v %v", v, err)
	}
}

// failingStore fails the writes a restore makes for one key.
type failingStore struct {
	metadata.StoreAPI
	failKey string
}

var errStore = errors.New("store unavailable")

func (f *failingStore) PutObjectVersion(meta metadata.ObjectMeta) error {
	if meta.Key == f.failKey {
		return errStore
	}
	return f.StoreAPI.PutObjectVersion(meta)
}

func (f *failingStore) SetLatestVersion(bucket, key, versionID string) error {
	if key == f.failKey {
		return errStore
	}
	return f.StoreAPI.SetLatestVersion(bucket, key, versionID)
}

// A store failure used to be counted as a skipped (expired) version, or
// ignored outright, and the restore reported success.
func TestRestoreReportsFailuresSeparatelyFromSkips(t *testing.T) {
	store := newStore(t)
	versionedBucket(t, store, "b")
	putVer(t, store, "b", "a", "v1", "e1")
	putVer(t, store, "b", "gone", "v1", "e1")
	snap, err := NewManager(store).Create("b", "s")
	if err != nil {
		t.Fatal(err)
	}
	putVer(t, store, "b", "a", "v2", "e2")
	putVer(t, store, "b", "new", "v3", "e3")
	// The snapshot's version of "gone" expires after it was replaced.
	putVer(t, store, "b", "gone", "v5", "e5")
	if err := store.DeleteObjectVersion("b", "gone", "v1"); err != nil {
		t.Fatal(err)
	}

	m := NewManager(&failingStore{StoreAPI: store, failKey: "new"})
	res, err := m.Restore("b", snap.ID)
	if err == nil {
		t.Fatal("a restore with a failed key reported success")
	}
	if res == nil || res.Failed != 1 || res.Skipped != 1 || res.Reverted != 1 || len(res.Errors) != 1 {
		t.Fatalf("result %+v, want 1 failed (new), 1 skipped (gone), 1 reverted (a)", res)
	}

	m = NewManager(&failingStore{StoreAPI: store, failKey: "a"})
	putVer(t, store, "b", "a", "v4", "e4")
	res, err = m.Restore("b", snap.ID)
	if err == nil || res.Failed != 1 || res.Skipped != 1 {
		t.Fatalf("a failed repoint: %+v %v, want counted as failed, not skipped", res, err)
	}
}
