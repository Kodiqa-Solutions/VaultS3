package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// faultyStore fails chosen metadata operations, to prove a handler checks the
// error instead of carrying on as if the call had worked.
type faultyStore struct {
	metadata.StoreAPI
	failListAccessKeys bool
	failPutObjectMeta  bool
	failPutObjectVer   bool
	failSetAdminCreds  bool
}

var errInjected = errors.New("injected store failure")

func (f *faultyStore) ListAccessKeys() ([]metadata.AccessKey, error) {
	if f.failListAccessKeys {
		return nil, errInjected
	}
	return f.StoreAPI.ListAccessKeys()
}

func (f *faultyStore) PutObjectMeta(m metadata.ObjectMeta) error {
	if f.failPutObjectMeta {
		return errInjected
	}
	return f.StoreAPI.PutObjectMeta(m)
}

func (f *faultyStore) PutObjectVersion(m metadata.ObjectMeta) error {
	if f.failPutObjectVer {
		return errInjected
	}
	return f.StoreAPI.PutObjectVersion(m)
}

func (f *faultyStore) SetAdminCredentials(ak, sk string) error {
	if f.failSetAdminCreds {
		return errInjected
	}
	return f.StoreAPI.SetAdminCredentials(ak, sk)
}

func TestPolicyDocumentIsValidatedAsAPolicy(t *testing.T) {
	h, store := newTestAPI(t)
	tok := getToken(t, h)
	bad := map[string]string{
		"not an object":      `["s3:GetObject"]`,
		"no statement":       `{"Version":"2012-10-17"}`,
		"empty statement":    `{"Statement":[]}`,
		"wrong effect":       `{"Statement":[{"Effect":"allow","Action":"s3:*","Resource":"*"}]}`,
		"action is a number": `{"Statement":[{"Effect":"Allow","Action":5,"Resource":"*"}]}`,
	}
	for name, doc := range bad {
		rr := doRequest(h, "POST", "/iam/policies", map[string]string{"name": "p", "document": doc}, tok)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rr.Code)
		}
	}
	if pols, _ := store.ListIAMPolicies(); len(pols) != 0 {
		t.Fatalf("a refused document was stored: %+v", pols)
	}
	ok := `{"Statement":[{"Effect":"Deny","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*"}]}`
	if rr := doRequest(h, "POST", "/iam/policies", map[string]string{"name": "p", "document": ok}, tok); rr.Code != http.StatusCreated {
		t.Fatalf("a valid policy was refused: %d %s", rr.Code, rr.Body.String())
	}
}

