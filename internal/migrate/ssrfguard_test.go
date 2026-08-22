package migrate

import (
	"net"
	"testing"
)

// A caller-supplied migration endpoint is a server-side request primitive. It
// must not be able to reach loopback, the internal network, or the cloud
// metadata service, where the reply is instance credentials (security
// assessment finding 6).
func TestBlockedDestinations(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"10.0.0.5", "172.16.4.1", "192.168.1.10",
		"169.254.169.254", // AWS/GCP/Azure instance metadata
		"fd00:ec2::254",   // AWS IMDS over IPv6
		"0.0.0.0", "224.0.0.1",
		"::ffff:127.0.0.1", // loopback smuggled through an IPv4-mapped address
		"::ffff:10.1.2.3",
	}
	for _, addr := range blocked {
		if !blockedIP(net.ParseIP(addr)) {
			t.Errorf("%s is reachable as a migration source, it must not be", addr)
		}
	}
	allowed := []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946", "8.8.8.8"}
	for _, addr := range allowed {
		if blockedIP(net.ParseIP(addr)) {
			t.Errorf("%s is blocked, but a public source is a legitimate migration", addr)
		}
	}
}

// Only http and https, and only with a host: anything else is a way to reach
// something that is not an S3 endpoint at all.
func TestValidateEndpointRejectsOtherSchemes(t *testing.T) {
	for _, bad := range []string{"file:///etc/passwd", "gopher://x/", "ftp://host/", "http://"} {
		if err := ValidateEndpoint(bad); err == nil {
			t.Errorf("ValidateEndpoint(%q) accepted it", bad)
		}
	}
	for _, good := range []string{"https://s3.example.com", "http://minio.internal:9000/"} {
		if err := ValidateEndpoint(good); err != nil {
			t.Errorf("ValidateEndpoint(%q) = %v, want nil", good, err)
		}
	}
}
