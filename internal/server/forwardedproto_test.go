package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/middleware"
)

// Behind nginx terminating TLS, aws:SecureTransport comes from the trusted
// proxy's X-Forwarded-Proto. From any other peer the header is ignored.
func TestForwardedProtoOnlyFromATrustedProxy(t *testing.T) {
	var got bool
	h := trustedProxies([]string{"10.0.0.1"}, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = middleware.SecureTransport(r)
	}))
	for _, c := range []struct {
		peer, proto string
		want        bool
	}{
		{"10.0.0.1:5000", "https", true},
		{"10.0.0.1:5000", "http", false},
		{"10.0.0.1:5000", "", false},
		{"203.0.113.9:5000", "https", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.peer
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		got = false
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("peer %s proto %q: SecureTransport %v, want %v", c.peer, c.proto, got, c.want)
		}
	}
	// A client's forged value before the proxy's own: the last one counts.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:5000"
	r.Header.Add("X-Forwarded-Proto", "https")
	r.Header.Add("X-Forwarded-Proto", "http")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got {
		t.Error("a forged first X-Forwarded-Proto was believed")
	}
}
