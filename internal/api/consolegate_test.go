package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// sessionWithPolicy creates an IAM user holding one policy and returns a
// genuine session for it, minted by this server's own signer.
func sessionWithPolicy(t *testing.T, h *APIHandler, store *metadata.Store, user, doc string) string {
	t.Helper()
	var policies []string
	if doc != "" {
		name := user + "-policy"
		if err := store.CreateIAMPolicy(metadata.IAMPolicy{Name: name, Document: doc}); err != nil {
			t.Fatal(err)
		}
		policies = []string{name}
	}
	if err := store.CreateIAMUser(metadata.IAMUser{Name: user, PolicyARNs: policies}); err != nil {
		t.Fatal(err)
	}
	tok, err := h.jwt.Generate(user, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Every one of these answered a session with no policies at all before the
// gate became an allowlist. They must now be refused for the reason "admin
// access required", not for some unrelated one.
func TestNonAdminSessionIsRefusedEveryAdminRoute(t *testing.T) {
	h, store := newTestAPI(t)
	tok := sessionWithPolicy(t, h, store, "nobody", "")

	routes := []struct{ method, path string }{
		{"GET", "/logs"},
		{"GET", "/events"},
		{"GET", "/trace"},
		{"GET", "/notifications"},
		{"GET", "/system"},
		{"GET", "/diagnostics"},
		{"POST", "/vectors/query"},
		{"GET", "/vectors/status"},
		{"POST", "/cluster/repair"},
		{"GET", "/cluster/repair"},
		{"GET", "/cluster/info"},
		{"GET", "/cluster/status"},
		{"GET", "/ratelimit/status"},
		{"GET", "/some-route-added-next-year"},
	}
	for _, rt := range routes {
		rr := doRequest(h, rt.method, rt.path, nil, tok)
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "admin access required") {
			t.Errorf("%s %s answered a non-admin %d %s, want 403 admin access required",
				rt.method, rt.path, rr.Code, strings.TrimSpace(rr.Body.String()))
		}
	}

	// The routes the dashboard needs as a non-admin still answer.
	for _, p := range []string{"/auth/me", "/version", "/buckets", "/stats", "/activity", "/tco"} {
		if rr := doRequest(h, "GET", p, nil, tok); rr.Code != http.StatusOK {
			t.Errorf("GET %s answered a non-admin %d, the dashboard needs it", p, rr.Code)
		}
	}
}

// /stats, /tco and /activity are open to non-admins but show only the buckets
// the caller may list. They used to list every bucket, undoing the filtering
// GET /buckets applies.
func TestCrossBucketReadsAreFilteredForNonAdmins(t *testing.T) {
	h, store := newTestAPI(t)
	for _, b := range []string{"mine", "theirs"} {
		if err := store.CreateBucket(b); err != nil {
			t.Fatal(err)
		}
	}
	tok := sessionWithPolicy(t, h, store, "ann",
		`{"Statement":[{"Effect":"Allow","Action":"s3:ListBucket","Resource":"arn:aws:s3:::mine"}]}`)
	h.activity.Record(ActivityEntry{Bucket: "theirs", Key: "payroll.csv", ClientIP: "198.51.100.7"})
	h.activity.Record(ActivityEntry{Bucket: "mine", Key: "notes.txt", ClientIP: "198.51.100.8"})

	rr := doRequest(h, "GET", "/stats", nil, tok)
	var stats statsResponse
	json.NewDecoder(rr.Body).Decode(&stats)
	if rr.Code != http.StatusOK || stats.TotalBuckets != 1 || len(stats.Buckets) != 1 || stats.Buckets[0].Name != "mine" {
		t.Errorf("/stats showed a non-admin %+v, want only bucket mine", stats.Buckets)
	}

	rr = doRequest(h, "GET", "/activity", nil, tok)
	body := rr.Body.String()
	if strings.Contains(body, "payroll.csv") || strings.Contains(body, "198.51.100") || !strings.Contains(body, "notes.txt") {
		t.Errorf("/activity showed a non-admin %s, want only the call on mine, without the client IP", body)
	}

	// Admin still sees everything.
	rr = doRequest(h, "GET", "/activity", nil, getToken(t, h))
	if !strings.Contains(rr.Body.String(), "payroll.csv") || !strings.Contains(rr.Body.String(), "198.51.100.7") {
		t.Errorf("admin lost activity entries: %s", rr.Body.String())
	}
}

// Creating a bucket named no existing bucket, so the per-bucket gate never saw
// it and any session could create buckets.
func TestCreateBucketNeedsCreateBucketPermission(t *testing.T) {
	h, store := newTestAPI(t)
	none := sessionWithPolicy(t, h, store, "nobody", "")
	rr := doRequest(h, "POST", "/buckets", map[string]string{"name": "sneaky"}, none)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "s3:CreateBucket") {
		t.Fatalf("a session with no policies got %d %s, want 403 naming s3:CreateBucket", rr.Code, rr.Body.String())
	}
	if store.BucketExists("sneaky") {
		t.Fatal("the bucket was created anyway")
	}

	may := sessionWithPolicy(t, h, store, "builder",
		`{"Statement":[{"Effect":"Allow","Action":"s3:CreateBucket","Resource":"arn:aws:s3:::team-*"}]}`)
	if rr := doRequest(h, "POST", "/buckets", map[string]string{"name": "team-a"}, may); rr.Code != http.StatusCreated {
		t.Fatalf("an allowed create answered %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequest(h, "POST", "/buckets", map[string]string{"name": "other-b"}, may); rr.Code != http.StatusForbidden {
		t.Fatalf("a create outside the grant answered %d", rr.Code)
	}
}

// A token in the query string is for the two download routes only, on GET. It
// used to be accepted on any path containing "/download", for any method.
func TestTokenInURLOnlyForDownloads(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/api/v1/buckets/b/download/k.txt", true},
		{"GET", "/api/v1/buckets/b/download/dir/k.txt", true},
		{"GET", "/api/v1/buckets/b/download-zip", true},
		{"PUT", "/api/v1/buckets/downloads-x/policy", false},
		{"DELETE", "/api/v1/iam/users/download-bot", false},
		{"DELETE", "/api/v1/buckets/b/download/k.txt", false},
		{"POST", "/api/v1/buckets/b/download-zip", false},
		{"GET", "/api/v1/buckets/download/objects", false},
		{"GET", "/api/v1/buckets/b/download", false},
		{"GET", "/api/v1/keys/download", false},
	}
	for _, c := range cases {
		if got := allowsTokenInURL(c.method, c.path); got != c.want {
			t.Errorf("allowsTokenInURL(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}

	// End to end: an admin session leaked from a download link cannot change a
	// bucket policy.
	h, store := newTestAPI(t)
	if err := store.CreateBucket("downloads-x"); err != nil {
		t.Fatal(err)
	}
	tok := getToken(t, h)
	req := httptest.NewRequest("PUT", "/api/v1/buckets/downloads-x/policy?token="+tok, strings.NewReader(`{"Statement":[]}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("a query-string token changed a bucket policy: %d", rr.Code)
	}
	if p, _ := store.GetBucketPolicy("downloads-x"); len(p) > 0 {
		t.Fatal("the policy was written")
	}
}

// Console authorization ignored the user's AllowedCIDRs and evaluated with no
// condition context, so an Allow with a Condition never granted.
func TestConsoleHonoursSourceIPConditionsAndAllowedCIDRs(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("office"); err != nil {
		t.Fatal(err)
	}
	tok := sessionWithPolicy(t, h, store, "carol",
		`{"Statement":[{"Effect":"Allow","Action":"s3:ListBucket","Resource":"arn:aws:s3:::office",`+
			`"Condition":{"IpAddress":{"aws:SourceIp":["10.1.0.0/16"]}}}]}`)

	list := func(remote string) int {
		req := httptest.NewRequest("GET", "/api/v1/buckets/office/objects", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.RemoteAddr = remote
		// A spoofed header must not count as the source address.
		req.Header.Set("X-Forwarded-For", "10.1.2.3")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if code := list("10.1.2.3:5555"); code != http.StatusOK {
		t.Fatalf("an Allow whose IpAddress condition holds answered %d, want 200", code)
	}
	if code := list("203.0.113.9:5555"); code != http.StatusForbidden {
		t.Fatalf("an Allow whose condition fails answered %d, want 403", code)
	}

	// AllowedCIDRs on the user apply to every console request.
	u, _ := store.GetIAMUser("carol")
	u.AllowedCIDRs = []string{"10.1.2.0/24"}
	if err := store.UpdateIAMUser(*u); err != nil {
		t.Fatal(err)
	}
	if code := list("10.1.9.9:5555"); code != http.StatusForbidden {
		t.Fatalf("a request from outside the user's AllowedCIDRs answered %d, want 403", code)
	}
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.RemoteAddr = "10.1.9.9:5555"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "not in allowed range") {
		t.Fatalf("AllowedCIDRs did not apply to /auth/me: %d %s", rr.Code, rr.Body.String())
	}
	if code := list("10.1.2.3:5555"); code != http.StatusOK {
		t.Fatalf("a request from inside both ranges answered %d", code)
	}
}
