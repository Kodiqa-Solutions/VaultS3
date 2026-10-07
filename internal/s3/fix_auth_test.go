package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/ratelimit"
)

// Every bucket and object sub-resource maps to its own AWS action. They used
// to fall back on the method alone, so s3:CreateBucket rewrote lifecycle,
// website and replication configuration, DELETE ?policy was s3:GetBucketPolicy,
// and every POST on a key, multipart upload included, needed s3:*.
func TestMapMethodToActionCoversEverySubresource(t *testing.T) {
	q := func(kv ...string) map[string][]string {
		m := map[string][]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = append(m[kv[i]], kv[i+1])
		}
		return m
	}
	cases := []struct {
		method, key string
		query       map[string][]string
		want        string
	}{
		{"PUT", "", q("lifecycle", ""), "s3:PutLifecycleConfiguration"},
		{"GET", "", q("lifecycle", ""), "s3:GetLifecycleConfiguration"},
		{"DELETE", "", q("lifecycle", ""), "s3:PutLifecycleConfiguration"},
		{"PUT", "", q("website", ""), "s3:PutBucketWebsite"},
		{"GET", "", q("website", ""), "s3:GetBucketWebsite"},
		{"DELETE", "", q("website", ""), "s3:DeleteBucketWebsite"},
		{"PUT", "", q("replication", ""), "s3:PutReplicationConfiguration"},
		{"GET", "", q("replication", ""), "s3:GetReplicationConfiguration"},
		{"DELETE", "", q("replication", ""), "s3:PutReplicationConfiguration"},
		{"PUT", "", q("notification", ""), "s3:PutBucketNotification"},
		{"GET", "", q("notification", ""), "s3:GetBucketNotification"},
		{"PUT", "", q("lambda", ""), "s3:PutBucketNotification"},
		{"PUT", "", q("versioning", ""), "s3:PutBucketVersioning"},
		{"GET", "", q("versioning", ""), "s3:GetBucketVersioning"},
		{"GET", "", q("versions", ""), "s3:ListBucketVersions"},
		{"PUT", "", q("encryption", ""), "s3:PutEncryptionConfiguration"},
		{"GET", "", q("encryption", ""), "s3:GetEncryptionConfiguration"},
		{"DELETE", "", q("encryption", ""), "s3:PutEncryptionConfiguration"},
		{"PUT", "", q("object-lock", ""), "s3:PutBucketObjectLockConfiguration"},
		{"GET", "", q("object-lock", ""), "s3:GetBucketObjectLockConfiguration"},
		{"PUT", "", q("policy", ""), "s3:PutBucketPolicy"},
		{"GET", "", q("policy", ""), "s3:GetBucketPolicy"},
		{"DELETE", "", q("policy", ""), "s3:DeleteBucketPolicy"},
		{"PUT", "", q("cors", ""), "s3:PutBucketCORS"},
		{"GET", "", q("cors", ""), "s3:GetBucketCORS"},
		{"PUT", "", q("tagging", ""), "s3:PutBucketTagging"},
		{"GET", "", q("tagging", ""), "s3:GetBucketTagging"},
		{"PUT", "", q("acl", ""), "s3:PutBucketAcl"},
		{"GET", "", q("acl", ""), "s3:GetBucketAcl"},
		{"GET", "", q("location", ""), "s3:GetBucketLocation"},
		{"PUT", "", q("publicAccessBlock", ""), "s3:PutBucketPublicAccessBlock"},
		{"PUT", "", q("logging", ""), "s3:PutBucketLogging"},
		{"GET", "", q("uploads", ""), "s3:ListBucketMultipartUploads"},
		{"PUT", "", q("quota", ""), "s3:PutBucketQuota"},
		{"PUT", "", q("durability", ""), "s3:PutBucketDurability"},
		{"POST", "", q("delete", ""), "s3:DeleteObject"},
		{"POST", "", nil, "s3:PutObject"},
		{"PUT", "", nil, "s3:CreateBucket"},
		{"DELETE", "", nil, "s3:DeleteBucket"},
		{"GET", "", nil, "s3:ListBucket"},
		{"HEAD", "", nil, "s3:ListBucket"},
		// A request naming two sub-resources maps to the one the router serves.
		{"PUT", "", q("versions", "", "lifecycle", ""), "s3:PutLifecycleConfiguration"},
		// A method a sub-resource does not have fails closed.
		{"POST", "", q("lifecycle", ""), "s3:*"},

		{"PUT", "k", q("retention", ""), "s3:PutObjectRetention"},
		{"GET", "k", q("retention", ""), "s3:GetObjectRetention"},
		{"PUT", "k", q("legal-hold", ""), "s3:PutObjectLegalHold"},
		{"GET", "k", q("legal-hold", ""), "s3:GetObjectLegalHold"},
		{"PUT", "k", q("acl", ""), "s3:PutObjectAcl"},
		{"GET", "k", q("acl", ""), "s3:GetObjectAcl"},
		{"PUT", "k", q("tagging", ""), "s3:PutObjectTagging"},
		{"GET", "k", q("tagging", ""), "s3:GetObjectTagging"},
		{"DELETE", "k", q("tagging", ""), "s3:DeleteObjectTagging"},
		{"PUT", "k", q("tagging", "", "versionId", "v1"), "s3:PutObjectVersionTagging"},
		{"GET", "k", q("tagging", "", "versionId", "v1"), "s3:GetObjectVersionTagging"},
		{"DELETE", "k", q("tagging", "", "versionId", "v1"), "s3:DeleteObjectVersionTagging"},
		{"GET", "k", q("attributes", ""), "s3:GetObjectAttributes"},
		{"POST", "k", q("uploads", ""), "s3:PutObject"},
		{"POST", "k", q("uploadId", "ab12"), "s3:PutObject"},
		{"PUT", "k", q("uploadId", "ab12", "partNumber", "1"), "s3:PutObject"},
		{"GET", "k", q("uploadId", "ab12"), "s3:ListMultipartUploadParts"},
		{"DELETE", "k", q("uploadId", "ab12"), "s3:AbortMultipartUpload"},
		{"POST", "k", q("restore", ""), "s3:RestoreObject"},
		{"POST", "k", q("select", "", "select-type", "2"), "s3:GetObject"},
		// restore is POST-only in the router; GET ?restore is a plain read.
		{"GET", "k", q("restore", ""), "s3:GetObject"},
		{"GET", "k", q("versionId", "v1"), "s3:GetObjectVersion"},
		{"DELETE", "k", q("versionId", "v1"), "s3:DeleteObjectVersion"},
		{"POST", "k", nil, "s3:*"},
	}
	for _, c := range cases {
		got := mapMethodToAction(c.method, "b", c.key, c.query)
		if got != c.want {
			t.Errorf("%s key=%q %v: got %s, want %s", c.method, c.key, c.query, got, c.want)
		}
	}
}

