package s3

import (
	"net/http"
	"testing"
)

func TestBucketDenyReadsEveryPolicyFormAWSAccepts(t *testing.T) {
	e := newFixEnv(t)
	expect(t, "create", e.admin(t, http.MethodPut, "/bkt", ""), http.StatusOK, "")
	ak, sk := e.addUser(t, "w", allow(`"s3:PutObject","s3:DeleteObject"`, `"arn:aws:s3:::bkt/*"`))
	// Baseline: string "false" denies plain-HTTP requests.
	pol := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::bkt/*","Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodPut, "/bkt/a", "x"); r.code != http.StatusForbidden {
		t.Fatalf("baseline: string false not denied: %d", r.code)
	}
	// Same policy with a JSON boolean (AWS accepts it, Terraform jsonencode emits it).
	pol = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::bkt/*","Condition":{"Bool":{"aws:SecureTransport":false}}}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodPut, "/bkt/a", "x"); r.code != http.StatusForbidden {
		t.Errorf("JSON boolean false: Deny ignored, PUT over HTTP got %d", r.code)
	}
	// A second statement with a numeric value voids an unrelated Deny.
	pol = `{"Version":"2012-10-17","Statement":[
	 {"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::bkt/*"},
	 {"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::bkt/*","Condition":{"NumericLessThan":{"s3:TlsVersion":1.2}}}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodDelete, "/bkt/a", ""); r.code != http.StatusForbidden {
		t.Errorf("unconditional Deny DeleteObject ignored because another statement has a number: %d", r.code)
	}
	// Single statement object (no array).
	pol = `{"Version":"2012-10-17","Statement":{"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::bkt/*"}}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodDelete, "/bkt/a", ""); r.code != http.StatusForbidden {
		t.Errorf("Statement as an object: Deny ignored: %d", r.code)
	}
	// Condition key in other case (AWS keys are case-insensitive).
	pol = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::bkt/*","Condition":{"Bool":{"aws:securetransport":"false"}}}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusNoContent, "")
	if r := e.as(t, ak, sk, http.MethodPut, "/bkt/b", "x"); r.code != http.StatusForbidden {
		t.Errorf("lower-case condition key: Deny ignored: %d", r.code)
	}
}

// A policy this server cannot read is refused instead of stored with none
// of its Denies applying.
func TestAnUnreadableBucketPolicyIsRefused(t *testing.T) {
	e := newFixEnv(t)
	expect(t, "create", e.admin(t, http.MethodPut, "/bkt", ""), http.StatusOK, "")
	pol := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::bkt/*","Condition":{"Bool":{"aws:SecureTransport":{"nested":1}}}}]}`
	expect(t, "policy", e.admin(t, http.MethodPut, "/bkt?policy", pol), http.StatusBadRequest, "MalformedPolicy")
}
