package metadata

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// The object lock flag was written to the local store only, so on a cluster
// just the node that served the request knew the bucket was locked. Enabling it
// now goes through the replicated default retention command, which sets it.
func TestObjectLockFlagIsReplicated(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	r := &recordingRaft{}
	d := NewDistributedStore(s, r)
	if err := d.SetBucketObjectLockEnabled("b", true); err != nil {
		t.Fatal(err)
	}
	if len(r.types) != 1 || r.types[0] != cmdSetBucketDefaultRet {
		t.Fatalf("enabling object lock applied commands %v, want one default retention command", r.types)
	}
	if d.SetBucketObjectLockEnabled("b", false) == nil {
		t.Error("disabling object lock was accepted")
	}

	// Applying that command, as every node does, sets the flag.
	if err := s.SetBucketDefaultRetention("b", "", 0); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.GetBucket("b"); info == nil || !info.ObjectLockEnabled {
		t.Error("applying the default retention command did not mark the bucket object-lock enabled")
	}
}

type recordingRaft struct{ types []uint16 }

func (f *recordingRaft) Apply(b []byte) error {
	var c raftCommand
	if err := json.Unmarshal(b, &c); err != nil {
		return err
	}
	f.types = append(f.types, c.Type)
	return nil
}
func (f *recordingRaft) IsLeader() bool               { return true }
func (f *recordingRaft) ForwardToLeader([]byte) error { return nil }
