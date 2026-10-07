package metadata

import (
	"path/filepath"
	"testing"
)

// A key-marker without a version-id-marker continues AFTER every version of
// that key, as AWS defines it, so a client paging by key marker does not see
// the marker key's versions again.
func TestListObjectVersionsKeyMarkerSkipsAllVersionsOfKey(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, k := range []string{"a", "b", "c"} {
		for _, v := range []string{"v1", "v2"} {
			if err := s.PutObjectVersion(ObjectMeta{Bucket: "bk", Key: k, VersionID: v}); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, _, err := s.ListObjectVersions("bk", "", "b", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "c" || got[1].Key != "c" {
		var keys []string
		for _, m := range got {
			keys = append(keys, m.Key+"/"+m.VersionID)
		}
		t.Fatalf("after key-marker b got %v, want only c's two versions", keys)
	}
	// With a version-id-marker it continues within the key, after that version.
	got, _, _ = s.ListObjectVersions("bk", "", "b", "v1", 0)
	if len(got) != 3 || got[0].Key != "b" || got[0].VersionID != "v2" {
		t.Fatalf("after b/v1 got %d entries starting %v", len(got), got)
	}
}
