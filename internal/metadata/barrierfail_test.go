package metadata

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type cutOffRaft struct{ followerRaft }

func (c *cutOffRaft) ReadBarrier(time.Duration) error { return errors.New("no leader") }

// A follower that cannot catch up with the leader (just restarted, or cut
// off) answers that it cannot tell, not with its own copy: the 5.0.14 soak
// read objects written while a node was down as missing, or as the object
// they replaced, from the restarted node.
func TestAConsistentReadThatCannotCatchUpFails(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "k", ETag: `"old"`}); err != nil {
		t.Fatal(err)
	}
	d := NewDistributedStore(s, &cutOffRaft{})
	meta, err := d.GetObjectMetaConsistent("b", "k")
	if meta != nil || !errors.Is(err, ErrShardUnavailable) {
		t.Errorf("got %v, %v; want no record and ErrShardUnavailable", meta, err)
	}
	// With the barrier passing, the record is read.
	d = NewDistributedStore(s, &followerRaft{})
	if meta, err := d.GetObjectMetaConsistent("b", "k"); meta == nil || err != nil {
		t.Errorf("after catching up: %v, %v", meta, err)
	}
}
