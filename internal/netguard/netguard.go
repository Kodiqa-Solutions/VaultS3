// Package netguard refuses outbound connections to addresses a caller should
// never be able to make the server reach: loopback, private, link-local,
// unspecified and the cloud metadata services.
//
// The check runs on the address actually being dialled, after DNS resolution,
// so a hostname that resolves to 127.0.0.1 or 169.254.169.254 is refused, and so
// is a redirect or a DNS answer that changes after a URL was validated. Checking
// the URL's host when it is saved catches only literal addresses.
package netguard

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

// Blocked reports whether an address is off limits for a caller-supplied
// destination.
func Blocked(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// Carrier-grade NAT space is not "private" to Go but is internal in practice.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xC0 == 64 {
		return true
	}
	if ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return true
	}
	// IPv4-mapped IPv6 hides a loopback or private address behind ::ffff:.
	if v4 := ip.To4(); v4 != nil && !ip.Equal(v4) {
		return Blocked(v4)
	}
	return false
}

// Metadata reports whether an address belongs to a cloud metadata service: the
// IPv4 link-local range, where AWS, GCP, Azure and others serve credentials on
// 169.254.169.254, and AWS's IPv6 endpoint. These stay refused even when an
// operator allows private destinations, since nothing a bucket owner points a
// webhook at should be able to read the server's cloud credentials.
func Metadata(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 169 && v4[1] == 254
	}
	return ip.Equal(net.ParseIP("fd00:ec2::254"))
}

// MetadataControl is a dialer Control that refuses only metadata addresses.
func MetadataControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("blocked destination %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil || Metadata(ip) {
		return fmt.Errorf("blocked destination %s: cloud metadata addresses are never allowed", host)
	}
	return nil
}

func control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("blocked destination %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil || Blocked(ip) {
		return fmt.Errorf("blocked destination %s: loopback, private, link-local and metadata addresses are not allowed", host)
	}
	return nil
}

// DialContext returns a dial function that refuses blocked addresses. With
// allowPrivate, for an operator who points the server at a service on their own
// network on purpose, only metadata addresses are refused. The save-time check
// refuses those too, but only as the URL is written, and a hostname can resolve
// to one later.
func DialContext(timeout time.Duration, allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout, Control: control}
	if allowPrivate {
		d.Control = MetadataControl
	}
	return d.DialContext
}
