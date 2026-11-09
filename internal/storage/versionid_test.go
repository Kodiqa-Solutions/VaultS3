package storage

import (
	"strings"
	"testing"
)

func TestVersionIDProblem(t *testing.T) {
	for _, ok := range []string{"null", "18e6588146411850aaaaaaaa", "0000000000000001", "v1.2_x-y"} {
		if p := VersionIDProblem(ok); p != "" {
			t.Errorf("%q refused: %s", ok, p)
		}
	}
	for _, bad := range []string{"", ".", "..", "secret/18e6", "../x", "a\\b", "a\x00b", "a b", strings.Repeat("a", 129)} {
		if VersionIDProblem(bad) == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The engine itself never resolves a bad version ID into another key's file.
func TestVersionPathRefusesABadID(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	_ = fs.CreateBucketDir("b")
	_, _, _ = fs.PutObjectVersion("b", "doc/secret", "v1", strings.NewReader("locked"), 6)
	_ = fs.DeleteObjectVersion("b", "doc", "secret/v1")
	r, _, err := fs.GetObjectVersion("b", "doc/secret", "v1")
	if err != nil {
		t.Fatalf("another key's version was deleted through a crafted id: %v", err)
	}
	_ = r.Close()
}
