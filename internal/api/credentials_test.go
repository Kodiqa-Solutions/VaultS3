package api

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// A failed write of the new admin credentials used to be ignored and reported
// as success, so the old password came back on restart.
func TestChangeCredentialsFailsWhenItCannotPersist(t *testing.T) {
	h, store := newTestAPI(t)
	tok := getToken(t, h)
	h.store = &faultyStore{StoreAPI: store, failSetAdminCreds: true}
	rr := doRequest(h, "PUT", "/settings/credentials", changeCredentialsRequest{
		CurrentSecretKey: "secret", NewAccessKey: "root2", NewSecretKey: "a-much-longer-secret",
	}, tok)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "could not save") {
		t.Fatalf("got %d %s, want 500 could not save", rr.Code, rr.Body.String())
	}
	// Nothing changed: the old pair still logs in and the old session still works.
	if ak, sk := h.adminCredentials(); ak != "admin" || sk != "secret" {
		t.Fatalf("in-memory credentials changed to %s/%s after a failed save", ak, sk)
	}
	if rr := doRequest(h, "GET", "/auth/me", nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("the session was rotated away after a failed save: %d", rr.Code)
	}
}

// Logins and authenticated requests read the credentials and the signer while
// a change replaces them. Run under -race.
func TestChangeCredentialsIsRaceFreeWithLogins(t *testing.T) {
	h, _ := newTestAPI(t)
	tok := getToken(t, h)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				doRequest(h, "POST", "/auth/login", loginRequest{AccessKey: "admin", SecretKey: "secret"}, "")
				doRequest(h, "GET", "/auth/me", nil, tok)
				doRequest(h, "GET", "/keys", nil, tok)
			}
		}()
	}
	cur := "secret"
	for i := 0; i < 20; i++ {
		next := "rotated-secret-" + strings.Repeat("x", i)
		rr := doRequest(h, "PUT", "/settings/credentials", changeCredentialsRequest{
			CurrentSecretKey: cur, NewAccessKey: "admin", NewSecretKey: next,
		}, adminSession(t, h))
		if rr.Code != http.StatusOK {
			t.Fatalf("change %d: %d %s", i, rr.Code, rr.Body.String())
		}
		cur = next
	}
	close(stop)
	wg.Wait()
	if _, sk := h.adminCredentials(); sk != cur {
		t.Fatalf("final secret %q, want %q", sk, cur)
	}
}

// adminSession mints an admin session with whatever signer is current, since
// each credential change rotates it.
func adminSession(t *testing.T, h *APIHandler) string {
	t.Helper()
	tok, err := h.jwtService().Generate("admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestMaskSecretRevealsAtMostFour(t *testing.T) {
	for _, s := range []string{"", "short", "exactly8", "fifteen-chars!!"} {
		if got := maskSecret(s); got != "****" {
			t.Errorf("maskSecret(%q) = %q, want **** for a secret under 16", s, got)
		}
	}
	got := maskSecret("abcdefghijklmnopqrstuvwxyz")
	if got != "ab****yz" {
		t.Errorf("maskSecret(26 chars) = %q, want ab****yz", got)
	}
	shown := strings.ReplaceAll(got, "*", "")
	if len(shown) > 4 {
		t.Errorf("%d characters revealed, want at most 4", len(shown))
	}
}
