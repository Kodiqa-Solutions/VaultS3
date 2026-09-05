package iam

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// webhook returns a stub authorizer plus a counter of how many times it was
// actually called, so a test can assert that a decision was cached or that the
// endpoint was never troubled at all.
func webhook(t *testing.T, allow bool) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req AuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(AuthResponse{Allow: allow})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newExt(t *testing.T, cfg ExternalAuthConfig) *ExternalAuth {
	t.Helper()
	e, err := NewExternalAuth(cfg)
	if err != nil {
		t.Fatalf("NewExternalAuth: %v", err)
	}
	return e
}

func TestExternalAuthAllowAndDeny(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow bool
	}{{"allow", true}, {"deny", false}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := webhook(t, tc.allow)
			e := newExt(t, ExternalAuthConfig{URL: srv.URL})
			got, err := e.Allow(AuthRequest{AccessKey: "k", Action: "s3:GetObject", Resource: "arn:aws:s3:::b/o"})
			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if got != tc.allow {
				t.Fatalf("Allow = %v, want %v", got, tc.allow)
			}
		})
	}
}

// A nil authorizer must behave exactly as the server did before this feature
// existed, since that is the configuration almost every deployment runs.
func TestExternalAuthNilAllows(t *testing.T) {
	var e *ExternalAuth
	ok, err := e.Allow(AuthRequest{Action: "s3:GetObject"})
	if err != nil || !ok {
		t.Fatalf("nil authorizer must allow, got (%v, %v)", ok, err)
	}
}

func TestExternalAuthFailClosedByDefault(t *testing.T) {
	// A URL that refuses connections: the server is created then immediately shut.
	srv, _ := webhook(t, true)
	url := srv.URL
	srv.Close()

	e := newExt(t, ExternalAuthConfig{URL: url, Timeout: 200 * time.Millisecond})
	ok, err := e.Allow(AuthRequest{Action: "s3:GetObject"})
	if err == nil {
		t.Fatal("want a transport error")
	}
	if ok {
		t.Fatal("an unreachable authorizer must DENY by default, it allowed")
	}
}

func TestExternalAuthFailOpen(t *testing.T) {
	srv, _ := webhook(t, true)
	url := srv.URL
	srv.Close()

	e := newExt(t, ExternalAuthConfig{URL: url, Timeout: 200 * time.Millisecond, FailOpen: true})
	ok, err := e.Allow(AuthRequest{Action: "s3:GetObject"})
	if err == nil {
		t.Fatal("want the transport error to still be reported")
	}
	if !ok {
		t.Fatal("fail_open must allow when the endpoint is unreachable")
	}
}

func TestExternalAuthNon200Denies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	e := newExt(t, ExternalAuthConfig{URL: srv.URL})
	if ok, err := e.Allow(AuthRequest{Action: "s3:GetObject"}); ok || err == nil {
		t.Fatalf("a 500 must deny, got (%v, %v)", ok, err)
	}
}

func TestExternalAuthGarbageResponseDenies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "this is not json")
	}))
	defer srv.Close()
	e := newExt(t, ExternalAuthConfig{URL: srv.URL})
	if ok, err := e.Allow(AuthRequest{Action: "s3:GetObject"}); ok || err == nil {
		t.Fatalf("an unparseable body must deny, got (%v, %v)", ok, err)
	}
}

func TestExternalAuthCachesDecision(t *testing.T) {
	srv, calls := webhook(t, true)
	e := newExt(t, ExternalAuthConfig{URL: srv.URL, CacheTTL: time.Minute})
	req := AuthRequest{AccessKey: "k", Action: "s3:GetObject", Resource: "arn:aws:s3:::b/o"}
	for i := 0; i < 5; i++ {
		if ok, err := e.Allow(req); !ok || err != nil {
			t.Fatalf("call %d: (%v, %v)", i, ok, err)
		}
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("5 identical requests made %d webhook calls, want 1", got)
	}
}

// The cache key must cover every field sent to the webhook. A decision made for
// one object must never be reused for another.
func TestExternalAuthCacheKeyCoversRequest(t *testing.T) {
	srv, calls := webhook(t, true)
	e := newExt(t, ExternalAuthConfig{URL: srv.URL, CacheTTL: time.Minute})
	base := AuthRequest{AccessKey: "k", User: "u", Action: "s3:GetObject", Resource: "arn:aws:s3:::b/o", SourceIP: "10.0.0.1"}
	vary := []AuthRequest{
		base,
		func() AuthRequest { r := base; r.AccessKey = "k2"; return r }(),
		func() AuthRequest { r := base; r.User = "u2"; return r }(),
		func() AuthRequest { r := base; r.Action = "s3:PutObject"; return r }(),
		func() AuthRequest { r := base; r.Resource = "arn:aws:s3:::b/other"; return r }(),
		func() AuthRequest { r := base; r.SourceIP = "10.0.0.2"; return r }(),
	}
	for _, r := range vary {
		if _, err := e.Allow(r); err != nil {
			t.Fatalf("Allow: %v", err)
		}
	}
	if got := atomic.LoadInt32(calls); int(got) != len(vary) {
		t.Fatalf("%d distinct requests made %d webhook calls, want %d", len(vary), got, len(vary))
	}
}

