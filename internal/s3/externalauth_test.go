package s3

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
)

// These cover the integration semantics rather than the HTTP client: what the
// combination of an IAM decision and a webhook decision resolves to. Issue #52.

func stubWebhook(t *testing.T, allow bool) (string, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		json.NewEncoder(w).Encode(iam.AuthResponse{Allow: allow})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &calls
}

func authWith(t *testing.T, url string, authoritative bool) *Authenticator {
	t.Helper()
	a := NewAuthenticator("admin", "secret", nil, nil, nil)
	if url != "" {
		e, err := iam.NewExternalAuth(iam.ExternalAuthConfig{
			URL: url, Authoritative: authoritative, Timeout: time.Second,
		})
		if err != nil {
			t.Fatalf("NewExternalAuth: %v", err)
		}
		a.SetExternalAuth(e)
	}
	return a
}

func readerPolicy() iam.Policy {
	var p iam.Policy
	json.Unmarshal([]byte(`{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`), &p)
	return p
}

func denySecretPolicy() iam.Policy {
	var p iam.Policy
	json.Unmarshal([]byte(`{"Statement":[{"Effect":"Deny","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/secret"}]}`), &p)
	return p
}

// Deny-only is the default: IAM must allow AND the webhook must allow.
func TestExternalDenyOnlyNarrowsIAM(t *testing.T) {
	url, calls := stubWebhook(t, false)
	a := authWith(t, url, false)
	id := &iam.Identity{UserID: "bob", AccessKey: "AKIA", Policies: []iam.Policy{readerPolicy()}}

	if err := a.AuthorizeWithContext(id, "s3:GetObject", "arn:aws:s3:::b/o", nil); err == nil {
		t.Fatal("webhook denied but access was granted")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("webhook calls = %d, want 1", got)
	}
}

func TestExternalDenyOnlyAllowsWhenBothAgree(t *testing.T) {
	url, _ := stubWebhook(t, true)
	a := authWith(t, url, false)
	id := &iam.Identity{UserID: "bob", Policies: []iam.Policy{readerPolicy()}}

	if err := a.AuthorizeWithContext(id, "s3:GetObject", "arn:aws:s3:::b/o", nil); err != nil {
		t.Fatalf("both allowed but access was refused: %v", err)
	}
}

// In deny-only mode a request IAM already refused must not reach the endpoint.
// Otherwise every unauthorized probe becomes traffic on someone's auth service.
func TestExternalDenyOnlySkipsWebhookWhenIAMDenies(t *testing.T) {
	url, calls := stubWebhook(t, true)
	a := authWith(t, url, false)
	id := &iam.Identity{UserID: "bob", Policies: []iam.Policy{readerPolicy()}}

	if err := a.AuthorizeWithContext(id, "s3:DeleteObject", "arn:aws:s3:::b/o", nil); err == nil {
		t.Fatal("IAM had no allow for DeleteObject, want denied")
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("webhook was called %d times for a request IAM already refused, want 0", got)
	}
}

// Authoritative mode lets the webhook grant what IAM alone would not.
func TestExternalAuthoritativeGrants(t *testing.T) {
	url, _ := stubWebhook(t, true)
	a := authWith(t, url, true)
	id := &iam.Identity{UserID: "bob", Policies: []iam.Policy{readerPolicy()}}

	if err := a.AuthorizeWithContext(id, "s3:DeleteObject", "arn:aws:s3:::b/o", nil); err != nil {
		t.Fatalf("authoritative webhook allowed but access was refused: %v", err)
	}
}

// The guard that matters most: an explicit Deny an operator wrote is final, and
// turning authoritative mode on must not quietly reopen it.
func TestExternalAuthoritativeCannotOverrideExplicitDeny(t *testing.T) {
	url, calls := stubWebhook(t, true)
	a := authWith(t, url, true)
	id := &iam.Identity{UserID: "bob", Policies: []iam.Policy{readerPolicy(), denySecretPolicy()}}

	if err := a.AuthorizeWithContext(id, "s3:GetObject", "arn:aws:s3:::b/secret", nil); err == nil {
		t.Fatal("an authoritative webhook overrode an explicit IAM Deny")
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("an explicit Deny was put to the webhook (%d calls), it must never be", got)
	}
}

