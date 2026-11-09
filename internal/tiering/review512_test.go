package tiering

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// Regression (5.0.12 review): A DELETE of "a" (files removed, record not yet) and a PUT of "a/x" land
// Regression (5.0.12 review): between the move's checks and its rename: the hot path "a" is now the folder
// Regression (5.0.12 review): holding a/x. The move renames that folder aside and cannot link it back.
func TestMoveLeavesAnotherKeysFolderInPlace(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "a", []byte("a's bytes"), time.Now().Add(-48*time.Hour).Unix())
	r.mgr.beforeColdRemove = func() {
		os.Remove(r.hot.ObjectPath("b", "a"))
		r.hot.PutObject("b", "a/x", bytes.NewReader([]byte("x data")), 6)
	}
	err := r.mgr.migrateToCol("b", "a")
	t.Logf("migrateToCol: %v", err)
	if !r.hot.ObjectExists("b", "a/x") {
		entries, _ := os.ReadDir(r.hot.ObjectPath("b", ""))
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("object a/x is gone from its path; bucket dir now holds %v", names)
	}
}
