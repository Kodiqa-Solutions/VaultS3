package s3

import (
	"io"
	"net/http"
	"testing"
)

// scopedPolicy grants anonymous s3:GetObject on one prefix only.
const scopedPolicy = `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": "*",
    "Action": "s3:GetObject",
    "Resource": "arn:aws:s3:::scopetest/public/*"
  }]
}`

// anonGet performs an unsigned GET, exactly as an unauthenticated caller would.
func anonGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("anonymous GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestPublicReadPolicyIsScopedToItsResourceKey is a regression test for the
// report that a bucket policy scoped to one prefix published the whole bucket.
// The object key was never considered when evaluating anonymous access, so a
// policy naming "arn:aws:s3:::scopetest/public/*" also served secret/*.
func TestPublicReadPolicyIsScopedToItsResourceKey(t *testing.T) {
	_, store, _, ts := newObjTestServer(t)
	if err := store.CreateBucket("scopetest"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	const publicKey, secretKey = "public/ok.txt", "secret/credentials.env"
	const secretBody = "SECRET=should-never-be-public"
	if resp := doSigned(t, http.MethodPut, ts.URL+"/scopetest/"+publicKey, []byte("PUBLIC-INTENDED")); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT public object: status %d", resp.StatusCode)
	}
	if resp := doSigned(t, http.MethodPut, ts.URL+"/scopetest/"+secretKey, []byte(secretBody)); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT secret object: status %d", resp.StatusCode)
	}

	// Before any policy, both objects are private.
	if status, _ := anonGet(t, ts.URL+"/scopetest/"+publicKey); status != http.StatusForbidden {
		t.Fatalf("precondition: anonymous GET of %s before the policy = %d, want 403", publicKey, status)
	}
	if status, _ := anonGet(t, ts.URL+"/scopetest/"+secretKey); status != http.StatusForbidden {
		t.Fatalf("precondition: anonymous GET of %s before the policy = %d, want 403", secretKey, status)
	}

	if err := store.PutBucketPolicy("scopetest", []byte(scopedPolicy)); err != nil {
		t.Fatalf("PutBucketPolicy: %v", err)
	}

	// The named prefix is public, as the operator intended.
	status, body := anonGet(t, ts.URL+"/scopetest/"+publicKey)
	if status != http.StatusOK || body != "PUBLIC-INTENDED" {
		t.Fatalf("anonymous GET of the published prefix = %d %q, want 200 and the object body", status, body)
	}

	// Everything outside it stays private.
	status, body = anonGet(t, ts.URL+"/scopetest/"+secretKey)
	if status == http.StatusOK {
		t.Fatalf("a policy scoped to public/* published %s anonymously: %d %q", secretKey, status, body)
	}
	if status != http.StatusForbidden {
		t.Fatalf("anonymous GET of %s = %d, want 403", secretKey, status)
	}
}