func TestExternalAuthCacheExpires(t *testing.T) {
	srv, calls := webhook(t, true)
	e := newExt(t, ExternalAuthConfig{URL: srv.URL, CacheTTL: 30 * time.Millisecond})
	req := AuthRequest{Action: "s3:GetObject", Resource: "arn:aws:s3:::b/o"}
	e.Allow(req)
	time.Sleep(60 * time.Millisecond)
	e.Allow(req)
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("expired entry reused: %d calls, want 2", got)
	}
}

// A failure must not be cached. Caching it would turn one bad moment at the
// endpoint into a guaranteed TTL of refusals after it had already recovered.
func TestExternalAuthDoesNotCacheFailures(t *testing.T) {
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(AuthResponse{Allow: true})
	}))
	defer srv.Close()

	e := newExt(t, ExternalAuthConfig{URL: srv.URL, CacheTTL: time.Hour})
	if ok, _ := e.Allow(AuthRequest{Action: "s3:GetObject"}); ok {
		t.Fatal("want a denial while the endpoint is down")
	}
	up.Store(true)
	if ok, err := e.Allow(AuthRequest{Action: "s3:GetObject"}); !ok || err != nil {
		t.Fatalf("recovered endpoint still refused: (%v, %v)", ok, err)
	}
}

func TestNewExternalAuthRejectsBadURL(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"empty", ""},
		{"no scheme", "auth.internal/authz"},
		{"wrong scheme", "ftp://auth.internal/authz"},
		{"file scheme", "file:///etc/passwd"},
		{"no host", "http:///authz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewExternalAuth(ExternalAuthConfig{URL: tc.url}); err == nil {
				t.Fatalf("accepted %q", tc.url)
			}
		})
	}
}

// A private address must be accepted. This is the case that reusing the bucket
// notification SSRF validator would have broken, and it is where a real
// authorization service actually lives.
func TestNewExternalAuthAcceptsPrivateAddresses(t *testing.T) {
	for _, u := range []string{
		"http://10.0.0.5:8080/authz",
		"http://192.168.1.10/authz",
		"http://172.16.0.9/authz",
		"http://auth.internal/authz",
		"https://authz.example.com/v1/decide",
	} {
		if _, err := NewExternalAuth(ExternalAuthConfig{URL: u}); err != nil {
			t.Fatalf("rejected %q: %v", u, err)
		}
	}
}

func TestExternalAuthSendsBearerToken(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(AuthResponse{Allow: true})
	}))
	defer srv.Close()
	e := newExt(t, ExternalAuthConfig{URL: srv.URL, Token: "s3cr3t"})
	e.Allow(AuthRequest{Action: "s3:GetObject"})
	if h := <-got; h != "Bearer s3cr3t" {
		t.Fatalf("Authorization = %q", h)
	}
}

func TestExternalAuthSendsFullRequest(t *testing.T) {
	got := make(chan AuthRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req AuthRequest
		json.NewDecoder(r.Body).Decode(&req)
		got <- req
		json.NewEncoder(w).Encode(AuthResponse{Allow: true})
	}))
	defer srv.Close()
	e := newExt(t, ExternalAuthConfig{URL: srv.URL})
	want := AuthRequest{AccessKey: "AKIA", User: "bob", Action: "s3:GetObject", Resource: "arn:aws:s3:::b/o", SourceIP: "10.1.2.3"}
	e.Allow(want)
	if g := <-got; g != want {
		t.Fatalf("webhook received %+v, want %+v", g, want)
	}
}

// EvaluateDetailed must separate "a policy said Deny" from "nothing said Allow".
// Authoritative mode leans on that distinction to keep an explicit Deny final.
func TestEvaluateDetailedDistinguishesDeny(t *testing.T) {
	allowPol := Policy{Statement: []Statement{{Effect: "Allow", Action: stringOrSlice{"s3:GetObject"}, Resource: stringOrSlice{"arn:aws:s3:::b/*"}}}}
	denyPol := Policy{Statement: []Statement{{Effect: "Deny", Action: stringOrSlice{"s3:GetObject"}, Resource: stringOrSlice{"arn:aws:s3:::b/secret"}}}}

	if allowed, explicit := EvaluateDetailed([]Policy{allowPol}, "s3:GetObject", "arn:aws:s3:::b/o", nil); !allowed || explicit {
		t.Fatalf("allow case: (%v, %v)", allowed, explicit)
	}
	if allowed, explicit := EvaluateDetailed([]Policy{allowPol}, "s3:PutObject", "arn:aws:s3:::b/o", nil); allowed || explicit {
		t.Fatalf("no-match case must be a silent deny: (%v, %v)", allowed, explicit)
	}
	if allowed, explicit := EvaluateDetailed([]Policy{allowPol, denyPol}, "s3:GetObject", "arn:aws:s3:::b/secret", nil); allowed || !explicit {
		t.Fatalf("explicit deny case: (%v, %v)", allowed, explicit)
	}
}

func TestSourceIPOf(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.1:53142":   "10.0.0.1",
		"192.168.1.5:9000": "192.168.1.5",
		"[::1]:9000":       "::1",
		"10.0.0.1":         "10.0.0.1",
	} {
		if got := SourceIPOf(in); got != want {
			t.Fatalf("SourceIPOf(%q) = %q, want %q", in, got, want)
		}
	}
}
