package metadata

import (
	"path/filepath"
	"testing"
)

// Two quick writes of one key through a lagging follower each demoted "the
// previous latest" from a stale read, so the key ended with two latest
// versions. Storing a latest version demotes the others itself.
func TestStoringALatestVersionDemotesTheOthers(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, v := range []string{"v1", "v2", "v3"} {
		if err := s.PutObjectVersion(ObjectMeta{Bucket: "b", Key: "k", VersionID: v, IsLatest: true}); err != nil {
			t.Fatal(err)
		}
	}
	s.PutObjectVersion(ObjectMeta{Bucket: "b", Key: "k2", VersionID: "x", IsLatest: true})
	vers, _, err := s.ListObjectVersions("b", "k", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	latest := 0
	for _, v := range vers {
		if v.Key == "k" && v.IsLatest {
			latest++
			if v.VersionID != "v3" {
				t.Errorf("%s is marked latest, want v3", v.VersionID)
			}
		}
	}
	if latest != 1 {
		t.Errorf("%d latest versions of k, want 1", latest)
	}
	if m, _ := s.GetObjectVersion("b", "k2", "x"); m == nil || !m.IsLatest {
		t.Error("another key's latest version was demoted")
	}
}
