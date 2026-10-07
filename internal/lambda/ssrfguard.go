package lambda

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/netguard"
)

// Dial-time SSRF guard for function URLs.
//
// A function URL is checked when the trigger is saved, but only as written: a
// literal private address is refused, while a hostname is taken on trust. A
// name that resolves to 127.0.0.1, to an internal service or to the cloud
// metadata service on 169.254.169.254 at call time therefore had the server
// POST every event, object bodies included, wherever it pointed, and a DNS
// answer can change between the save and the call. The check is applied to
// the address actually dialed, on every connection, the way the migration
// client does it (internal/migrate/ssrfguard.go).

// dialControl vets each address the function client dials. It is a variable
// so tests, whose function servers listen on 127.0.0.1, can lift it.
var dialControl = guardedControl

// guardedControl refuses a connection to an address the server should never be
// made to reach on a trigger's behalf.
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
		return fmt.Errorf("blocked destination %s: loopback, private, link-local and metadata addresses are not allowed as function URLs", ip)
	}
	return nil
}

// blockedIP reports whether an address is off limits for a function URL. It
// is the same list webhooks use. A copy of its own here had drifted and let a
// function URL reach carrier-grade NAT space (100.64.0.0/10).
func blockedIP(ip net.IP) bool {
	return netguard.Blocked(ip)
}

// newFunctionClient is the HTTP client that calls function URLs. Redirects
// are not followed, since the destination is vetted at dial time but a
// redirect would also skip the save-time check. No proxy is used either,
// because the guard would then vet the proxy's address and not the function's.
// With allowPrivate only metadata addresses are refused.
func newFunctionClient(timeout time.Duration, allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		d.Control = func(network, address string, c syscall.RawConn) error {
			if dialControl == nil {
				return nil
			}
			return dialControl(network, address, c)
		}
	} else {
		// Private destinations are allowed, cloud metadata addresses never are.
		d.Control = netguard.MetadataControl
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.DialContext(ctx, network, addr)
			},
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 5 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// AllowPrivateEndpoints lets function URLs reach loopback, private and
// link-local addresses, for an operator who runs their functions on their own
// network. Call it before Start.
func (m *TriggerManager) AllowPrivateEndpoints(allow bool) {
	m.client = newFunctionClient(m.client.Timeout, allow)
}
