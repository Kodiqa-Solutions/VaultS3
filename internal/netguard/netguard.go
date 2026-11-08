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
	// NAT64 and 6to4 addresses carry an IPv4 address that a gateway forwards
	// to, so they are as internal as the address they carry. Site-local
	// (fec0::/10, deprecated) is internal too.
	if v4 := embeddedIPv4(ip); v4 != nil {
		return Blocked(v4)
	}
	if ip16 := ip.To16(); ip16 != nil && ip.To4() == nil && ip16[0] == 0xfe && ip16[1]&0xc0 == 0xc0 {
		return true
	}
	return false
}

// embeddedIPv4 is the IPv4 address inside a NAT64 (64:ff9b::/96), 6to4
// (2002::/16) or deprecated IPv4-compatible (::a.b.c.d) address, or nil.
func embeddedIPv4(ip net.IP) net.IP {
	ip16 := ip.To16()
	if ip16 == nil || ip.To4() != nil {
		return nil
	}
	nat64 := net.IP{0, 0x64, 0xff, 0x9b, 0, 0, 0, 0, 0, 0, 0, 0}
	if net.IP(ip16[:12]).Equal(nat64) {
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15])
	}
	if ip16[0] == 0x20 && ip16[1] == 0x02 {
		return net.IPv4(ip16[2], ip16[3], ip16[4], ip16[5])
	}
	if net.IP(ip16[:12]).Equal(make(net.IP, 12)) && !ip.IsUnspecified() && !ip.IsLoopback() {
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15])
	}
	return nil
}

// Metadata reports whether an address belongs to a cloud metadata service: the
// IPv4 link-local range, where AWS, GCP, Azure and others serve credentials on
// 169.254.169.254, and AWS's IPv6 endpoint. These stay refused even when an
// operator allows private destinations, since nothing a bucket owner points a
// webhook at should be able to read the server's cloud credentials.
func Metadata(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		// Link-local (AWS, GCP, Azure, ...) and Alibaba Cloud's 100.100.100.200.
		return (v4[0] == 169 && v4[1] == 254) || v4.Equal(net.IPv4(100, 100, 100, 200))
	}
	if v4 := embeddedIPv4(ip); v4 != nil {
		return Metadata(v4)
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
