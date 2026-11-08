package storage

import "testing"

// Regression (5.0.11 round 11): "." and empty segments were accepted, and the
// path folds them away, so "./doc" and "a//doc" named the files of "doc" and
// "a/doc". A key carried in a body (DeleteObjects, a form, the dashboard)
// reached a locked or denied object as another key.
func TestKeyProblem(t *testing.T) {
	refused := []string{"", "/a", "a\x00b", "../x", "a/../b", "./doc", "a/./b", "a/.", ".", "a//b", "//a",
		".vs", ".vs/k/null", ".ec/x", ".multipart/u", "a/.vaults3-tmp-1", ".vaults3-tmp-x"}
	for _, k := range refused {
		if KeyProblem(k) == "" {
			t.Errorf("%q accepted", k)
		}
	}
	allowed := []string{"a", "a/b", "a/", "dir/sub/", ".hidden", "a/.vs/b", "a/.ec", "...", "a..b", "a.b/c.d", "é/日"}
	for _, k := range allowed {
		if msg := KeyProblem(k); msg != "" {
			t.Errorf("%q refused: %s", k, msg)
		}
	}
}
