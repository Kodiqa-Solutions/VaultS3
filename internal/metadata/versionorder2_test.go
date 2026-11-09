package metadata

import (
	"path/filepath"
	"testing"
)

// Regression (5.0.12 review): interleaving: B (higher ID) has written its version record but not yet its
// latest pointer when A (lower ID) writes both. The pointer ends at B, but the
// version index flags A latest and B noncurrent.
func TestInterleavedWritersLeaveTheFlagsOnThePointer(t *testing.T) {
	SetVersionOrdering(true)
	t.Cleanup(func() { SetVersionOrdering(false) })
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateBucket("b")
	c := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588140000000cccccccc", Size: 1, IsLatest: true}
	s.PutObjectVersion(c)
	s.PutObjectMeta(c)
	a := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588143385000aaaaaaaa", Size: 2, IsLatest: true}
	bb := ObjectMeta{Bucket: "b", Key: "k", VersionID: "18e6588146411850bbbbbbbb", Size: 3, IsLatest: true}
	demote := func() {
		if cur, err := s.GetObjectMeta("b", "k"); err == nil && cur != nil {
			cur.IsLatest = false
			s.PutObjectVersion(*cur)
		}
	}
	demote()               // B demotes C
	s.PutObjectVersion(bb) // B version record
	demote()               // A demotes C (pointer still C)
	s.PutObjectVersion(a)  // A version record: pointer is C, so A latest, B demoted
	s.PutObjectMeta(a)     // pointer A
	s.PutObjectMeta(bb)    // pointer B
	cur, _ := s.GetObjectMeta("b", "k")
	vs, _, _ := s.ListObjectVersions("b", "k", "", "", 10)
	latest := map[string]bool{}
	for _, v := range vs {
		latest[v.VersionID] = v.IsLatest
	}
	t.Logf("pointer %s flags %v", cur.VersionID, latest)
	// A legal hold on A, as PutObjectLegalHold does: the record says latest.
	av, _ := s.GetObjectVersion("b", "k", a.VersionID)
	av.LegalHold = true
	s.UpdateObjectVersionMeta(*av)
	after, _ := s.GetObjectMeta("b", "k")
	t.Logf("pointer after a legal hold on the older version: %s", after.VersionID)
	if !latest[cur.VersionID] || latest[a.VersionID] || after.VersionID != bb.VersionID {
		t.Fatalf("pointer %s but version flags %v", cur.VersionID, latest)
	}
}