// End to end: s3:CreateBucket alone no longer rewrites a bucket's lifecycle,
// and s3:PutObject alone can now upload in parts.
func TestCreateBucketGrantDoesNotReachSubresources(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/fbk", "").must(t, "create bucket")
	ak, sk := e.addUser(t, "maker", allow(`"s3:CreateBucket","s3:PutObject"`, `"arn:aws:s3:::fbk","arn:aws:s3:::fbk/*"`))

	lc := `<LifecycleConfiguration><Rule><ID>x</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`
	expect(t, "PUT ?lifecycle with only s3:CreateBucket", e.as(t, ak, sk, http.MethodPut, "/fbk?lifecycle", lc), http.StatusForbidden, "AccessDenied")

	r := e.as(t, ak, sk, http.MethodPost, "/fbk/big?uploads", "")
	expect(t, "CreateMultipartUpload with s3:PutObject", r, http.StatusOK, "")
}

// aws:SourceIp is the TCP peer. It used to come from X-Forwarded-For, so a
// policy allowing one address range was satisfied by anyone who sent that
// header, and the external authorizer was told the same forged address.
func TestSourceIpConditionIgnoresForwardedFor(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/ipb", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/ipb/k", "data").must(t, "put")
	office := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*",
		"Condition":{"IpAddress":{"aws:SourceIp":["203.0.113.0/24"]}}}]}`
	ak, sk := e.addUser(t, "remote", office)
	expect(t, "GET claiming an office address in X-Forwarded-For", e.as(t, ak, sk, http.MethodGet, "/ipb/k", "", "X-Forwarded-For", "203.0.113.7"),
		http.StatusForbidden, "AccessDenied")

	local := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*",
		"Condition":{"IpAddress":{"aws:SourceIp":["127.0.0.1/32","::1/128"]}}}]}`
	ak2, sk2 := e.addUser(t, "local", local)
	expect(t, "GET from the allowed peer with a forged header", e.as(t, ak2, sk2, http.MethodGet, "/ipb/k", "", "X-Forwarded-For", "198.51.100.1"),
		http.StatusOK, "")
}

