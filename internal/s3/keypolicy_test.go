package s3

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

const bucketOnePolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::bkt-one","arn:aws:s3:::bkt-one/*"]}]}`

func keyPolicyStore(t *testing.T) (*metadata.Store, *Authenticator) {
	t.Helper()
	store, err := metadata.NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Now().UTC()
	if err := store.CreateIAMUser(metadata.IAMUser{Name: "alice", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{Name: "access-key-AKONE", CreatedAt: now, Document: bucketOnePolicy}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []metadata.AccessKey{
		{AccessKey: "AKONE", SecretKey: "s", CreatedAt: now, UserID: "alice", PolicyName: "access-key-AKONE"},
		{AccessKey: "AKBARE", SecretKey: "s", CreatedAt: now, UserID: "alice"},
	} {
		if err := store.CreateAccessKey(k); err != nil {
			t.Fatal(err)
		}
	}
	return store, NewAuthenticator("admin", "secret", store, nil, nil)
}

// A key's grant is its own policy, not one attached to its user, so it must be
// read from the key. Another key of the same user gets none of it.
func TestAKeyIsAuthorizedByItsOwnPolicy(t *testing.T) {
	_, a := keyPolicyStore(t)
	r := httptest.NewRequest("GET", "/", nil)

	one, _, err := a.resolveIdentity("AKONE", r)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(one, "s3:PutObject", "arn:aws:s3:::bkt-one/x"); err != nil {
		t.Errorf("key denied the bucket its own policy grants: %v", err)
	}
	if err := a.Authorize(one, "s3:PutObject", "arn:aws:s3:::bkt-two/x"); err == nil {
		t.Errorf("key allowed a bucket its policy does not name")
	}

	bare, _, err := a.resolveIdentity("AKBARE", r)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(bare, "s3:PutObject", "arn:aws:s3:::bkt-one/x"); err == nil {
		t.Errorf("a second key of the same user was granted the first key's access")
	}
}

// A key left behind when its user is deleted used to lose all access, because
// its grant was the user's. Holding the grant on the key must not change that.
func TestAKeyOfADeletedUserIsDenied(t *testing.T) {
	store, a := keyPolicyStore(t)
	if err := store.DeleteIAMUser("alice"); err != nil {
		t.Fatal(err)
	}
	id, _, err := a.resolveIdentity("AKONE", httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(id, "s3:PutObject", "arn:aws:s3:::bkt-one/x"); err == nil {
		t.Errorf("a key whose user was deleted still reached its bucket")
	}
}

// An unreadable key policy must deny the key, not be skipped, the same as an
// unreadable user policy.
func TestAnUnreadableKeyPolicyDeniesTheKey(t *testing.T) {
	store, a := keyPolicyStore(t)
	if err := store.UpdateIAMPolicy(metadata.IAMPolicy{Name: "access-key-AKONE", Document: "{not json"}); err != nil {
		t.Fatal(err)
	}
	id, _, err := a.resolveIdentity("AKONE", httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !id.PolicyLoadFailed {
		t.Errorf("an unreadable key policy was skipped instead of failing the identity")
	}
}
