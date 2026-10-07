package ratelimit

import "testing"

// A key's allowance is charged only through AllowKey, so requests from an IP
// that never authenticated cannot drain it.
func TestAllowKeyChargesOnlyTheKey(t *testing.T) {
	l := NewLimiter(1000, 1000, 1, 2)
	defer l.Stop()
	for i := 0; i < 50; i++ {
		l.Allow("203.0.113.9", "")
	}
	if !l.AllowKey("AKVICTIM") {
		t.Fatal("unauthenticated traffic drained the key's allowance")
	}
	l.AllowKey("AKVICTIM")
	if l.AllowKey("AKVICTIM") {
		t.Error("the key's own limit was not enforced")
	}
}
