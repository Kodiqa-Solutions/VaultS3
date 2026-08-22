package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// The decisive test for the console IDOR (security assessment finding 14).
//
// It uses a GENUINE session for a non-admin subject, minted by this server's own
// signer, not a forged one. A forged token is now rejected by signature, which
// would let this pass for the wrong reason and prove nothing about
// authorization. What is under test is: a real session, for a real user, with no
// IAM policy, must not reach another user's bucket.
func TestConsoleDeniesCrossBucketAccessToANonAdminSession(t *testing.T) {
	h, store := newTestAPI(t)
	admin := getToken(t, h)

	if rr := doRequest(h, "POST", "/buckets", map[string]string{"name": "victim-bucket"}, admin); rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("create bucket: %d %s", rr.Code, rr.Body.String())
	}
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: "victim-bucket", Key: "secret.txt", Size: 3}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIAMUser(metadata.IAMUser{Name: "mallory"}); err != nil {
		t.Fatal(err)
	}

	// A real session for mallory, exactly as an OIDC login would issue one.
	mallory, err := h.jwt.Generate("mallory", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// It is a valid session: the server accepts it as an identity.
	if rr := doRequest(h, "GET", "/auth/me", nil, mallory); rr.Code != http.StatusOK {
		t.Fatalf("the non-admin session is not valid at all (%d), so this test proves nothing", rr.Code)
	}

	// Every one of these returned 200/204 before the fix.
	denied := []struct {
		method, path string
		body         interface{}
	}{
		{"GET", "/buckets/victim-bucket/objects", nil},
		{"GET", "/buckets/victim-bucket/download/secret.txt", nil},
		{"POST", "/buckets/victim-bucket/bulk-delete", map[string][]string{"keys": {"secret.txt"}}},
		{"DELETE", "/buckets/victim-bucket/objects/secret.txt", nil},
		{"PUT", "/buckets/victim-bucket/versioning", map[string]string{"versioning": "Enabled"}},
		{"PUT", "/buckets/victim-bucket/policy", map[string]string{"policy": "{}"}},
		{"GET", "/buckets/victim-bucket", nil},
	}
	for _, c := range denied {
		rr := doRequest(h, c.method, c.path, c.body, mallory)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s %s returned %d for a user with no policies, want 403", c.method, c.path, rr.Code)
		}
	}

	// The object must still be there after all of that.
	if meta, err := store.GetObjectMeta("victim-bucket", "secret.txt"); err != nil || meta == nil {
		t.Fatal("the object was destroyed by a user with no permissions")
	}

	// And the bucket must not even be advertised to them.
	rr := doRequest(h, "GET", "/buckets", nil, mallory)
	if rr.Code == http.StatusOK && strings.Contains(rr.Body.String(), "victim-bucket") {
		t.Error("the bucket list advertises a bucket the caller cannot open")
	}

	// Admin is unaffected.
	if rr := doRequest(h, "GET", "/buckets/victim-bucket/objects", nil, admin); rr.Code != http.StatusOK {
		t.Fatalf("admin lost access to the bucket: %d %s", rr.Code, rr.Body.String())
	}
}

// A user whose policy grants access to their own bucket keeps it, so the fix
// denies the right thing rather than everything.
func TestConsoleAllowsAUserTheirOwnBucket(t *testing.T) {
	h, store := newTestAPI(t)
	admin := getToken(t, h)

	if rr := doRequest(h, "POST", "/buckets", map[string]string{"name": "grace-bucket"}, admin); rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("create bucket: %d", rr.Code)
	}
	if err := store.CreateIAMUser(metadata.IAMUser{Name: "grace", PolicyARNs: []string{"grace-policy"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{
		Name: "grace-policy",
		Document: `{"Statement":[{"Effect":"Allow","Action":["s3:ListBucket","s3:GetObject"],` +
			`"Resource":["arn:aws:s3:::grace-bucket","arn:aws:s3:::grace-bucket/*"]}]}`,
	}); err != nil {
		t.Fatal(err)
	}

	grace, err := h.jwt.Generate("grace", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rr := doRequest(h, "GET", "/buckets/grace-bucket/objects", nil, grace); rr.Code != http.StatusOK {
		t.Fatalf("a user was denied their own bucket: %d %s", rr.Code, rr.Body.String())
	}
	// But writing is not in the policy.
	if rr := doRequest(h, "DELETE", "/buckets/grace-bucket/objects/x.txt", nil, grace); rr.Code != http.StatusForbidden {
		t.Errorf("delete allowed with a read-only policy: %d", rr.Code)
	}
}
