package s3

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// VaultS3 runs behind nginx, so RemoteAddr inside the container is the proxy
// (the Docker bridge), the same value for every visitor on earth. The audit
// trail is only useful if it records the forwarded client address instead.
// Without this, every entry on a proxied deployment reads 172.17.0.1 and the
// security log cannot answer where a denied request came from.
func TestAuditRecordsTheForwardedClientIP(t *testing.T) {
	h := newTestHandler(t)

	var gotIP, gotEffect string
	h.SetAuditFunc(func(_, _, _, _, effect, sourceIP string, _ int) {
		gotIP, gotEffect = sourceIP, effect
	})

	// An anonymous request that will be denied, exactly like the scanner traffic.
	r := httptest.NewRequest(http.MethodGet, "/lol.php", nil)
	r.RemoteAddr = "172.17.0.1:54321" // the proxy, not the client
	r.Header.Set("X-Forwarded-For", "20.104.50.220, 10.0.0.5")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if gotEffect != "Deny" {
		t.Fatalf("expected the anonymous request to be denied, got effect %q", gotEffect)
	}
	if gotIP != "20.104.50.220" {
		t.Errorf("audit recorded source IP %q, want the original client 20.104.50.220."+
			" Recording the proxy address makes every audit entry identical", gotIP)
	}
}