// The per-key rate limit is charged only once the key is proven. It was charged
// from the unsigned Authorization header, so naming a victim key in junk
// requests drained its allowance and the victim was then refused.
func TestRateLimitChargesAKeyOnlyAfterAuthentication(t *testing.T) {
	e := newFixEnv(t)
	lim := ratelimit.NewLimiter(1000, 1000, 0.001, 3)
	t.Cleanup(lim.Stop)
	e.h.SetRateLimiter(lim)
	e.admin(t, http.MethodPut, "/rlb", "").must(t, "create bucket")

	for i := 0; i < 10; i++ {
		// The victim's key with a wrong secret: unauthenticated.
		r := e.as(t, testAccessKey, "not-the-secret", http.MethodGet, "/rlb", "")
		if r.code == http.StatusTooManyRequests {
			continue
		}
		expect(t, "forged request", r, http.StatusForbidden, "SignatureDoesNotMatch")
	}
	expect(t, "the victim's own request", e.admin(t, http.MethodGet, "/rlb", ""), http.StatusOK, "")
}

// signWith signs r the way a client would, but with every input the test wants
// to get wrong under its control.
func signWith(r *http.Request, ak, sk, amzDate, scopeDate, signedHeaders string) {
	if amzDate != "" {
		r.Header.Set("X-Amz-Date", amzDate)
	}
	h := sha256.Sum256(nil)
	r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(h[:]))
	canonical := buildCanonicalRequest(r, signedHeaders, "")
	sts := buildStringToSignAt(amzDate, scopeDate, testRegion, "s3", canonical)
	sig := hex.EncodeToString(hmacSHA256(deriveSigningKey(sk, scopeDate, testRegion, "s3"), []byte(sts)))
	r.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s",
		ak, scopeDate, testRegion, signedHeaders, sig))
}

// Each of these was signed correctly over what it carried, and was accepted.
func TestHeaderAuthRejectsWhatItUsedToSkip(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/hab", "").must(t, "create bucket")
	now := time.Now().UTC()
	today, stamp := now.Format("20060102"), now.Format("20060102T150405Z")
	yesterday := now.Add(-24 * time.Hour).Format("20060102")

	do := func(amzDate, scope, signed string) result {
		req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/hab", nil)
		signWith(req, testAccessKey, testSecretKey, amzDate, scope, signed)
		return send(t, req)
	}
	// Control: the same signer with honest inputs is accepted.
	expect(t, "control", do(stamp, today, "host;x-amz-content-sha256;x-amz-date"), http.StatusOK, "")

	// An unreadable timestamp skipped the skew check, so the request never expired.
	expect(t, "unparseable X-Amz-Date", do("not-a-date", today, "host;x-amz-content-sha256;x-amz-date"), http.StatusForbidden, "AccessDenied")
	// A key derived for another day signed a request stamped today.
	expect(t, "scope date differs from X-Amz-Date", do(stamp, yesterday, "host;x-amz-content-sha256;x-amz-date"), http.StatusBadRequest, "AuthorizationHeaderMalformed")
	// Without host signed, the signature does not bind the request to this endpoint.
	expect(t, "host not signed", do(stamp, today, "x-amz-content-sha256;x-amz-date"), http.StatusForbidden, "AccessDenied")
}

