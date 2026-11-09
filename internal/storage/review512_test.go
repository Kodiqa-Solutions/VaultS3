package storage

import (
	"bytes"
	"testing"
)

// Regression (5.0.12 review): cold "a", hot "a/b": deleting "a" must succeed and leave "a/b".
func TestColdTierDeletesAColdKeyUnderAHotFolder(t *testing.T) {
	ct, hot, cold := newColdTierRig(t)
	cold.PutObject("b", "a", bytes.NewReader([]byte("cold")), 4)
	hot.PutObject("b", "a/b", bytes.NewReader([]byte("hot")), 3)
	err := ct.DeleteObject("b", "a")
	if cold.ObjectExists("b", "a") {
		t.Errorf("cold a still there")
	}
	if !hot.ObjectExists("b", "a/b") {
		t.Errorf("hot a/b gone")
	}
	if err != nil {
		t.Fatalf("DeleteObject(a) = %v, want nil (cold copy was removed)", err)
	}
}
