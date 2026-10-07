package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// The email claim becomes the IAM identity, and email_verified was never read,
// so on a provider that allows unverified addresses anyone could sign in as
// victim@corp.com.
func TestOIDCSessionNeedsAVerifiedEmail(t *testing.T) {
	h, store := newTestAPI(t)
	h.cfg.OIDC.AutoCreateUsers = true
	if err := store.CreateIAMUser(metadata.IAMUser{Name: "victim@corp.com", PolicyARNs: []string{"admin-ish"}}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		verified interface{}
		want     int
		reason   string
	}{
		{"absent", nil, http.StatusForbidden, "no email_verified claim"},
		{"false", false, http.StatusForbidden, "not verified"},
		{"string false", "false", http.StatusForbidden, "not verified"},
		{"true", true, http.StatusOK, ""},
		{"string true", "true", http.StatusOK, ""},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		h.issueOIDCSession(rr, &OIDCClaims{Sub: "attacker", Email: "victim@corp.com", EmailVerified: c.verified})
		if rr.Code != c.want || !strings.Contains(rr.Body.String(), c.reason) {
			t.Errorf("email_verified %s: %d %s, want %d %q", c.name, rr.Code, rr.Body.String(), c.want, c.reason)
		}
	}
	// With no email the subject is the identity, and needs no verification.
	rr := httptest.NewRecorder()
	h.issueOIDCSession(rr, &OIDCClaims{Sub: "svc-123"})
	if rr.Code != http.StatusOK {
		t.Errorf("subject-only login: %d %s", rr.Code, rr.Body.String())
	}
}

// Role mapping applied only when the user was first created, so a user added to
// a mapped group later never got its policy.
func TestOIDCRoleMappingAppliesOnEveryLogin(t *testing.T) {
	h, store := newTestAPI(t)
	h.cfg.OIDC.AutoCreateUsers = true
	h.cfg.OIDC.RoleMapping = map[string]string{"readers": "ReadOnly", "writers": "ReadWrite"}

	login := func(groups ...string) {
		t.Helper()
		rr := httptest.NewRecorder()
		h.issueOIDCSession(rr, &OIDCClaims{Sub: "s", Email: "gus@corp.com", EmailVerified: true, Groups: groups})
		if rr.Code != http.StatusOK {
			t.Fatalf("login: %d %s", rr.Code, rr.Body.String())
		}
	}
	login("readers")
	u, _ := store.GetIAMUser("gus@corp.com")
	if len(u.PolicyARNs) != 1 || u.PolicyARNs[0] != "ReadOnly" {
		t.Fatalf("after the first login: %v", u.PolicyARNs)
	}
	// An admin attaches a policy by hand, then the user joins writers.
	u.PolicyARNs = append(u.PolicyARNs, "Manual")
	store.UpdateIAMUser(*u)
	login("readers", "writers")
	u, _ = store.GetIAMUser("gus@corp.com")
	want := map[string]bool{"ReadOnly": true, "Manual": true, "ReadWrite": true}
	if len(u.PolicyARNs) != 3 {
		t.Fatalf("after joining writers: %v, want ReadOnly, Manual, ReadWrite", u.PolicyARNs)
	}
	for _, p := range u.PolicyARNs {
		if !want[p] {
			t.Fatalf("unexpected policy %s in %v", p, u.PolicyARNs)
		}
	}
}

// Some providers never send email_verified. The opt-in lets their logins
// through when the claim is absent, and never when it says false.
func TestOIDCAcceptEmailWithoutVerifiedClaim(t *testing.T) {
	h, _ := newTestAPI(t)
	h.cfg.OIDC.AutoCreateUsers = true
	h.cfg.OIDC.AcceptEmailWithoutVerifiedClaim = true
	rr := httptest.NewRecorder()
	h.issueOIDCSession(rr, &OIDCClaims{Sub: "s1", Email: "ann@corp.com"})
	if rr.Code != http.StatusOK {
		t.Errorf("absent claim with the opt-in: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.issueOIDCSession(rr, &OIDCClaims{Sub: "s2", Email: "bob@corp.com", EmailVerified: false})
	if rr.Code != http.StatusForbidden {
		t.Errorf("a claim saying false was accepted with the opt-in: %d", rr.Code)
	}
}
