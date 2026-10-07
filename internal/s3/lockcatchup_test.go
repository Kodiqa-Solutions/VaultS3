package s3

import (
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

type barrierStore struct {
	*metadata.Store
	barriers int
}

func (b *barrierStore) ReadBarrier() error { b.barriers++; return nil }

// On a follower a lock decision read a lagging local copy, so a COMPLIANCE
// retention set a moment earlier could be downgraded. The lock check catches up
// first on a bucket that uses object lock, and only there.
func TestLockDecisionsCatchUpWithTheCluster(t *testing.T) {
	s, err := metadata.NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bs := &barrierStore{Store: s}
	s.CreateBucket("locked")
	s.SetBucketObjectLockEnabled("locked", true)
	s.CreateBucket("plain")
	h := &ObjectHandler{store: bs}

	h.checkObjectLock("plain", "k", "")
	if bs.barriers != 0 {
		t.Errorf("a bucket without object lock paid for a barrier")
	}
	h.checkObjectLock("locked", "k", "")
	if bs.barriers != 1 {
		t.Errorf("%d barriers before a lock decision on a lock-enabled bucket, want 1", bs.barriers)
	}
}
