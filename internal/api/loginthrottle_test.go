package api

import (
	"testing"
	"time"
)

// The login endpoint guards the admin credential pair and had no limit at all:
// 30 wrong passwords in a row all returned 401 with no delay, and the correct
// one worked immediately after (security assessment finding 4).
func TestLoginThrottleLocksOutAfterRepeatedFailures(t *testing.T) {
	now := time.Unix(1000, 0)
	th := newLoginThrottle()
	th.now = func() time.Time { return now }

	for i := 0; i < loginFailureLimit-1; i++ {
		th.fail("10.0.0.1")
		if blocked, _ := th.blocked("10.0.0.1"); blocked {
			t.Fatalf("locked out after %d failures, the allowance is %d", i+1, loginFailureLimit)
		}
	}
	th.fail("10.0.0.1")
	blocked, retry := th.blocked("10.0.0.1")
	if !blocked {
		t.Fatalf("%d consecutive failures did not lock the address out", loginFailureLimit)
	}
	if retry <= 0 {
		t.Fatal("lockout reports no retry-after")
	}

	// One address's lockout must not affect another.
	if blocked, _ := th.blocked("10.0.0.2"); blocked {
		t.Fatal("a different address was locked out too")
	}

	// The lockout expires.
	now = now.Add(loginLockout + time.Second)
	if blocked, _ := th.blocked("10.0.0.1"); blocked {
		t.Fatal("the lockout never expires")
	}
}

// A correct login clears the history, so an occasional typo does not accumulate
// towards a lockout for a legitimate user.
func TestSuccessfulLoginClearsFailures(t *testing.T) {
	now := time.Unix(2000, 0)
	th := newLoginThrottle()
	th.now = func() time.Time { return now }

	for i := 0; i < loginFailureLimit-1; i++ {
		th.fail("10.0.0.3")
	}
	th.succeed("10.0.0.3")
	for i := 0; i < loginFailureLimit-1; i++ {
		th.fail("10.0.0.3")
		if blocked, _ := th.blocked("10.0.0.3"); blocked {
			t.Fatal("failures from before a successful login still counted")
		}
	}
}

// Scattered failures over a long period must not add up to a lockout.
func TestOldFailuresAreForgotten(t *testing.T) {
	now := time.Unix(3000, 0)
	th := newLoginThrottle()
	th.now = func() time.Time { return now }

	for i := 0; i < loginFailureLimit*2; i++ {
		th.fail("10.0.0.4")
		if blocked, _ := th.blocked("10.0.0.4"); blocked {
			t.Fatal("spaced-out failures accumulated into a lockout")
		}
		now = now.Add(loginFailureWindow + time.Minute)
		th.blocked("10.0.0.4") // the read is what expires the window
	}
}
