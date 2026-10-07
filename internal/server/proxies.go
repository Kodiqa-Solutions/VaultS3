package server

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// trustedProxies restores the client's address on requests that arrive through
// a reverse proxy the operator listed in server.trusted_proxies.
//
// Every address-based decision (rate limits, IP allow and block lists,
// aws:SourceIp in policies, the login throttle, the audit log) reads
// RemoteAddr, because X-Forwarded-For is set by the client and anyone can put
// any address in it. Behind nginx that made every client the proxy. A proxy the
// operator trusts is believed: X-Forwarded-For is read from the right, skipping
// trusted proxies, and the first address that is not one of them is the client.
// From anywhere else the header is ignored.
func trustedProxies(cidrs []string, next http.Handler) http.Handler {
	var nets []*net.IPNet
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			if strings.Contains(c, ":") {
				c += "/128"
			} else {
				c += "/32"
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			slog.Warn("server.trusted_proxies: ignoring an entry that is not an address or CIDR", "entry", c)
			continue
		}
		nets = append(nets, n)
	}
	if len(nets) == 0 {
		return next
	}
	trusted := func(ip net.IP) bool {
		for _, n := range nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		peer := net.ParseIP(host)
		if peer == nil || !trusted(peer) {
			next.ServeHTTP(w, r)
			return
		}
		hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(hops[i]))
			if ip == nil {
				break
			}
			if !trusted(ip) {
				r2 := r.Clone(r.Context())
				r2.RemoteAddr = net.JoinHostPort(ip.String(), "0")
				next.ServeHTTP(w, r2)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
