package metadata

import (
	"testing"
	"time"
)

type barrierRaft struct{ fakeRaft }

func (barrierRaft) ReadBarrier(time.Duration) error { return nil }

// Regression (5.0.16 review): replica repair reads every walked object through GetObjectMetaConsistent.
// On a follower whose barrier succeeded, a key deleted since the walk still
// costs the full ReadYourWritesTimeout (2s), although after the barrier the
// miss is already authoritative.
func TestAMissAfterTheReadBarrierIsFinal(t *testing.T) {
	store := newTestStore(t)
	_ = store.CreateBucket("b")
	ds := NewDistributedStore(store, &barrierRaft{fakeRaft{leader: false}})
	start := time.Now()
	if m, _ := ds.GetObjectMetaConsistent("b", "deleted-since-walk"); m != nil {
		t.Fatal("hit")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("a miss after a successful read barrier waited %s per object", d)
	}
}
