package tiering

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

type failHotTierStore struct {
	metadata.StoreAPI
	fail bool
}

func (f *failHotTierStore) SetObjectTier(bucket, key, tier string) error {
	if f.fail && tier == "hot" {
		return errors.New("injected")
	}
	return f.StoreAPI.SetObjectTier(bucket, key, tier)
}

// Regression (5.0.12 review): a move whose hot-file rename fails (not
// ENOENT) and whose revert cannot mark the record hot keeps the cold copy. A
// later scan must converge.
func TestAFailedRevertToHotIsRepairedByTheScan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory that makes the rename fail")
	}
	r := newTierRig(t, 30)
	r.putHot(t, "k", []byte("data"), time.Now().Add(-48*time.Hour).Unix())
	fs := &failHotTierStore{StoreAPI: r.store, fail: true}
	r.mgr.store = fs
	dir := filepath.Dir(r.hot.ObjectPath("b", "k"))
	r.mgr.beforeColdRemove = func() { os.Chmod(dir, 0o555) }
	err := r.mgr.migrateToCol("b", "k")
	os.Chmod(dir, 0o755)
	r.mgr.beforeColdRemove = nil
	t.Logf("migrateToCol: %v", err)
	fs.fail = false
	r.mgr.scan()
	r.mgr.scan()
	m, _ := r.store.GetObjectMeta("b", "k")
	t.Logf("tier=%q hot=%v cold=%v", m.Tier, r.hot.ObjectExists("b", "k"), r.cold.ObjectExists("b", "k"))
	if m.Tier == "cold" || !r.hot.ObjectExists("b", "k") || r.cold.ObjectExists("b", "k") {
		t.Fatal("after two scans the record should be hot, with the hot file kept and the stale cold copy dropped")
	}
}
