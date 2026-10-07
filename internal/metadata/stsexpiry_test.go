package metadata

import (
	"path/filepath"
	"testing"
)

// An expired scoped session left its synthetic sts-<key> user and policy
// behind forever. They go with the key, and nothing else does.
func TestExpiredSessionTakesItsSyntheticUserAndPolicy(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateIAMUser(IAMUser{Name: "alice"})
	s.CreateIAMUser(IAMUser{Name: "sts-AKSESSION"})
	s.CreateIAMPolicy(IAMPolicy{Name: "sts-AKSESSION", Document: "{}"})
	s.CreateAccessKey(AccessKey{AccessKey: "AKSESSION", SecretKey: "x", UserID: "sts-AKSESSION", SourceUserID: "alice", SessionToken: "t", ExpiresAt: 1})
	s.CreateAccessKey(AccessKey{AccessKey: "AKLIVE", SecretKey: "x", UserID: "alice"})

	if n, err := s.DeleteExpiredAccessKeysAt(100); err != nil || n != 1 {
		t.Fatalf("deleted %d (%v), want 1", n, err)
	}
	if _, err := s.GetIAMUser("sts-AKSESSION"); err == nil {
		t.Error("the synthetic session user was left behind")
	}
	if _, err := s.GetIAMPolicy("sts-AKSESSION"); err == nil {
		t.Error("the synthetic session policy was left behind")
	}
	if _, err := s.GetIAMUser("alice"); err != nil {
		t.Error("the session's source user was deleted")
	}
	if _, err := s.GetAccessKey("AKLIVE"); err != nil {
		t.Error("a key that never expires was deleted")
	}
}
