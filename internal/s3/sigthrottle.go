package s3

import (
	"net"
	"sync"
	"time"
)

// sigThrottle slows down repeated failed signatures for one access key from
// one client address.
//
// The dashboard locks out an address after a few wrong logins, but the admin's
// key and secret also sign S3 requests, where a wrong guess cost nothing.
// After sigFailureLimit failures in a row for one key from one address, each
// further failure for that pair is answered only after sigFailureDelay.
//
// It never refuses a correct signature. An earlier version locked the pair
// out, the right signature included, so a guess could not be confirmed; but
// access keys are not secret (they are in every presigned URL), and clients
// often share one address (behind NAT, a proxy, or a cluster node that
// forwards requests before they are authenticated), so anyone could lock a
// key out for everyone. A secret VaultS3 generates is 192 bits and cannot be
// guessed; this slows the guessing of a weak one that an operator chose.
type sigThrottle struct {
	mu      sync.Mutex
	entries map[sigKey]*sigEntry
}

type sigKey struct{ ip, key string }

type sigEntry struct {
	failures int
	lastFail time.Time
}

// sigFailureLimit and sigFailureDelay are variables so a test can change
// them.
var (
	sigFailureLimit = 20
	sigFailureDelay = time.Second
	sigNow          = time.Now
)

const (
	sigFailureWindow = 15 * time.Minute
	sigMaxEntries    = 100_000
)

// clientBucket is the address a failure is counted against: an IPv4 address,
// or the /64 an IPv6 address is in, since one host has a whole /64.
func clientBucket(ip string) string {
	p := net.ParseIP(ip)
	if p == nil || p.To4() != nil {
		return ip
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// fail records a failed signature and returns how long to wait before
// answering it.
func (t *sigThrottle) fail(ip, key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = map[sigKey]*sigEntry{}
	}
	now := sigNow()
	k := sigKey{clientBucket(ip), key}
	e := t.entries[k]
	if e == nil {
		if len(t.entries) >= sigMaxEntries {
			t.evict(now)
		}
		e = &sigEntry{}
		t.entries[k] = e
	} else if now.Sub(e.lastFail) > sigFailureWindow {
		e.failures = 0
	}
	e.failures++
	e.lastFail = now
	if e.failures > sigFailureLimit {
		return sigFailureDelay
	}
	return 0
}

// evict makes room: entries past their window go, and if none has, the one
// that failed longest ago. It used to stop counting when the table was full,
// so filling it (one IPv6 /64 is enough addresses) switched counting off.
func (t *sigThrottle) evict(now time.Time) {
	var oldest sigKey
	var oldestAt time.Time
	removed := false
	for k, e := range t.entries {
		if now.Sub(e.lastFail) > sigFailureWindow {
			delete(t.entries, k)
			removed = true
			continue
		}
		if oldestAt.IsZero() || e.lastFail.Before(oldestAt) {
			oldest, oldestAt = k, e.lastFail
		}
	}
	if !removed && !oldestAt.IsZero() {
		delete(t.entries, oldest)
	}
}

func (t *sigThrottle) reset(ip, key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, sigKey{clientBucket(ip), key})
}
