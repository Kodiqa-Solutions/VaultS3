package s3

import (
	"net/http"
	"testing"
	"time"
)

// Repeated failed signatures for one key from one address are answered
// slowly, so a weak secret cannot be guessed at thousands of tries a second.
// A correct signature is never refused or delayed: access keys are public and
// clients share addresses, so refusing it let anyone lock a key out for
// everyone (5.0.14 review of the first version).
func TestFailedSignaturesAreSlowedAndTheRightOneAlwaysPasses(t *testing.T) {
	prevLimit, prevDelay := sigFailureLimit, sigFailureDelay
	sigFailureLimit, sigFailureDelay = 5, 300*time.Millisecond
	defer func() { sigFailureLimit, sigFailureDelay = prevLimit, prevDelay }()
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/sigb", "").must(t, "bucket")
	for i := 0; i < sigFailureLimit; i++ {
		e.as(t, testAccessKey, "wrong-secret", http.MethodGet, "/sigb", "")
	}
	start := time.Now()
	if r := e.as(t, testAccessKey, "wrong-secret", http.MethodGet, "/sigb", ""); r.code != http.StatusForbidden {
		t.Fatalf("a wrong signature past the limit: %d", r.code)
	}
	if took := time.Since(start); took < sigFailureDelay {
		t.Errorf("a failure past the limit was answered in %v, want at least %v", took, sigFailureDelay)
	}
	start = time.Now()
	e.admin(t, http.MethodGet, "/sigb", "").must(t, "the right secret after the failures")
	if took := time.Since(start); took > sigFailureDelay {
		t.Errorf("the right signature was delayed %v", took)
	}
	// It cleared the count: the next failure is answered at once.
	start = time.Now()
	e.as(t, testAccessKey, "wrong-secret", http.MethodGet, "/sigb", "")
	if took := time.Since(start); took > sigFailureDelay {
		t.Errorf("a failure after a success was delayed %v", took)
	}
}

// A full table makes room instead of stopping to count.
func TestAFullThrottleTableKeepsCounting(t *testing.T) {
	var th sigThrottle
	th.entries = map[sigKey]*sigEntry{}
	now := time.Now()
	for i := 0; i < sigMaxEntries; i++ {
		th.entries[sigKey{ip: "10.0.0.1", key: string(rune('a'+i%26)) + time.Duration(i).String()}] = &sigEntry{failures: 1, lastFail: now}
	}
	prev := sigFailureLimit
	sigFailureLimit = 2
	defer func() { sigFailureLimit = prev }()
	th.fail("203.0.113.9", "admin")
	th.fail("203.0.113.9", "admin")
	if d := th.fail("203.0.113.9", "admin"); d == 0 {
		t.Fatal("a full table stopped counting")
	}
	if len(th.entries) > sigMaxEntries {
		t.Errorf("table grew to %d", len(th.entries))
	}
}

func TestIPv6IsCountedPerSlash64(t *testing.T) {
	if clientBucket("2001:db8::1") != clientBucket("2001:db8::ffff") {
		t.Error("two addresses of one /64 counted apart")
	}
	if clientBucket("203.0.113.9") != "203.0.113.9" {
		t.Error("an IPv4 address changed")
	}
}
