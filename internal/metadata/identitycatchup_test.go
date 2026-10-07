package metadata

import (
	"path/filepath"
	"testing"
	"time"
)

type followerRaft struct {
	forwarded, barriers int
}

func (f *followerRaft) Apply([]byte) error              { return nil }
func (f *followerRaft) IsLeader() bool                  { return false }
func (f *followerRaft) ForwardToLeader([]byte) error    { f.forwarded++; return nil }
func (f *followerRaft) ReadBarrier(time.Duration) error { f.barriers++; return nil }

// A user created through a follower was "not found" by the next request to the
// same follower, because the forwarded write returned before this node applied
// it. Configuration and identity writes wait for the local apply, object writes still do not.
func TestFollowerCatchesUpAfterAnIdentityWrite(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := &followerRaft{}
	d := NewDistributedStore(s, r)

	d.CreateIAMUser(IAMUser{Name: "u"})
	d.CreateIAMPolicy(IAMPolicy{Name: "p"})
	d.CreateAccessKey(AccessKey{AccessKey: "k"})
	if r.barriers != 3 {
		t.Errorf("%d read barriers after 3 identity writes, want 3", r.barriers)
	}
	d.SetBucketVersioning("b", "Enabled")
	if r.barriers != 4 {
		t.Errorf("enabling versioning did not wait for the local apply, so enabling object lock next on this node failed")
	}
	d.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "k"})
	if r.barriers != 4 {
		t.Errorf("an object write waited for the local apply, which issue #37 removed for throughput")
	}
}
