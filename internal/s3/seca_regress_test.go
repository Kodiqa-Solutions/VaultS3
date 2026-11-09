package s3

import (
	"net/http"
	"testing"
)

// Regression (5.0.14 security review): an object grant ("bucket/*") also
// matched the bucket, so s3:Put* on objects allowed bucket configuration:
// publishing the bucket through its policy, changing its quota and versioning.
func TestAnObjectGrantDoesNotReachBucketConfiguration(t *testing.T) {
	e := newFixEnv(t)
	expect(t, "create", e.admin(t, http.MethodPut, "/bkt", ""), http.StatusOK, "")
	ak, sk := e.addUser(t, "objonly", allow(`"s3:Put*","s3:Get*","s3:Delete*"`, `"arn:aws:s3:::bkt/*"`))
	pol := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}]}`
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/bkt?policy", pol},
		{http.MethodPut, "/bkt?quota", `{"max_size_bytes":0,"max_objects":0}`},
		{http.MethodPut, "/bkt?versioning", `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`},
		{http.MethodDelete, "/bkt", ""},
	} {
		if r := e.as(t, ak, sk, c.method, c.path, c.body); r.code != http.StatusForbidden {
			t.Errorf("%s %s with an object-only grant: %d, want 403", c.method, c.path, r.code)
		}
	}
	// Its objects are still its own.
	expect(t, "object put", e.as(t, ak, sk, http.MethodPut, "/bkt/o", "x"), http.StatusOK, "")
}

// Regression (5.0.14 security review): a bucket-policy Deny applied only to
// anonymous requests, never to a signed one.
func TestABucketPolicyDenyAppliesToSignedRequests(t *testing.T) {
	e := newFixEnv(t)
	expect(t, "create", e.admin(t, http.MethodPut, "/bkt", ""), http.StatusOK, "")
	e.admin(t, http.MethodPut, "/bkt/keep.txt", "data")
	e.admin(t, http.MethodPut, "/bkt/other.txt", "data")
	pol := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":["s3:DeleteObject","s3:GetObject"],"Resource":"arn:aws:s3:::bkt/keep.txt"}]}`
	expect(t, "put policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	ak, sk := e.addUser(t, "rw", allow(`"s3:GetObject","s3:DeleteObject"`, `"arn:aws:s3:::bkt/*"`))
	if r := e.as(t, ak, sk, http.MethodGet, "/bkt/keep.txt", ""); r.code != http.StatusForbidden {
		t.Errorf("GET despite a bucket-policy Deny: %d", r.code)
	}
	if r := e.as(t, ak, sk, http.MethodDelete, "/bkt/keep.txt", ""); r.code != http.StatusForbidden {
		t.Errorf("DELETE despite a bucket-policy Deny: %d", r.code)
	}
	expect(t, "an object the Deny does not name", e.as(t, ak, sk, http.MethodGet, "/bkt/other.txt", ""), http.StatusOK, "")
	// A Deny naming another user does not apply to this one.
	pol2 := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":{"AWS":"arn:aws:iam::000000000000:user/mallory"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}]}`
	expect(t, "policy 2", e.admin(t, http.MethodPut, "/bkt?policy", pol2), http.StatusNoContent, "")
	expect(t, "another user's Deny", e.as(t, ak, sk, http.MethodGet, "/bkt/other.txt", ""), http.StatusOK, "")
	m, msk := e.addUser(t, "mallory", allow(`"s3:GetObject"`, `"arn:aws:s3:::bkt/*"`))
	if r := e.as(t, m, msk, http.MethodGet, "/bkt/other.txt", ""); r.code != http.StatusForbidden {
		t.Errorf("a Deny naming the user did not apply: %d", r.code)
	}
}

// Regression (5.0.14 security review): ${aws:username} was never
// substituted, so a Deny written with it never applied and a home-folder
// Allow granted the literal "${aws:username}" folder to everyone holding it.
func TestPolicyVariablesAreSubstituted(t *testing.T) {
	e := newFixEnv(t)
	expect(t, "create", e.admin(t, http.MethodPut, "/bkt", ""), http.StatusOK, "")
	e.admin(t, http.MethodPut, "/bkt/locked-alice/x", "alice private")
	doc := `{"Version":"2012-10-17","Statement":[
	 {"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::bkt/*"},
	 {"Effect":"Deny","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/locked-${aws:username}/*"}]}`
	ak, sk := e.addUser(t, "alice", doc)
	if r := e.as(t, ak, sk, http.MethodGet, "/bkt/locked-alice/x", ""); r.code != http.StatusForbidden {
		t.Errorf("a Deny with ${aws:username} did not apply: %d", r.code)
	}
	ak2, sk2 := e.addUser(t, "bob", allow(`"s3:GetObject","s3:PutObject"`, `"arn:aws:s3:::bkt/home/${aws:username}/*"`))
	expect(t, "bob's own home folder", e.as(t, ak2, sk2, http.MethodPut, "/bkt/home/bob/f", "x"), http.StatusOK, "")
	if r := e.as(t, ak2, sk2, http.MethodPut, "/bkt/home/$%7Baws:username%7D/f", "x"); r.code != http.StatusForbidden {
		t.Errorf("the literal ${aws:username} folder: %d", r.code)
	}
	if r := e.as(t, ak2, sk2, http.MethodPut, "/bkt/home/alice/f", "x"); r.code != http.StatusForbidden {
		t.Errorf("another user's home folder: %d", r.code)
	}
}