func TestIAMNamesFollowTheAWSRule(t *testing.T) {
	h, store := newTestAPI(t)
	tok := getToken(t, h)
	long := strings.Repeat("a", 65)
	for _, name := range []string{"a/b", "a b", "ä", long, "x?y"} {
		if rr := doRequest(h, "POST", "/iam/users", map[string]string{"name": name}, tok); rr.Code != http.StatusBadRequest {
			t.Errorf("user %q: %d, want 400", name, rr.Code)
		}
		if rr := doRequest(h, "POST", "/iam/groups", map[string]string{"name": name}, tok); rr.Code != http.StatusBadRequest {
			t.Errorf("group %q: %d, want 400", name, rr.Code)
		}
	}
	doc := `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`
	if rr := doRequest(h, "POST", "/iam/policies", map[string]string{"name": strings.Repeat("p", 129), "document": doc}, tok); rr.Code != http.StatusBadRequest {
		t.Errorf("129-character policy name: %d, want 400", rr.Code)
	}
	if rr := doRequest(h, "POST", "/iam/policies", map[string]string{"name": strings.Repeat("p", 128), "document": doc}, tok); rr.Code != http.StatusCreated {
		t.Errorf("128-character policy name: %d, want 201", rr.Code)
	}
	for _, name := range []string{"alice", "svc+ci=1,a.b@corp-x_y", strings.Repeat("a", 64)} {
		if rr := doRequest(h, "POST", "/iam/users", map[string]string{"name": name}, tok); rr.Code != http.StatusCreated {
			t.Errorf("valid user %q: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	// Key issuance creates the user it names, and used to skip the rule.
	if code, _, _ := issueRaw(h, tok, map[string]interface{}{"userId": "bad/name"}); code != http.StatusBadRequest {
		t.Errorf("POST /keys with userId bad/name: %d, want 400", code)
	}
	if _, err := store.GetIAMUser("bad/name"); err == nil {
		t.Error("the key route created a user with an invalid name")
	}
	// A user stored under an old, now invalid, name can still be deleted.
	if err := store.CreateIAMUser(metadata.IAMUser{Name: "legacy name"}); err != nil {
		t.Fatal(err)
	}
	if rr := doRequest(h, "DELETE", "/iam/users/legacy%20name", nil, tok); rr.Code != http.StatusNoContent {
		t.Errorf("deleting a legacy-named user: %d", rr.Code)
	}
}

func TestSTSSessionArtifactsGoWithTheKey(t *testing.T) {
	h, store := newTestAPI(t)
	tok := getToken(t, h)
	if err := store.CreateIAMUser(metadata.IAMUser{Name: "src"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIAMUser(metadata.IAMUser{Name: "bystander"}); err != nil {
		t.Fatal(err)
	}
	doc := `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`

	// A policy with no user to scope is refused, not dropped in silence.
	rr := doRequest(h, "POST", "/sts/session-token", map[string]string{"policy": doc}, tok)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "policy needs userId") {
		t.Fatalf("policy without userId: %d %s, want 400", rr.Code, rr.Body.String())
	}
	// A session policy the authorizer could not use is refused too.
	rr = doRequest(h, "POST", "/sts/session-token", map[string]string{"policy": `{"Statement":[]}`, "userId": "src"}, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty session policy: %d, want 400", rr.Code)
	}

	mint := func() string {
		rr := doRequest(h, "POST", "/sts/session-token", map[string]string{"policy": doc, "userId": "src"}, tok)
		if rr.Code != http.StatusCreated {
			t.Fatalf("mint: %d %s", rr.Code, rr.Body.String())
		}
		var resp stsResponse
		json.Unmarshal(rr.Body.Bytes(), &resp)
		return resp.AccessKey
	}

	// Deleting the key removes its synthetic user and policy.
	ak := mint()
	if _, err := store.GetIAMUser("sts-" + ak); err != nil {
		t.Fatalf("no synthetic user was created: %v", err)
	}
	// It is plumbing, not a user anyone created, so it is not listed.
	rr = doRequest(h, "GET", "/iam/users", nil, tok)
	if strings.Contains(rr.Body.String(), "sts-"+ak) || !strings.Contains(rr.Body.String(), "bystander") {
		t.Fatalf("user list: %s", rr.Body.String())
	}
	if rr := doRequest(h, "DELETE", "/keys/"+ak, nil, tok); rr.Code != http.StatusNoContent {
		t.Fatalf("delete key: %d", rr.Code)
	}
	if _, err := store.GetIAMUser("sts-" + ak); err == nil {
		t.Error("the synthetic user outlived its key")
	}
	if _, err := store.GetIAMPolicy("sts-" + ak); err == nil {
		t.Error("the session policy outlived its key")
	}

	// Deleting the source user removes them too.
	ak = mint()
	if rr := doRequest(h, "DELETE", "/iam/users/src", nil, tok); rr.Code != http.StatusNoContent {
		t.Fatalf("delete user: %d", rr.Code)
	}
	if _, err := store.GetIAMUser("sts-" + ak); err == nil {
		t.Error("the synthetic user outlived its source user")
	}
	if _, err := store.GetIAMPolicy("sts-" + ak); err == nil {
		t.Error("the session policy outlived its source user")
	}
	if _, err := store.GetIAMUser("bystander"); err != nil {
		t.Error("an unrelated user was deleted")
	}
}

// The "does an access key own this policy" check was skipped when the key list
// could not be read, deleting a key's own policy whenever the store hiccuped.
func TestDeletePolicyRefusesWhenKeysCannotBeChecked(t *testing.T) {
	h, store := newTestAPI(t)
	tok := getToken(t, h)
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{Name: "access-key-AK1", Document: `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`}); err != nil {
		t.Fatal(err)
	}
	h.store = &faultyStore{StoreAPI: store, failListAccessKeys: true}
	rr := doRequest(h, "DELETE", "/iam/policies/access-key-AK1", nil, tok)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "could not check whether an access key owns") {
		t.Fatalf("got %d %s, want 503 naming the access key check", rr.Code, rr.Body.String())
	}
	if _, err := store.GetIAMPolicy("access-key-AK1"); err != nil {
		t.Fatal("the policy was deleted anyway")
	}
}

// A deleted group's name stayed on its members, so recreating it re-granted
// its new policies to people nobody had added.
func TestDeleteGroupRemovesItFromMembers(t *testing.T) {
	h, store := newTestAPI(t)
	tok := getToken(t, h)
	store.CreateIAMGroup(metadata.IAMGroup{Name: "ops"})
	store.CreateIAMGroup(metadata.IAMGroup{Name: "dev"})
	store.CreateIAMUser(metadata.IAMUser{Name: "erin", Groups: []string{"ops", "dev"}})
	if rr := doRequest(h, "DELETE", "/iam/groups/ops", nil, tok); rr.Code != http.StatusNoContent {
		t.Fatalf("delete group: %d", rr.Code)
	}
	u, _ := store.GetIAMUser("erin")
	if len(u.Groups) != 1 || u.Groups[0] != "dev" {
		t.Fatalf("erin's groups after deleting ops: %v, want [dev]", u.Groups)
	}
}

func TestIPRestrictionsAreValidated(t *testing.T) {
	h, store := newTestAPI(t)
	tok := getToken(t, h)
	store.CreateIAMUser(metadata.IAMUser{Name: "fay", AllowedCIDRs: []string{"10.0.0.0/8"}})
	rr := doRequest(h, "PUT", "/iam/users/fay/ip-restrictions", map[string][]string{"allowedCidrs": {"10.0.0.0/8", "10.0.0.0/33"}}, tok)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "10.0.0.0/33") {
		t.Fatalf("got %d %s, want 400 naming the bad entry", rr.Code, rr.Body.String())
	}
	u, _ := store.GetIAMUser("fay")
	if len(u.AllowedCIDRs) != 1 || u.AllowedCIDRs[0] != "10.0.0.0/8" {
		t.Fatalf("the restrictions changed anyway: %v", u.AllowedCIDRs)
	}
	if rr := doRequest(h, "PUT", "/iam/users/fay/ip-restrictions", map[string][]string{"allowedCidrs": {"192.0.2.0/24", "2001:db8::/32"}}, tok); rr.Code != http.StatusNoContent {
		t.Fatalf("valid CIDRs: %d", rr.Code)
	}
}