func TestPresignedURLRejectsWhatItUsedToSkip(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pab", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/pab/k", "v").must(t, "put")

	presign := func(at time.Time, expires string) result {
		u, _ := url.Parse(e.ts.URL + "/pab/k")
		q := url.Values{}
		q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
		q.Set("X-Amz-Credential", fmt.Sprintf("%s/%s/%s/s3/aws4_request", testAccessKey, at.Format("20060102"), testRegion))
		q.Set("X-Amz-Date", at.Format("20060102T150405Z"))
		if expires != "" {
			q.Set("X-Amz-Expires", expires)
		}
		q.Set("X-Amz-SignedHeaders", "host")
		canonical := fmt.Sprintf("GET\n%s\n%s\nhost:%s\n\nhost\nUNSIGNED-PAYLOAD", uriEncodePath(u.Path), canonicalQueryEncode(q), u.Host)
		sts := buildStringToSignAt(at.Format("20060102T150405Z"), at.Format("20060102"), testRegion, "s3", canonical)
		q.Set("X-Amz-Signature", hex.EncodeToString(hmacSHA256(deriveSigningKey(testSecretKey, at.Format("20060102"), testRegion, "s3"), []byte(sts))))
		u.RawQuery = q.Encode()
		req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
		return send(t, req)
	}
	now := time.Now().UTC()
	expect(t, "control", presign(now, "300"), http.StatusOK, "")
	// A missing lifetime defaulted to the seven day maximum.
	expect(t, "no X-Amz-Expires", presign(now, ""), http.StatusBadRequest, "AuthorizationQueryParametersError")
	// A URL dated tomorrow was accepted, so it outlived its stated lifetime.
	expect(t, "dated in the future", presign(now.Add(24*time.Hour), "300"), http.StatusForbidden, "AccessDenied")
}

