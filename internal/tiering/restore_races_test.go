package tiering

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// A1: suspended bucket. Pre-versioning object moved cold by 5.0.10, then a
// suspended PUT replaced the null version (data now in .vs/x/null). The cold
// plain file is stale. Restore should not bring it back.
func TestRestoreSkipsAPlainFileReplacedByASuspendedNullVersion(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "x", []byte("OLD pre-versioning bytes"), time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "x"); err != nil {
		t.Fatal(err)
	}
	newer := []byte("NEW null version")
	r.hot.PutObjectVersion("b", "x", "null", bytes.NewReader(newer), int64(len(newer)))
	cur := metadata.ObjectMeta{Bucket: "b", Key: "x", VersionID: "null", IsLatest: true, Size: int64(len(newer)), LastModified: time.Now().Unix()}
	r.store.PutObjectVersion(cur)
	r.store.PutObjectMeta(cur)
	err := r.mgr.MigrateToHot("b", "x")
	if _, serr := os.Stat(r.hot.ObjectPath("b", "x")); serr == nil {
		got, _ := os.ReadFile(r.hot.ObjectPath("b", "x"))
		t.Fatalf("restore returned %v and wrote stale bytes %q at the plain path, referenced by no record", err, got)
	}
}

// A1b: Enabled again, current V3, live "null" version is a suspended null in .vs.
func TestRestoreSkipsAPlainFileBelowASuspendedNullVersion(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "y", []byte("OLD pre-versioning bytes"), time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "y"); err != nil {
		t.Fatal(err)
	}
	nul := []byte("suspended null")
	r.hot.PutObjectVersion("b", "y", "null", bytes.NewReader(nul), int64(len(nul)))
	nm := metadata.ObjectMeta{Bucket: "b", Key: "y", VersionID: "null", Size: int64(len(nul))}
	r.store.PutObjectVersion(nm)
	cur := metadata.ObjectMeta{Bucket: "b", Key: "y", VersionID: "0000000000000009aaaaaaaa", IsLatest: true, Size: 3}
	r.store.PutObjectVersion(cur)
	r.store.PutObjectMeta(cur)
	err := r.mgr.MigrateToHot("b", "y")
	if _, serr := os.Stat(r.hot.ObjectPath("b", "y")); serr == nil {
		t.Fatalf("restore returned %v and wrote stale bytes at the plain path", err)
	}
}

// B: a DELETE whose data step ran before the link and whose record step ran
// after the post-link re-read leaves the restored bytes behind.
type hookStore struct {
	metadata.StoreAPI
	calls int
	after func()
}

func (h *hookStore) GetObjectMeta(b, k string) (*metadata.ObjectMeta, error) {
	m, err := h.StoreAPI.GetObjectMeta(b, k)
	h.calls++
	if h.calls == 2 && h.after != nil {
		h.after()
	}
	return m, err
}

func TestRestoreRacingADeleteAfterTheRecheckLeavesNoOrphan(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "d", []byte("to be deleted"), time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "d"); err != nil {
		t.Fatal(err)
	}
	hs := &hookStore{StoreAPI: r.store}
	hs.after = func() { r.store.DeleteObjectMeta("b", "d") }
	m := NewManager(hs, r.hot, r.cold, 30, 3600)
	m.beforeRestoreLink = func() { r.hot.DeleteObject("b", "d") } // DELETE's data step
	err := m.MigrateToHot("b", "d")
	if _, serr := os.Stat(r.hot.ObjectPath("b", "d")); serr == nil {
		if mm, _ := r.store.GetObjectMeta("b", "d"); mm == nil {
			t.Fatalf("restore returned %v; bytes left in hot with no record", err)
		}
	}
}

// C: re-read sees the record gone, then a DELETE+PUT... (PUT lands between
// the re-read and os.Remove): the PUT's file is removed.
func TestRestoreCleanupKeepsAPutThatLandedAfterTheRecheck(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "p", []byte("old"), time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "p"); err != nil {
		t.Fatal(err)
	}
	newer := []byte("NEW PUT after delete")
	hs := &hookStore{StoreAPI: r.store}
	hs.after = func() {
		// PUT: rename onto the hot path, then record
		r.hot.PutObject("b", "p", bytes.NewReader(newer), int64(len(newer)))
		r.store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "p", Size: int64(len(newer)), LastModified: time.Now().Unix()})
	}
	m := NewManager(hs, r.hot, r.cold, 30, 3600)
	m.beforeRestoreLink = func() { r.store.DeleteObjectMeta("b", "p") }
	m.MigrateToHot("b", "p")
	if mm, _ := r.store.GetObjectMeta("b", "p"); mm != nil {
		if _, err := os.Stat(r.hot.ObjectPath("b", "p")); err != nil {
			t.Fatalf("a PUT that landed after the delete has a record but its data was removed by restore cleanup")
		}
	}
}

// D: non-slash key whose cold path is a directory (cold has "a/b"), record
// hot, hot "a" is a regular file: restore answers?
func TestRestoreOfAColdFolderUnderAFileKeepsTheFile(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "a/b", []byte("child"), time.Now().Unix())
	if err := r.mgr.migrateToCol("b", "a/b"); err != nil {
		t.Fatal(err)
	}
	r.store.DeleteObjectMeta("b", "a/b")
	r.putHot(t, "a", []byte("file a"), time.Now().Unix())
	err := r.mgr.MigrateToHot("b", "a")
	t.Logf("restore of a: %v", err)
	if got := r.read(t, r.hot, "a"); string(got) != "file a" {
		t.Fatalf("hot a = %q", got)
	}
}
