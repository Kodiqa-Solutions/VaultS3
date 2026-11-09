package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Regression (5.0.14 security review): only the first X-Forwarded-For line
// was read, so behind a proxy that appends its own line the client chose its
// own address.
func TestEveryForwardedForLineIsRead(t *testing.T) {
	var got string
	h := trustedProxies([]string{"10.0.0.5"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:4444"
	req.Header.Add("X-Forwarded-For", "192.168.1.50") // forged by the client
	req.Header.Add("X-Forwarded-For", "203.0.113.9")  // the proxy's own line
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "203.0.113.9" && got != "203.0.113.9:0" && !hasHost(got, "203.0.113.9") {
		t.Fatalf("client address %q, want 203.0.113.9", got)
	}
}

func hasHost(addr, host string) bool {
	h, _, err := net.SplitHostPort(addr)
	return err == nil && h == host
}
