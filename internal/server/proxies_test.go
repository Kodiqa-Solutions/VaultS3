package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func remoteSeen(t *testing.T, cidrs []string, remote, xff string) string {
	t.Helper()
	var got string
	h := trustedProxies(cidrs, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	h.ServeHTTP(httptest.NewRecorder(), r)
	return got
}

// X-Forwarded-For is believed only from a listed proxy, and then the client is
// the rightmost address that is not itself a trusted proxy: anything to its
// left was written by the client.
func TestTrustedProxiesRestoreTheClientOnlyFromAListedProxy(t *testing.T) {
	proxies := []string{"10.0.0.0/8", "192.168.1.5"}
	cases := []struct{ remote, xff, want string }{
		{"10.0.0.2:4000", "203.0.113.9", "203.0.113.9:0"},
		{"10.0.0.2:4000", "1.2.3.4, 203.0.113.9", "203.0.113.9:0"},
		{"10.0.0.2:4000", "203.0.113.9, 192.168.1.5", "203.0.113.9:0"},
		{"198.51.100.7:4000", "203.0.113.9", "198.51.100.7:4000"},
		{"10.0.0.2:4000", "", "10.0.0.2:4000"},
		{"10.0.0.2:4000", "not-an-ip", "10.0.0.2:4000"},
	}
	for _, c := range cases {
		if got := remoteSeen(t, proxies, c.remote, c.xff); got != c.want {
			t.Errorf("from %s with X-Forwarded-For %q: saw %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	if got := remoteSeen(t, nil, "10.0.0.2:4000", "203.0.113.9"); got != "10.0.0.2:4000" {
		t.Errorf("with no trusted proxies the header was believed: %s", got)
	}
}