// A refusal says only what AWS says, and the specific state of a credential
// is revealed only to a caller whose signature proved it holds the secret.
func TestAuthRefusalsDoNotLeakCredentialState(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/alb", "").must(t, "create bucket")
	now := time.Now().UTC()
	if err := e.store.CreateAccessKey(metadata.AccessKey{AccessKey: "AKEXPIRED", SecretKey: "expired-secret", CreatedAt: now,
		ExpiresAt: now.Add(-time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}

	expect(t, "unknown key", e.as(t, "AKNOBODY", "x", http.MethodGet, "/alb", ""), http.StatusForbidden, "InvalidAccessKeyId")
	expect(t, "known key, wrong secret", e.as(t, testAccessKey, "wrong", http.MethodGet, "/alb", ""), http.StatusForbidden, "SignatureDoesNotMatch")
	// Expiry used to be announced to anyone who named the key.
	r := e.as(t, "AKEXPIRED", "wrong", http.MethodGet, "/alb", "")
	expect(t, "expired key, wrong secret", r, http.StatusForbidden, "SignatureDoesNotMatch")
	if strings.Contains(r.body, "expired") {
		t.Errorf("the refusal told a caller without the secret that the key has expired: %s", r.body)
	}
	expect(t, "expired key, right secret", e.as(t, "AKEXPIRED", "expired-secret", http.MethodGet, "/alb", ""), http.StatusBadRequest, "ExpiredToken")

	// The IP check's refusal used to carry its own text, which says whether
	// the address hit a blocklist or missed an allowlist.
	ak, sk := e.addUser(t, "fenced", allow(`"s3:*"`, `"*"`), "10.99.0.0/16")
	r = e.as(t, ak, sk, http.MethodGet, "/alb", "")
	expect(t, "outside the allowlist", r, http.StatusForbidden, "AccessDenied")
	if !strings.Contains(r.body, "<Message>Access Denied</Message>") {
		t.Errorf("the refusal explained the IP decision: %s", r.body)
	}
}

// policyReadFails makes every read of a user's attached policies fail.
type policyReadFails struct {
	metadata.StoreAPI
}

func (policyReadFails) GetUserPolicies(string) ([]metadata.IAMPolicy, error) {
	return nil, errors.New("disk read error")
}

// A user's policies that cannot be read are not an empty set. The error was
// ignored while the key's own grant still loaded, so a Deny attached to the
// user was dropped and the key's grant decided alone.
func TestUnreadableUserPoliciesDenyTheIdentity(t *testing.T) {
	store, _ := keyPolicyStore(t)
	a := NewAuthenticator("admin", "secret", policyReadFails{store}, nil, nil)
	id, _, err := a.resolveIdentity("AKONE", httptestRequestGET())
	if err != nil {
		t.Fatal(err)
	}
	if !id.PolicyLoadFailed {
		t.Fatal("an unreadable user policy set was treated as empty")
	}
	if err := a.Authorize(id, "s3:PutObject", "arn:aws:s3:::bkt-one/x"); err == nil {
		t.Error("the key's own grant was honoured while the user's policies could not be read")
	}
}

func httptestRequestGET() *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	return r
}

// stsEnv is a user with a scoped STS session derived from it, set up the way
// the STS endpoint does: the session policy on a synthetic user, the source
// user named on the key.
func stsEnv(t *testing.T, userPolicy, sessionPolicy string, cidrs ...string) (*fixEnv, string) {
	t.Helper()
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/sts", "").must(t, "create bucket")
	e.admin(t, http.MethodPut, "/sts/k", "v").must(t, "put")
	e.addUser(t, "src", userPolicy, cidrs...)
	now := time.Now().UTC()
	if err := e.store.CreateIAMPolicy(metadata.IAMPolicy{Name: "sts-AKSESS", CreatedAt: now, Document: sessionPolicy}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateIAMUser(metadata.IAMUser{Name: "sts-AKSESS", CreatedAt: now, PolicyARNs: []string{"sts-AKSESS"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateAccessKey(metadata.AccessKey{AccessKey: "AKSESS", SecretKey: "sess-secret", CreatedAt: now,
		UserID: "sts-AKSESS", SourceUserID: "src", SessionToken: "tok", ExpiresAt: now.Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	return e, "tok"
}

func (e *fixEnv) session(t *testing.T, tok, method, path, body string) result {
	t.Helper()
	return e.as(t, "AKSESS", "sess-secret", method, path, body, "X-Amz-Security-Token", tok)
}

// A scoped session may do only what BOTH its session policy and its user allow.
// It was evaluated on the session policy alone, so a session could do what its
// user could not.
func TestScopedSessionIsTheIntersectionWithItsUser(t *testing.T) {
	e, tok := stsEnv(t, allow(`"s3:GetObject"`, `"*"`), allow(`"s3:GetObject","s3:PutObject"`, `"*"`))
	expect(t, "GET, allowed by both", e.session(t, tok, http.MethodGet, "/sts/k", ""), http.StatusOK, "")
	expect(t, "PUT, allowed by the session only", e.session(t, tok, http.MethodPut, "/sts/k", "overwrite"), http.StatusForbidden, "AccessDenied")
}

// A session ends with the user it was issued for, and carries its allowlist.
func TestScopedSessionNeedsItsUser(t *testing.T) {
	e, tok := stsEnv(t, allow(`"s3:*"`, `"*"`), allow(`"s3:GetObject"`, `"*"`))
	expect(t, "control", e.session(t, tok, http.MethodGet, "/sts/k", ""), http.StatusOK, "")
	if err := e.store.DeleteIAMUser("src"); err != nil {
		t.Fatal(err)
	}
	expect(t, "after the user was deleted", e.session(t, tok, http.MethodGet, "/sts/k", ""), http.StatusForbidden, "AccessDenied")
}

func TestScopedSessionCarriesItsUsersAllowlist(t *testing.T) {
	e, tok := stsEnv(t, allow(`"s3:*"`, `"*"`), allow(`"s3:GetObject"`, `"*"`), "10.99.0.0/16")
	expect(t, "from outside the user's allowlist", e.session(t, tok, http.MethodGet, "/sts/k", ""), http.StatusForbidden, "AccessDenied")
}
