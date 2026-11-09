package metadata

import (
	"path/filepath"
	"testing"
)

func orderedStore(t *testing.T) *Store {
	t.Helper()
	SetVersionOrdering(true)
	t.Cleanup(func() { SetVersionOrdering(false) })
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.CreateBucket("b")
	return s
}

func flaggedLatest(s *Store) []string {
	vs, _, _ := s.ListObjectVersions("b", "", "", "", 0)
	var out []string
	for _, v := range vs {
		if v.IsLatest {
			out = append(out, v.VersionID)
		}
	}
	return out
}

// Regression (5.0.12 review): a dashboard upload whose latest pointer failed
// put the previous version back before removing its own record, which (still
// flagged latest, with the higher ID) kept the restored one noncurrent: no
// version was flagged latest. The record now goes first.
func TestAFailedPointerWriteRestoresThePreviousLatest(t *testing.T) {
	s := orderedStore(t)
	prev := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588143385000aaaaaaaa", Size: 1, IsLatest: true}
	s.PutObjectVersion(prev)
	s.PutObjectMeta(prev)
	d := prev
	d.IsLatest = false
	s.PutObjectVersion(d) // demoteLatest
	nw := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588146411850bbbbbbbb", Size: 2, IsLatest: true}
	s.PutObjectVersion(nw)
	// PutObjectMeta(nw) fails: what recordNewLatestVersion now does.
	s.DeleteObjectVersion("b", "k", nw.VersionID)
	r := prev
	r.IsLatest = true
	s.PutObjectVersion(r)
	cur, _ := s.GetObjectMeta("b", "k")
	if got := flaggedLatest(s); len(got) != 1 || got[0] != cur.VersionID || cur.VersionID != prev.VersionID {
		t.Fatalf("pointer %s, flagged latest %v", cur.VersionID, got)
	}
}

// Regression (5.0.12 review): a version PutObjectVersion stored as
// noncurrent (a higher one is flagged latest) still moved the pointer when
// its writer called PutObjectMeta with its own IsLatest: GET served it while
// the listing called another current. The pointer now follows the record.
func TestThePointerFollowsTheVersionRecord(t *testing.T) {
	s := orderedStore(t)
	b := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588150000000bbbbbbbb", IsLatest: true}
	s.PutObjectVersion(b) // flagged latest, its pointer not written yet
	r := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588145000000cccccccc", IsLatest: true}
	s.PutObjectVersion(r)
	s.PutObjectMeta(r)
	if cur, err := s.GetObjectMeta("b", "k"); err == nil && cur != nil && cur.VersionID == r.VersionID {
		t.Fatalf("the pointer moved to %s, which is stored noncurrent below %s", r.VersionID, b.VersionID)
	}
	s.PutObjectMeta(b)
	cur, _ := s.GetObjectMeta("b", "k")
	if got := flaggedLatest(s); len(got) != 1 || got[0] != b.VersionID || cur.VersionID != b.VersionID {
		t.Fatalf("pointer %s, flagged latest %v", cur.VersionID, got)
	}
	// Updating the current version itself (tags, lock) still writes.
	b.Tags = map[string]string{"x": "y"}
	s.PutObjectMeta(b)
	if cur, _ := s.GetObjectMeta("b", "k"); cur.Tags["x"] != "y" {
		t.Fatal("an update of the current version was skipped")
	}
}
