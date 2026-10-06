package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

func issueKey(t *testing.T, h http.Handler, token, user string, buckets ...string) keyCreateResponse {
	t.Helper()
	body := map[string]interface{}{"userId": user}
	if buckets != nil {
		body["buckets"] = buckets
	}
	rr := doRequest(h, "POST", "/keys", body, token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue key for %s: %d %s", user, rr.Code, rr.Body.String())
	}
	var k keyCreateResponse
	if err := json.NewDecoder(rr.Body).Decode(&k); err != nil {
		t.Fatal(err)
	}
	return k
}

func keyPolicyDoc(t *testing.T, store *metadata.Store, accessKey string) string {
	t.Helper()
	k, err := store.GetAccessKey(accessKey)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if k.PolicyName == "" {
		t.Fatalf("key %s has no policy of its own", accessKey)
	}
	p, err := store.GetIAMPolicy(k.PolicyName)
	if err != nil {
		t.Fatalf("key %s names policy %q, which does not exist", accessKey, k.PolicyName)
	}
	return p.Document
}

func makeBuckets(t *testing.T, store *metadata.Store, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := store.CreateBucket(n); err != nil {
			t.Fatalf("create bucket %s: %v", n, err)
		}
	}
}

// Every key for a user shared one policy attached to the user, rewritten on
// each issue. A second key scoped to another bucket took the first key's
// bucket away, and a second key with full access widened the first to every
// bucket.
func TestASecondKeyDoesNotChangeTheFirst(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	makeBuckets(t, store, "bkt-one", "bkt-two")

	first := issueKey(t, h, token, "alice", "bkt-one")
	before := keyPolicyDoc(t, store, first.AccessKey)
	issueKey(t, h, token, "alice", "bkt-two")
	issueKey(t, h, token, "alice")

	if after := keyPolicyDoc(t, store, first.AccessKey); after != before {
		t.Errorf("first key's grant changed when later keys were issued:\nbefore %s\nafter  %s", before, after)
	}
	if !strings.Contains(before, "bkt-one") || strings.Contains(before, "bkt-two") {
		t.Errorf("first key's grant is %s, want bkt-one only", before)
	}
	u, err := store.GetIAMUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.PolicyARNs) != 0 {
		t.Errorf("alice carries %v, a key's grant must not be attached to the user, where every key would share it", u.PolicyARNs)
	}
}

