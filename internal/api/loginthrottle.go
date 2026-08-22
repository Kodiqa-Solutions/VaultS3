package api

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Login throttling (security assessment finding 4).
//
// The credential endpoints had no rate limit, no backoff and no lockout: 30
// wrong passwords in a row all returned 401 with no delay, and the correct one
// worked immediately afterwards. The general rate limiter is off in the shipped
// configuration and would not have helped much anyway, since its budget is
// sized for object traffic.
//
// This is deliberately separate and always on. It guards the two endpoints that
// hand out sessions, and nothing else, so it cannot be turned off by a config
// intended for a different purpose.

const (
	// loginFailureLimit is how many consecutive failures an address gets before
	// it is locked out.
	loginFailureLimit = 10
	// loginLockout is how long a locked-out address waits.
	loginLockout = 15 * time.Minute
	// loginFailureWindow is how long failures are remembered when no lockout has
	// been triggered, so an occasional typo never accumulates into one.
	loginFailureWindow = 15 * time.Minute
)

type loginAttempts struct {
	failures  int
	lastFail  time.Time
	lockedTil time.Time
}

// loginThrottle counts failed logins per client address.
type loginThrottle struct {
	mu   sync.Mutex
	byIP map[string]*loginAttempts
	now  func() time.Time
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{byIP: make(map[string]*loginAttempts), now: time.Now}
}

// blocked reports whether an address is currently locked out, and for how long.
func (t *loginThrottle) blocked(ip string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.byIP[ip]
	if a == nil {
		return false, 0
	}
	now := t.now()
	if now.Before(a.lockedTil) {
		return true, a.lockedTil.Sub(now)
	}
	// The window has passed with no lockout: forget the failures.
	if !a.lastFail.IsZero() && now.Sub(a.lastFail) > loginFailureWindow {
		delete(t.byIP, ip)
	}
	return false, 0
}

// fail records a failed attempt and locks the address out once it has used up
// its allowance.
func (t *loginThrottle) fail(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	a := t.byIP[ip]
	if a == nil {
		a = &loginAttempts{}
		t.byIP[ip] = a
	}
	if !a.lastFail.IsZero() && now.Sub(a.lastFail) > loginFailureWindow {
		a.failures = 0
	}
	a.failures++
	a.lastFail = now
	if a.failures >= loginFailureLimit {
		a.lockedTil = now.Add(loginLockout)
		a.failures = 0
	}
	// Keep the map from growing without bound on a scan across many addresses.
	if len(t.byIP) > 10000 {
		for k, v := range t.byIP {
			if now.After(v.lockedTil) && now.Sub(v.lastFail) > loginFailureWindow {
				delete(t.byIP, k)
			}
		}
	}
}

// succeed clears an address's history after a correct login.
func (t *loginThrottle) succeed(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byIP, ip)
}

// clientIPOf extracts the address a request came from. It uses RemoteAddr only:
// a forwarded header is supplied by the client, so trusting one would let an
// attacker rotate their apparent address and defeat the lockout entirely.
func clientIPOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
