package metadata

import (
	"fmt"
	"path/filepath"
	"testing"
)

func newVersionStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	return s
}

// Version ids are time-ordered and the index is sorted, so the newest version is
// the LAST entry under a key. Taking the first entry (what a listing gives you)
// returns the OLDEST, which is how deleting a delete marker used to resurrect a
// stale copy of an object.
func TestLatestObjectVersionReturnsTheNewest(t *testing.T) {
	s := newVersionStore(t)
	var last string
	for i := 0; i < 5; i++ {
		last = fmt.Sprintf("v%03d", i)
		if err := s.PutObjectVersion(ObjectMeta{Bucket: "b", Key: "k", VersionID: last, Size: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestObjectVersion("b", "k")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.VersionID != last {
		t.Fatalf("latest = %v, want %s", got, last)
	}
}

func TestLatestObjectVersionIsNilWithoutVersions(t *testing.T) {
	s := newVersionStore(t)
	got, err := s.LatestObjectVersion("b", "absent")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("got %+v for a key with no versions, want nil", got)
	}
}

// The seek must not walk off this key into a neighbouring one, in either
// direction. Keys that share a prefix are the dangerous case.
func TestLatestObjectVersionStaysWithinItsKey(t *testing.T) {
	s := newVersionStore(t)
	for _, k := range []string{"k", "k-suffix", "kk", "j"} {
		if err := s.PutObjectVersion(ObjectMeta{Bucket: "b", Key: k, VersionID: "v1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutObjectVersion(ObjectMeta{Bucket: "b", Key: "k", VersionID: "v2"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestObjectVersion("b", "k")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Key != "k" || got.VersionID != "v2" {
		t.Fatalf("got %+v, want key k version v2", got)
	}
	// The very last key in the bucket must also resolve to itself, which is the
	// case where the seek runs past the end of the index.
	got, err = s.LatestObjectVersion("b", "kk")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Key != "kk" {
		t.Fatalf("got %+v for the last key in the index, want key kk", got)
	}
}

// A key whose versions are absent must not return another bucket's data.
func TestLatestObjectVersionIsScopedToBucket(t *testing.T) {
	s := newVersionStore(t)
	if err := s.CreateBucket("other"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutObjectVersion(ObjectMeta{Bucket: "other", Key: "k", VersionID: "v1"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestObjectVersion("b", "k")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("got %+v from another bucket", got)
	}
}
