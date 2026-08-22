package migrate

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// SSRF guard for the migration source (security assessment finding 6).
//
// The migration client fetches from an endpoint the caller supplies, and it used
// to accept any of them. That let a console user point the server at
// 127.0.0.1, at anything on the internal network, or at the cloud metadata
// service on 169.254.169.254, and read back what came out through the error
// message. On a cloud instance that is a direct path to instance credentials.
//
// The check is applied at DIAL time, not only when the URL is parsed. Validating
// a hostname up front and then letting the transport resolve it again leaves a
// DNS-rebinding window where the name resolves to something harmless during the
// check and to a loopback address a moment later.

// ValidateEndpoint rejects a source endpoint that is not a plain http(s) URL.
// The address check happens at dial time, since a name's address can change
// between here and the connection.
func ValidateEndpoint(endpoint string) error {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return fmt.Errorf("invalid endpoint: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("endpoint scheme %q is not allowed, use http or https", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("endpoint has no host")
	}
	return nil
}

// guardedControl refuses a connection to an address the server should never be
// made to reach on a caller's behalf.
func guardedControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("blocked destination %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("blocked destination %q", address)
	}
	if blockedIP(ip) {
		return fmt.Errorf("blocked destination %s: loopback, private, link-local and metadata addresses are not allowed as migration sources", ip)
	}
	return nil
}

// blockedIP reports whether an address is off limits for a caller-supplied
// destination.
func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// The cloud metadata services, which are link-local and therefore already
	// covered, named here so the intent survives a refactor.
	if ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return true
	}
	// IPv4-mapped IPv6 hides a loopback or private address behind ::ffff:.
	if v4 := ip.To4(); v4 != nil && !ip.Equal(v4) {
		return blockedIP(v4)
	}
	return false
}

// dialContextGuarded is the dialer the migration client uses. allowPrivate
// lifts the restriction for an operator migrating from a source on their own
// network, which is a legitimate deployment and cannot be done otherwise.
func dialContextGuarded(d *net.Dialer, allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if !allowPrivate {
		d.Control = guardedControl
	}
	return d.DialContext
}
