package metadata

import (
	"path/filepath"
	"testing"
)

// Regression (5.0.12 review): an upload takes its version ID before its body
// arrives, so a slow upload that began first committed last and became
// current with the lower ID, while a site that applied both made the higher
// one current. The store now keeps the highest ID current, and the loser as
// a noncurrent version, whatever order the writers commit in.
func TestTheHighestVersionIDStaysCurrent(t *testing.T) {
	SetVersionOrdering(true)
	t.Cleanup(func() { SetVersionOrdering(false) })
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateBucket("b")
	high := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588146411850aaaaaaaa", Size: 1, IsLatest: true}
	low := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588143385000bbbbbbbb", Size: 2, IsLatest: true}
	for _, m := range []ObjectMeta{high, low} { // the higher commits first
		// What a writer does: demote the current record, add its own, point at it.
		if cur, err := s.GetObjectMeta("b", "k"); err == nil && cur != nil {
			cur.IsLatest = false
			s.PutObjectVersion(*cur)
		}
		if err := s.PutObjectVersion(m); err != nil {
			t.Fatal(err)
		}
		if err := s.PutObjectMeta(m); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := s.GetObjectMeta("b", "k")
	if cur.VersionID != high.VersionID {
		t.Fatalf("current %s, want the higher ID %s", cur.VersionID, high.VersionID)
	}
	vs, _, _ := s.ListObjectVersions("b", "k", "", "", 10)
	latest := map[string]bool{}
	for _, v := range vs {
		latest[v.VersionID] = v.IsLatest
	}
	if len(vs) != 2 || !latest[high.VersionID] || latest[low.VersionID] {
		t.Fatalf("versions %+v: want both kept, only the higher latest", latest)
	}
	// The null version and pre-versioning objects are not compared by time.
	null := ObjectMeta{Bucket: "b", Key: "k", VersionID: "null", IsLatest: true}
	s.PutObjectVersion(null)
	s.PutObjectMeta(null)
	if cur, _ := s.GetObjectMeta("b", "k"); cur.VersionID != "null" {
		t.Fatalf("a null version write did not become current: %s", cur.VersionID)
	}
}

// Without active-active the last version committed is current, as before
// 5.0.12: on a Raft cluster a lagging node can give a later write a lower ID
// from a skewed clock, and that write must still win.
func TestWithoutActiveActiveTheLastCommitIsCurrent(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateBucket("b")
	high := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588146411850aaaaaaaa", IsLatest: true}
	low := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588143385000bbbbbbbb", IsLatest: true}
	for _, m := range []ObjectMeta{high, low} {
		s.PutObjectVersion(m)
		s.PutObjectMeta(m)
	}
	if cur, _ := s.GetObjectMeta("b", "k"); cur.VersionID != low.VersionID {
		t.Fatalf("current %s, want the last committed %s", cur.VersionID, low.VersionID)
	}
}
