package tiering

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// hookedCold runs a hook after the cold copy is written, the moment a PUT to
// the same key can land while a migration is under way.
type hookedCold struct {
	storage.Engine
	after func()
}

func (h *hookedCold) PutObject(bucket, key string, r io.Reader, size int64) (int64, string, error) {
	n, etag, err := h.Engine.PutObject(bucket, key, r, size)
	if h.after != nil {
		h.after()
		h.after = nil
	}
	return n, etag, err
}

// A PUT that lands while an object is being moved to cold must survive. The
// old migration deleted the hot copy (the NEW bytes) and marked the new record
// cold, so reads returned the old object.
func TestMigrationDoesNotDestroyConcurrentPut(t *testing.T) {
	r := newTierRig(t, 30)
	r.putHot(t, "obj", []byte("old bytes"), time.Now().Add(-48*time.Hour).Unix())

	newer := []byte("new bytes written during migration")
	cold := &hookedCold{Engine: r.cold}
	cold.after = func() {
		if _, _, err := r.hot.PutObject("b", "obj", bytes.NewReader(newer), int64(len(newer))); err != nil {
			t.Fatal(err)
		}
		r.store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "obj", ETag: "new", Size: int64(len(newer)), LastModified: time.Now().Unix()})
	}
	r.mgr.coldEngine = cold

	r.mgr.ManualMigrate("b", "obj", "cold")

	if tier := tierOf(t, r.store, "obj"); tier == "cold" {
		t.Fatal("the new object was marked cold")
	}
	if got := r.read(t, r.hot, "obj"); !bytes.Equal(got, newer) {
		t.Fatalf("hot copy is %q, want the bytes the concurrent PUT wrote", got)
	}
	rc, _, err := r.mgr.GetObject("b", "obj")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, newer) {
		t.Fatalf("reads return %q, want the new object", got)
	}
}