// Admin is never put to the webhook, on purpose: it is the break-glass path, so
// a broken endpoint cannot lock the operator out of their own server.
func TestExternalNeverAppliesToAdmin(t *testing.T) {
	url, calls := stubWebhook(t, false)
	a := authWith(t, url, true)

	if err := a.AuthorizeWithContext(&iam.Identity{IsAdmin: true}, "s3:DeleteBucket", "arn:aws:s3:::b", nil); err != nil {
		t.Fatalf("admin was refused: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("admin was put to the webhook (%d calls), want 0", got)
	}
}

// An unreachable endpoint denies, and the refusal says which authority refused.
func TestExternalUnreachableDenies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	a := authWith(t, url, false)
	id := &iam.Identity{UserID: "bob", Policies: []iam.Policy{readerPolicy()}}
	err := a.AuthorizeWithContext(id, "s3:GetObject", "arn:aws:s3:::b/o", nil)
	if err == nil {
		t.Fatal("an unreachable authorizer allowed the request")
	}
	var denied *iam.DeniedError
	if !asDenied(err, &denied) {
		t.Fatalf("want a DeniedError, got %T: %v", err, err)
	}
}

func asDenied(err error, target **iam.DeniedError) bool {
	d, ok := err.(*iam.DeniedError)
	if ok {
		*target = d
	}
	return ok
}

// With no webhook configured, authorization must behave exactly as before.
func TestExternalUnsetLeavesIAMUnchanged(t *testing.T) {
	a := authWith(t, "", false)
	id := &iam.Identity{UserID: "bob", Policies: []iam.Policy{readerPolicy()}}

	if err := a.AuthorizeWithContext(id, "s3:GetObject", "arn:aws:s3:::b/o", nil); err != nil {
		t.Fatalf("IAM allow was refused with no webhook: %v", err)
	}
	if err := a.AuthorizeWithContext(id, "s3:DeleteObject", "arn:aws:s3:::b/o", nil); err == nil {
		t.Fatal("IAM deny was allowed with no webhook")
	}
}

// The webhook receives the identity and the source IP, not just the action.
func TestExternalReceivesIdentityAndSourceIP(t *testing.T) {
	got := make(chan iam.AuthRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req iam.AuthRequest
		json.NewDecoder(r.Body).Decode(&req)
		got <- req
		json.NewEncoder(w).Encode(iam.AuthResponse{Allow: true})
	}))
	defer srv.Close()

	a := authWith(t, srv.URL, false)
	id := &iam.Identity{UserID: "bob", AccessKey: "AKIABOB", Policies: []iam.Policy{readerPolicy()}}
	ctx := map[string]string{"aws:SourceIp": "10.9.8.7"}
	if err := a.AuthorizeWithContext(id, "s3:GetObject", "arn:aws:s3:::b/o", ctx); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	req := <-got
	if req.AccessKey != "AKIABOB" || req.User != "bob" || req.SourceIP != "10.9.8.7" {
		t.Fatalf("webhook received %+v", req)
	}
	if req.Action != "s3:GetObject" || req.Resource != "arn:aws:s3:::b/o" {
		t.Fatalf("webhook received %+v", req)
	}
}

// fail_open must actually reach the request. The first version of this wiring
// checked the transport error before the decision, so a fail-open deployment
// denied anyway and the option did nothing.
func TestExternalFailOpenAllowsThroughAuthenticator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	a := NewAuthenticator("admin", "secret", nil, nil, nil)
	e, err := iam.NewExternalAuth(iam.ExternalAuthConfig{
		URL: url, FailOpen: true, Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewExternalAuth: %v", err)
	}
	a.SetExternalAuth(e)

	id := &iam.Identity{UserID: "bob", Policies: []iam.Policy{readerPolicy()}}
	if err := a.AuthorizeWithContext(id, "s3:GetObject", "arn:aws:s3:::b/o", nil); err != nil {
		t.Fatalf("fail_open was configured but the request was refused: %v", err)
	}
}

// fail_open must not become fail-anything: a webhook that answers and says no is
// still a denial, however unreachable a different webhook might have been.
func TestExternalFailOpenStillHonoursAnExplicitNo(t *testing.T) {
	url, _ := stubWebhook(t, false)
	a := NewAuthenticator("admin", "secret", nil, nil, nil)
	e, err := iam.NewExternalAuth(iam.ExternalAuthConfig{URL: url, FailOpen: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewExternalAuth: %v", err)
	}
	a.SetExternalAuth(e)

	id := &iam.Identity{UserID: "bob", Policies: []iam.Policy{readerPolicy()}}
	if err := a.AuthorizeWithContext(id, "s3:GetObject", "arn:aws:s3:::b/o", nil); err == nil {
		t.Fatal("fail_open let an explicit webhook denial through")
	}
}