// A user created on purpose kept only its own policies until a key was issued
// for it, then also got the key's grant, so every other key of that user got
// it too.
func TestIssuingAKeyLeavesTheUsersOwnPoliciesAlone(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	makeBuckets(t, store, "bkt-one")

	mustStatus(t, doRequest(h, "POST", "/iam/users", map[string]string{"name": "dave"}, token), http.StatusCreated)
	mustStatus(t, doRequest(h, "POST", "/iam/policies", map[string]string{
		"name": "dave-own", "document": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::bkt-one/*"]}]}`,
	}, token), http.StatusCreated)
	mustStatus(t, doRequest(h, "POST", "/iam/users/dave/policies", map[string]string{"policyName": "dave-own"}, token), http.StatusNoContent)

	issueKey(t, h, token, "dave")

	u, _ := store.GetIAMUser("dave")
	if len(u.PolicyARNs) != 1 || u.PolicyARNs[0] != "dave-own" {
		t.Errorf("dave's policies are %v after a key was issued, want [dave-own]", u.PolicyARNs)
	}
}

// Deleting a user's last key deleted the user, including one created on
// purpose with its own policies attached.
func TestDeletingTheLastKeyKeepsAUserCreatedOnPurpose(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)

	mustStatus(t, doRequest(h, "POST", "/iam/users", map[string]string{"name": "dave"}, token), http.StatusCreated)
	k := issueKey(t, h, token, "dave")
	policy := keyPolicyName(k.AccessKey)

	mustStatus(t, doRequest(h, "DELETE", "/keys/"+k.AccessKey, nil, token), http.StatusNoContent)

	if _, err := store.GetIAMUser("dave"); err != nil {
		t.Errorf("dave was deleted with his last key: %v", err)
	}
	if _, err := store.GetIAMPolicy(policy); err == nil {
		t.Errorf("the deleted key's policy %s was left behind", policy)
	}
}

// The convenience the old behaviour existed for still holds: a user that exists
// only because a key was issued goes with its last key, and not before.
func TestDeletingTheLastKeyRemovesAUserThatExistedOnlyForIt(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)

	k1 := issueKey(t, h, token, "erin")
	k2 := issueKey(t, h, token, "erin")

	mustStatus(t, doRequest(h, "DELETE", "/keys/"+k1.AccessKey, nil, token), http.StatusNoContent)
	if _, err := store.GetIAMUser("erin"); err != nil {
		t.Fatalf("erin was removed while she still had a key: %v", err)
	}
	mustStatus(t, doRequest(h, "DELETE", "/keys/"+k2.AccessKey, nil, token), http.StatusNoContent)
	if _, err := store.GetIAMUser("erin"); err == nil {
		t.Errorf("erin existed only for her keys and was kept after the last one was deleted")
	}
}

// legacyKeys writes the state an earlier release left: one policy per user,
// attached to the user, and keys with no policy of their own.
func legacyKeys(t *testing.T, store *metadata.Store, user, doc string, extraPolicies []string, keys ...string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{Name: legacyKeyPolicyName(user), CreatedAt: now, Document: doc}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIAMUser(metadata.IAMUser{
		Name: user, CreatedAt: now,
		PolicyARNs: append(append([]string{}, extraPolicies...), legacyKeyPolicyName(user)),
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if err := store.CreateAccessKey(metadata.AccessKey{AccessKey: k, SecretKey: "s", CreatedAt: now, UserID: user}); err != nil {
			t.Fatal(err)
		}
	}
}

// Keys issued before this release share the user's policy. Issuing a new key
// for such a user gives each old key its own copy first, so none of them gains
// or loses access, and the new key does not inherit theirs.
func TestIssuingAKeySplitsTheSharedPolicyOfOlderKeys(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	makeBuckets(t, store, "bkt-one", "bkt-two")
	oldDoc := allowS3On([]string{"arn:aws:s3:::bkt-one", "arn:aws:s3:::bkt-one/*"})
	legacyKeys(t, store, "alice", oldDoc, nil, "OLDKEYONE", "OLDKEYTWO")

	issueKey(t, h, token, "alice", "bkt-two")

	for _, k := range []string{"OLDKEYONE", "OLDKEYTWO"} {
		if got := keyPolicyDoc(t, store, k); got != oldDoc {
			t.Errorf("%s now has %s, want its old grant %s", k, got, oldDoc)
		}
	}
	u, _ := store.GetIAMUser("alice")
	if len(u.PolicyARNs) != 0 {
		t.Errorf("alice still carries %v, so the new key inherits the old keys' grant", u.PolicyARNs)
	}
	if !u.KeyManaged {
		t.Errorf("alice existed only for her keys and lost that mark in the conversion, so she would outlive them")
	}
	if _, err := store.GetIAMPolicy(legacyKeyPolicyName("alice")); err == nil {
		t.Errorf("the shared policy was left behind")
	}
}

// A user from an earlier release that someone created on purpose has its own
// policies besides the shared key one. Deleting its last key must keep the user
// and its own policies, and drop only the shared key grant.
func TestDeletingTheLastOlderKeyKeepsAUserWithItsOwnPolicies(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{Name: "frank-own", Document: allowS3On([]string{"*"})}); err != nil {
		t.Fatal(err)
	}
	legacyKeys(t, store, "frank", allowS3On([]string{"*"}), []string{"frank-own"}, "OLDFRANK")

	mustStatus(t, doRequest(h, "DELETE", "/keys/OLDFRANK", nil, token), http.StatusNoContent)

	u, err := store.GetIAMUser("frank")
	if err != nil {
		t.Fatalf("frank was deleted with his last key: %v", err)
	}
	if len(u.PolicyARNs) != 1 || u.PolicyARNs[0] != "frank-own" {
		t.Errorf("frank has %v, want only his own policy", u.PolicyARNs)
	}
}

// A user from an earlier release that existed only for its keys is still
// removed with the last of them.
func TestDeletingTheLastOlderKeyRemovesAUserThatExistedOnlyForIt(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	legacyKeys(t, store, "gina", allowS3On([]string{"*"}), nil, "OLDGINA")

	mustStatus(t, doRequest(h, "DELETE", "/keys/OLDGINA", nil, token), http.StatusNoContent)

	if _, err := store.GetIAMUser("gina"); err == nil {
		t.Errorf("gina existed only for her key and was kept")
	}
	if _, err := store.GetIAMPolicy(legacyKeyPolicyName("gina")); err == nil {
		t.Errorf("gina's shared key policy was left behind")
	}
}

// Deleting a key's policy on its own left the key listed and signing while it
// could reach nothing.
func TestAKeysPolicyCannotBeDeletedApartFromTheKey(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	k := issueKey(t, h, token, "alice")

	rr := doRequest(h, "DELETE", "/iam/policies/"+keyPolicyName(k.AccessKey), nil, token)
	if rr.Code != http.StatusConflict {
		t.Errorf("deleting a key's policy answered %d, want 409", rr.Code)
	}
	if _, err := store.GetIAMPolicy(keyPolicyName(k.AccessKey)); err != nil {
		t.Errorf("the key's policy was deleted anyway")
	}
}

func mustStatus(t *testing.T, rr interface {
	Result() *http.Response
}, want int) {
	t.Helper()
	if got := rr.Result().StatusCode; got != want {
		t.Fatalf("status %d, want %d", got, want)
	}
}

func issueRaw(h http.Handler, token string, body map[string]interface{}) (int, keyCreateResponse, string) {
	rr := doRequest(h, "POST", "/keys", body, token)
	var k keyCreateResponse
	raw := rr.Body.String()
	_ = json.Unmarshal([]byte(raw), &k)
	return rr.Code, k, raw
}

func readOnlyUser(t *testing.T, h http.Handler, token, name string) {
	t.Helper()
	mustStatus(t, doRequest(h, "POST", "/iam/users", map[string]string{"name": name}, token), http.StatusCreated)
	mustStatus(t, doRequest(h, "POST", "/iam/policies", map[string]string{
		"name": name + "-ro", "document": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["*"]}]}`,
	}, token), http.StatusCreated)
	mustStatus(t, doRequest(h, "POST", "/iam/users/"+name+"/policies", map[string]string{"policyName": name + "-ro"}, token), http.StatusNoContent)
}

// docs/ACCESS-CONTROL.md: create a user, attach a read-only policy, issue a key
// naming no buckets. That key got s3:* on every bucket, so the read-only user
// could write anywhere. It must get exactly the user's policies.
func TestAKeyForAUserWithPoliciesGetsOnlyThose(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	readOnlyUser(t, h, token, "alice")

	code, k, raw := issueRaw(h, token, map[string]interface{}{"userId": "alice"})
	if code != http.StatusCreated {
		t.Fatalf("issue: %d %s", code, raw)
	}
	if k.Access != keyAccessUser {
		t.Errorf("access %q, want %q", k.Access, keyAccessUser)
	}
	stored, _ := store.GetAccessKey(k.AccessKey)
	if stored.PolicyName != "" {
		p, _ := store.GetIAMPolicy(stored.PolicyName)
		t.Errorf("the read-only user's key was given its own grant %v", p)
	}
}

// Every bucket stays the default for a user with no policies, which is what the
// dashboard promises ("leave empty for full S3 access") and without which the
// key would reach nothing.
func TestAKeyForAUserWithoutPoliciesStillDefaultsToEveryBucket(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	code, k, raw := issueRaw(h, token, map[string]interface{}{"userId": "newcomer"})
	if code != http.StatusCreated || k.Access != keyAccessAll {
		t.Fatalf("issue: %d access %q %s", code, k.Access, raw)
	}
	if doc := keyPolicyDoc(t, store, k.AccessKey); !strings.Contains(doc, `"*"`) {
		t.Errorf("grant is %s, want every bucket", doc)
	}
}

func TestAllBucketsIsHonouredForAUserWithPolicies(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	readOnlyUser(t, h, token, "alice")
	code, k, raw := issueRaw(h, token, map[string]interface{}{"userId": "alice", "allBuckets": true})
	if code != http.StatusCreated || k.Access != keyAccessAll {
		t.Fatalf("issue: %d access %q %s", code, k.Access, raw)
	}
	if doc := keyPolicyDoc(t, store, k.AccessKey); !strings.Contains(doc, `"*"`) {
		t.Errorf("grant is %s, want every bucket", doc)
	}
}

// Asking for the user's policies only, for a user that has none, would issue a
// key that reaches nothing. Refuse, and leave nothing behind, not even the user.
func TestUserPoliciesOnlyRefusesAUserWithoutPolicies(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	code, _, raw := issueRaw(h, token, map[string]interface{}{"userId": "ghost", "userPoliciesOnly": true})
	if code != http.StatusBadRequest || !strings.Contains(raw, "no policies") {
		t.Fatalf("want 400 naming the reason, got %d %s", code, raw)
	}
	if _, err := store.GetIAMUser("ghost"); err == nil {
		t.Error("a refused request still created the user")
	}
	if keys, _ := store.ListAccessKeys(); len(keys) != 0 {
		t.Errorf("a refused request still issued %d key(s)", len(keys))
	}
}

func TestKeyAccessAlternativesAreExclusive(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	makeBuckets(t, store, "bkt-one")
	for _, body := range []map[string]interface{}{
		{"userId": "a", "buckets": []string{"bkt-one"}, "allBuckets": true},
		{"userId": "a", "allBuckets": true, "userPoliciesOnly": true},
		{"userId": "a", "buckets": []string{"bkt-one"}, "userPoliciesOnly": true},
	} {
		if code, _, raw := issueRaw(h, token, body); code != http.StatusBadRequest || !strings.Contains(raw, "alternatives") {
			t.Errorf("%v: %d %s", body, code, raw)
		}
	}
}

// Deleting a user left its keys behind. They were refused only while no user
// had the name: a new user created with it brought them back, each with the
// grant it was issued with.
func TestDeletingAUserRevokesItsKeysForGood(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	makeBuckets(t, store, "bkt-one")
	k := issueKey(t, h, token, "alice", "bkt-one")
	bystander := issueKey(t, h, token, "bob", "bkt-one")

	mustStatus(t, doRequest(h, "DELETE", "/iam/users/alice", nil, token), http.StatusNoContent)
	mustStatus(t, doRequest(h, "POST", "/iam/users", map[string]string{"name": "alice"}, token), http.StatusCreated)

	if _, err := store.GetAccessKey(k.AccessKey); err == nil {
		t.Error("alice's key survived her deletion and works again for the new alice")
	}
	if _, err := store.GetIAMPolicy(keyPolicyName(k.AccessKey)); err == nil {
		t.Error("alice's key grant was left behind")
	}
	if _, err := store.GetAccessKey(bystander.AccessKey); err != nil {
		t.Errorf("deleting alice removed bob's key: %v", err)
	}
}

// A user from an earlier release can still list its shared key policy after the
// policy itself was deleted by hand. That refused every new key for the user.
func TestIssuingAKeyCopesWithAStaleSharedPolicyEntry(t *testing.T) {
	h, store := newTestAPI(t)
	token := getToken(t, h)
	legacyKeys(t, store, "hana", allowS3On([]string{"*"}), nil, "OLDHANA")
	if err := store.DeleteIAMPolicy(legacyKeyPolicyName("hana")); err != nil {
		t.Fatal(err)
	}

	code, k, raw := issueRaw(h, token, map[string]interface{}{"userId": "hana", "allBuckets": true})
	if code != http.StatusCreated {
		t.Fatalf("issue for a user with a stale entry: %d %s", code, raw)
	}
	u, _ := store.GetIAMUser("hana")
	if len(u.PolicyARNs) != 0 {
		t.Errorf("stale entry kept: %v", u.PolicyARNs)
	}
	old, _ := store.GetAccessKey("OLDHANA")
	if old.PolicyName != "" {
		t.Errorf("the old key, which had no grant, was given one: %s", old.PolicyName)
	}
	_ = k
}
