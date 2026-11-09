package cluster

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/middleware"
)

// aws:SecureTransport on a forwarded request is the client's connection to
// the first node, not the hop between nodes, and only under the cluster
// secret (5.0.14 review).
func TestForwardedRequestsCarryTheClientsTransport(t *testing.T) {
	var got bool
	h := TrustForwardedClient("s3cr3t", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = middleware.SecureTransport(r)
	}))
	send := func(clientTLS, secret string, hopTLS bool) bool {
		r := httptest.NewRequest(http.MethodGet, "/b/k", nil)
		if hopTLS {
			r.TLS = &tls.ConnectionState{}
		}
		r.Header.Set(ClientIPHeader, "203.0.113.9")
		r.Header.Set(ClientTLSHeader, clientTLS)
		if secret != "" {
			r.Header.Set(clusterSecretHeader, secret)
		}
		got = false
		h.ServeHTTP(httptest.NewRecorder(), r)
		return got
	}
	if send("0", "s3cr3t", true) {
		t.Error("a plain-HTTP client forwarded over an HTTPS hop counted as TLS")
	}
	if !send("1", "s3cr3t", false) {
		t.Error("an HTTPS client forwarded over a plain hop counted as plain")
	}
	if send("1", "wrong", false) {
		t.Error("a client's own header was believed without the cluster secret")
	}

	// The forwarding side records what its inbound request was.
	in := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	in = middleware.WithForwardedTLS(in, true)
	out := in.Clone(in.Context())
	out.RemoteAddr = "203.0.113.9:1234"
	markForwardedClient(out, "s3cr3t")
	if out.Header.Get(ClientTLSHeader) != "1" {
		t.Errorf("forwarded header %q, want 1", out.Header.Get(ClientTLSHeader))
	}
}
