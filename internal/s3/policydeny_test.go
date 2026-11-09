package s3

import (
	"net/http"
	"testing"
)

// R9-1: a standard AWS bucket policy ("deny uploads that make the object
// public") uses a condition key this server never puts in the context. On AWS
// StringEquals on an absent key is false, so a plain upload is allowed. Here
// the missing key makes the Deny "undetermined", which now applies to every
// signed request: every non-admin user loses uploads after the upgrade.
func TestBucketDenyOnAnAbsentConditionKeyIsFalseAsOnAWS(t *testing.T) {
	e := newFixEnv(t)
	expect(t, "create", e.admin(t, http.MethodPut, "/bkt", ""), http.StatusOK, "")
	pol := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::bkt/*",
	 "Condition":{"StringEquals":{"s3:x-amz-acl":["public-read","public-read-write"]}}}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	ak, sk := e.addUser(t, "writer", allow(`"s3:PutObject"`, `"arn:aws:s3:::bkt/*"`))
	if r := e.as(t, ak, sk, http.MethodPut, "/bkt/private.txt", "x"); r.code != http.StatusOK {
		t.Errorf("a private upload (no ACL header) refused by a Deny that only names public ACLs: %d %s", r.code, errCode(r.body))
	}
	// An operator this evaluator does not know (ArnLike, Null, Numeric*...)
	// also makes the Deny apply to everyone.
	pol2 := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::bkt","arn:aws:s3:::bkt/*"],
	 "Condition":{"ArnLike":{"aws:PrincipalArn":"arn:aws:iam::*:user/contractor-*"}}}]}`
	expect(t, "policy2", e.admin(t, http.MethodPut, "/bkt?policy", pol2), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodPut, "/bkt/b.txt", "x"); r.code != http.StatusOK {
		t.Errorf("a Deny for contractor-* users blocks user 'writer': %d %s", r.code, errCode(r.body))
	}
}

// R9-2: a bucket-policy Deny written with NotPrincipal or NotAction (the
// usual "deny everyone except" forms) is ignored for signed requests.
func TestBucketDenyHonoursNotActionAndNotPrincipal(t *testing.T) {
	e := newFixEnv(t)
	expect(t, "create", e.admin(t, http.MethodPut, "/bkt", ""), http.StatusOK, "")
	e.admin(t, http.MethodPut, "/bkt/o", "data")
	ak, sk := e.addUser(t, "rw", allow(`"s3:GetObject","s3:DeleteObject","s3:PutObject"`, `"arn:aws:s3:::bkt/*"`))
	// Read-only bucket for everyone: deny every action but GetObject.
	pol := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","NotAction":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodDelete, "/bkt/o", ""); r.code != http.StatusForbidden {
		t.Errorf("DELETE despite Deny NotAction s3:GetObject: %d", r.code)
	}
	// Deny everyone but 'admin2'.
	pol2 := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","NotPrincipal":{"AWS":"arn:aws:iam::000000000000:user/admin2"},"Action":"s3:*","Resource":"arn:aws:s3:::bkt/*"}]}`
	expect(t, "policy2", e.admin(t, http.MethodPut, "/bkt?policy", pol2), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodPut, "/bkt/p", "x"); r.code != http.StatusForbidden {
		t.Errorf("PUT despite Deny NotPrincipal admin2: %d", r.code)
	}
}

// R9-6: the signature throttle is per (address, key), and access keys are
// not secret (they are in every presigned URL). Behind a proxy without
// trusted_proxies, on a cluster (requests proxied from another node arrive
// from that node), or behind NAT, everyone has the same address: anyone can
// lock any key, the admin's included, for 15 minutes, again and again.
func TestOthersBadSignaturesNeverLockOutTheRightOne(t *testing.T) {
	e := newFixEnv(t)
	expect(t, "create", e.admin(t, http.MethodPut, "/bkt", ""), http.StatusOK, "")
	for i := 0; i < sigFailureLimit; i++ {
		e.as(t, testAccessKey, "not-the-secret", http.MethodGet, "/bkt", "")
	}
	if r := e.admin(t, http.MethodGet, "/bkt", ""); r.code != http.StatusOK {
		t.Errorf("the admin, with the right secret, after someone else's %d bad signatures: %d %s", sigFailureLimit, r.code, errCode(r.body))
	}
}

// The usual "HTTPS only" bucket policy still denies plain HTTP, and a
// negated operator on a missing key is true, as on AWS.
func TestBucketDenySecureTransportAndNegatedOperators(t *testing.T) {
	e := newFixEnv(t)
	expect(t, "create", e.admin(t, http.MethodPut, "/bkt", ""), http.StatusOK, "")
	ak, sk := e.addUser(t, "rw", allow(`"s3:PutObject","s3:GetObject"`, `"arn:aws:s3:::bkt/*"`))
	pol := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::bkt/*","Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodPut, "/bkt/a", "x"); r.code != http.StatusForbidden {
		t.Errorf("plain HTTP despite a SecureTransport Deny: %d", r.code)
	}
	pol = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::bkt/*","Condition":{"StringNotEquals":{"s3:x-amz-server-side-encryption":"AES256"}}}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodPut, "/bkt/b", "x"); r.code != http.StatusForbidden {
		t.Errorf("StringNotEquals on a missing key did not deny: %d", r.code)
	}
	// ...IfExists holds when the key is missing (AWS), so the Deny applies.
	pol = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::bkt/*","Condition":{"StringEqualsIfExists":{"s3:x-amz-server-side-encryption":"aws:kms"}}}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodPut, "/bkt/c", "x"); r.code != http.StatusForbidden {
		t.Errorf("...IfExists on a missing key did not deny: %d", r.code)
	}
	// Null: true holds when the key is missing.
	pol = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::bkt/*","Condition":{"Null":{"aws:Referer":"false"}}}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodPut, "/bkt/d", "x"); r.code != http.StatusOK {
		t.Errorf("Null false on a missing key denied: %d", r.code)
	}
}
