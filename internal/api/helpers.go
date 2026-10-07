package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	// Limit request body to 1MB to prevent memory exhaustion
	limited := io.LimitReader(r.Body, 1<<20)
	return json.NewDecoder(limited).Decode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$`)

// validateBucketName checks DNS-compatible bucket naming rules.
func validateBucketName(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return fmt.Errorf("bucket name must be between 3 and 63 characters")
	}
	if !bucketNameRe.MatchString(name) {
		return fmt.Errorf("bucket name must be lowercase alphanumeric, may contain hyphens and dots, cannot start or end with hyphen/dot")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("bucket name must not contain consecutive dots")
	}
	return nil
}

// validateObjectKey checks object key constraints.
func validateObjectKey(key string) error {
	if len(key) == 0 {
		return fmt.Errorf("object key must not be empty")
	}
	if len(key) > 1024 {
		return fmt.Errorf("object key must not exceed 1024 characters")
	}
	if strings.ContainsRune(key, 0) {
		return fmt.Errorf("object key must not contain null bytes")
	}
	// Prevent path traversal
	for _, segment := range strings.Split(key, "/") {
		if segment == ".." {
			return fmt.Errorf("object key must not contain '..' path segments")
		}
	}
	return nil
}

// ValidateWebhookURL checks that a URL is safe to call (prevents SSRF).
//
// The host is resolved and EVERY address it resolves to is checked. Only
// literal IPs used to be checked, so a name such as internal.example.com that
// resolved to 10.0.0.5, 127.0.0.1 or 169.254.169.254 passed, and the function
// was then called with the server's network position. A name that does not
// resolve is refused too, since nothing about it can be checked.
//
// A check at save time cannot stop a name that changes its answer later (DNS
// rebinding). That needs the dial-time guard the migration client uses
// (internal/migrate/ssrfguard.go) on the client that calls the URL.
func ValidateWebhookURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL must have a host")
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return fmt.Errorf("URL must not point to localhost")
	}
	if strings.EqualFold(host, "metadata.google.internal") {
		return fmt.Errorf("URL must not point to cloud metadata service")
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ips, err = lookupWebhookHost(ctx, host)
		if err != nil {
			return fmt.Errorf("could not resolve %s: %v", host, err)
		}
		if len(ips) == 0 {
			return fmt.Errorf("%s resolves to no address", host)
		}
	}
	for _, ip := range ips {
		if blockedWebhookIP(ip) {
			if ip.String() == host {
				return fmt.Errorf("URL must not point to a loopback, private, link-local or metadata address (%s)", ip)
			}
			return fmt.Errorf("URL host %s resolves to %s, a loopback, private, link-local or metadata address", host, ip)
		}
	}
	return nil
}

// lookupWebhookHost resolves a webhook host. A variable so tests can answer
// without real DNS.
var lookupWebhookHost = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// blockedWebhookIP reports whether an address is off limits for a URL the
// server calls on a caller's behalf. Same rule as the migration client's
// dial-time guard (blockedIP in internal/migrate/ssrfguard.go), which is not
// exported.
func blockedWebhookIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return true
	}
	// IPv4-mapped IPv6 hides a loopback or private address behind ::ffff:.
	if v4 := ip.To4(); v4 != nil && !ip.Equal(v4) {
		return blockedWebhookIP(v4)
	}
	return false
}
