package migrate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"

	"github.com/Kodiqa-Solutions/VaultS3/internal/netguard"
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

// ErrPrivateSource is returned, wrapped, when a migration source resolves to a
// loopback or private address while those are not allowed. It names the
// setting that allows them, so the operator learns why and what to change; it
// says nothing about what is at the address.
var ErrPrivateSource = errors.New("the source is at a loopback or private address, which migration refuses by default; " +
	"to migrate from a source on your own network set migration.allow_private_sources: true " +
	"(env VAULTS3_ALLOW_PRIVATE_MIGRATION_SOURCES=true)")

// guardedControl refuses a connection to an address the server should never be
// made to reach on a caller's behalf.
func guardedControl(network, address string, _ syscall.RawConn) error {
	ip, err := dialIP(address)
	if err != nil {
		return err
	}
	if neverAllowedIP(ip) {
		return fmt.Errorf("blocked destination %s: link-local, cloud metadata, unspecified and multicast addresses are never migration sources", ip)
	}
	if blockedIP(ip) {
		return fmt.Errorf("blocked destination %s: %w", ip, ErrPrivateSource)
	}
	return nil
}

// privateAllowedControl is the dial check once an operator has allowed
// sources on their own network: loopback and private addresses connect, but
// the cloud metadata service and the other addresses no S3 server is reached
// at stay refused, so allowing a LAN does not also open instance credentials.
func privateAllowedControl(network, address string, _ syscall.RawConn) error {
	ip, err := dialIP(address)
	if err != nil {
		return err
	}
	if neverAllowedIP(ip) {
		return fmt.Errorf("blocked destination %s: link-local, cloud metadata, unspecified and multicast addresses are never migration sources", ip)
	}
	return nil
}

// dialIP is the IP a dial is about to connect to.
func dialIP(address string) (net.IP, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("blocked destination %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("blocked destination %q", address)
	}
	return ip, nil
}

// neverAllowedIP reports an address that is refused even when private sources
// are allowed: the cloud metadata services (link-local, Alibaba's
// 100.100.100.200, the private IPv6 fd00:ec2::254, and any of them carried in
// a NAT64 or 6to4 address), other link-local addresses, unspecified and
// multicast.
func neverAllowedIP(ip net.IP) bool {
	return netguard.Metadata(ip) || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast()
}

// blockedIP reports whether an address is off limits for a caller-supplied
// destination. It is the shared guard: a copy of its own here had drifted and
// let a migration reach carrier-grade NAT space, Alibaba Cloud's metadata
// service among it.
func blockedIP(ip net.IP) bool { return netguard.Blocked(ip) }

// dialContextGuarded is the dialer the migration client uses. allowPrivate
// lifts the restriction for an operator migrating from a source on their own
// network, which is a legitimate deployment and cannot be done otherwise.
func dialContextGuarded(d *net.Dialer, allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d.Control = guardedControl
	if allowPrivate {
		d.Control = privateAllowedControl
	}
	return d.DialContext
}
