package middleware

import (
	"context"
	"net/http"
	"strings"
)

type forwardedTLSKey struct{}

// WithForwardedTLS records whether the client's own connection was TLS, for a
// request that reached this server through something it trusts: a reverse
// proxy in trusted_proxies (its X-Forwarded-Proto), or another cluster node
// (proven by the cluster secret). Only those handlers may call it.
func WithForwardedTLS(r *http.Request, secure bool) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), forwardedTLSKey{}, secure))
}

// ForwardedProtoHTTPS reports whether the last X-Forwarded-Proto value is
// https. The proxy nearest the server writes the last one.
func ForwardedProtoHTTPS(r *http.Request) bool {
	vals := r.Header.Values("X-Forwarded-Proto")
	if len(vals) == 0 {
		return false
	}
	parts := strings.Split(vals[len(vals)-1], ",")
	return strings.EqualFold(strings.TrimSpace(parts[len(parts)-1]), "https")
}

// SecureTransport is aws:SecureTransport for r: TLS to this server, or to a
// trusted proxy in front of it. Behind nginx terminating TLS, r.TLS is always
// nil, so a bucket policy denying "aws:SecureTransport": "false" refused every
// request, HTTPS ones included.
//
// A recorded value decides over r.TLS, which on a request forwarded by
// another cluster node describes the hop between the nodes, not the client.
func SecureTransport(r *http.Request) bool {
	if v, ok := r.Context().Value(forwardedTLSKey{}).(bool); ok {
		return v
	}
	return r.TLS != nil
}
