package metadata

import (
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// A policy that exists but cannot be read was skipped, so a Deny in it simply
// stopped applying. A policy that was deleted is still skipped.
func TestUnreadablePolicyFailsTheUsersPolicySet(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateIAMPolicy(IAMPolicy{Name: "ok", Document: `{}`})
	s.CreateIAMUser(IAMUser{Name: "u", PolicyARNs: []string{"ok", "gone"}})
	if _, err := s.GetUserPolicies("u"); err != nil {
		t.Fatalf("a deleted policy must be skipped, got %v", err)
	}
	s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(iamPoliciesBucket).Put([]byte("ok"), []byte("{not json"))
	})
	if _, err := s.GetUserPolicies("u"); err == nil {
		t.Error("an unreadable policy was skipped instead of failing the policy set")
	}
}
