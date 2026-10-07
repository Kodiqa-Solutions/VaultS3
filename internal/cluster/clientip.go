package cluster

import (
	"crypto/hmac"
	"net"
	"net/http"
)

// ClientIPHeader carries the address of the client a forwarded request came
// from.
//
// A request is forwarded to the node that holds its object before anything
// checks who sent it, so the node that does check (the IP allow and block
// lists, a user's AllowedCIDRs, aws:SourceIp in a policy, the rate limiter, the
// audit trail) saw the forwarding node as the client. A client blocked by
// address got through whenever its request landed on a node that did not hold
// the object, and an allowlist that named only real clients refused every
// forwarded request.
//
// The header is only believed on a request that also carries the cluster
// secret, so a client cannot choose its own address by sending it.
const ClientIPHeader = "X-VaultS3-Client-IP"

// markForwardedClient stamps an outbound forwarded request with the address
// the inbound one came from and the cluster secret that makes the receiving
// node trust it. out.RemoteAddr is the inbound peer: on a request that already
// came through TrustForwardedClient it has been replaced with the original
// client, so a second hop passes the same client along.
func markForwardedClient(out *http.Request, secret string) {
	out.Header.Del(ClientIPHeader)
	out.Header.Del(clusterSecretHeader)
	if secret == "" {
		return
	}
	ip := hostOnly(out.RemoteAddr)
	if net.ParseIP(ip) == nil {
		return
	}
	out.Header.Set(ClientIPHeader, ip)
	out.Header.Set(clusterSecretHeader, secret)
}

// TrustForwardedClient wraps the S3 handler on a clustered node. A request a
// peer forwarded, proven by the cluster secret, has its RemoteAddr replaced with
// the client address the peer recorded, so every check downstream that reads
// RemoteAddr sees the real client. On any other request both headers are
// removed, so a client cannot claim an address of its own choosing.
func TrustForwardedClient(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claimed := r.Header.Get(ClientIPHeader)
		proof := r.Header.Get(clusterSecretHeader)
		if claimed != "" || proof != "" {
			r.Header.Del(ClientIPHeader)
			r.Header.Del(clusterSecretHeader)
			if secret != "" && proof != "" && hmac.Equal([]byte(proof), []byte(secret)) {
				if ip := net.ParseIP(claimed); ip != nil {
					r.RemoteAddr = net.JoinHostPort(ip.String(), "0")
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostOnly strips the port from a host:port pair, or returns it unchanged.
func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
