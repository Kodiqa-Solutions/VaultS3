package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/iam"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	s3auth "github.com/Kodiqa-Solutions/VaultS3/internal/s3"
)

// The dashboard authorizes on its own path, not through the S3 authenticator, so
// the external authorizer has to be wired there separately. Same semantics, a
// second place to get them wrong (issue #52).

func consoleWebhook(t *testing.T, allow bool) (string, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		json.NewEncoder(w).Encode(iam.AuthResponse{Allow: allow})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &calls
}

// consoleUser sets up a non-admin session holding a policy that allows reading
// the bucket, so the only thing left to decide is what the webhook says.
func consoleUser(t *testing.T, h *APIHandler, store *metadata.Store) string {
	t.Helper()
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::b"]}]}`
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{Name: "ro", Document: doc}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateIAMUser(metadata.IAMUser{Name: "bob", PolicyARNs: []string{"ro"}}); err != nil {
		t.Fatal(err)
	}
	tok, err := h.jwt.Generate("bob", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func attachExternal(t *testing.T, h *APIHandler, url string, authoritative bool) {
	t.Helper()
	a := s3auth.NewAuthenticator("admin", "secret", nil, nil, nil)
	e, err := iam.NewExternalAuth(iam.ExternalAuthConfig{URL: url, Authoritative: authoritative, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	a.SetExternalAuth(e)
	h.SetS3Authenticator(a)
}

// Baseline: without the webhook this session can list the bucket. If this ever
// stops being true the denial tests below prove nothing.
func TestConsoleExternalBaselineAllows(t *testing.T) {
	h, store := newTestAPI(t)
	tok := consoleUser(t, h, store)
	if rr := doRequest(h, "GET", "/buckets/b/objects", nil, tok); rr.Code == http.StatusForbidden {
		t.Fatalf("the IAM policy alone did not permit the listing (%d)", rr.Code)
	}
}

func TestConsoleExternalDenialBlocksTheDashboard(t *testing.T) {
	h, store := newTestAPI(t)
	tok := consoleUser(t, h, store)
	url, calls := consoleWebhook(t, false)
	attachExternal(t, h, url, false)

	if rr := doRequest(h, "GET", "/buckets/b/objects", nil, tok); rr.Code != http.StatusForbidden {
		t.Fatalf("the webhook denied but the dashboard served it (%d)", rr.Code)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("webhook calls = %d, want 1", got)
	}
}

func TestConsoleExternalAllowLetsItThrough(t *testing.T) {
	h, store := newTestAPI(t)
	tok := consoleUser(t, h, store)
	url, _ := consoleWebhook(t, true)
	attachExternal(t, h, url, false)

	if rr := doRequest(h, "GET", "/buckets/b/objects", nil, tok); rr.Code == http.StatusForbidden {
		t.Fatalf("IAM and the webhook both allowed but the dashboard refused (%d)", rr.Code)
	}
}

// Admin is exempt on the console path too, for the same break-glass reason.
func TestConsoleExternalNeverAppliesToAdmin(t *testing.T) {
	h, store := newTestAPI(t)
	consoleUser(t, h, store)
	url, calls := consoleWebhook(t, false)
	attachExternal(t, h, url, true)

	admin, err := h.jwt.Generate("admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rr := doRequest(h, "GET", "/buckets/b/objects", nil, admin); rr.Code == http.StatusForbidden {
		t.Fatalf("admin was refused by the console webhook (%d)", rr.Code)
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("admin was put to the webhook (%d calls), want 0", got)
	}
}

// An unreachable endpoint must deny here too, not fall through to the IAM answer.
func TestConsoleExternalUnreachableDenies(t *testing.T) {
	h, store := newTestAPI(t)
	tok := consoleUser(t, h, store)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	attachExternal(t, h, url, false)

	if rr := doRequest(h, "GET", "/buckets/b/objects", nil, tok); rr.Code != http.StatusForbidden {
		t.Fatalf("an unreachable authorizer let the dashboard through (%d)", rr.Code)
	}
}
