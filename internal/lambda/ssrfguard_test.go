package lambda

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// The tests' function servers listen on 127.0.0.1, which the dial guard
// refuses, so it is lifted for the package and switched back on by the tests
// that are about it.
func TestMain(m *testing.M) {
	dialControl = nil
	os.Exit(m.Run())
}

func withDialGuard(t *testing.T) {
	t.Helper()
	dialControl = guardedControl
	t.Cleanup(func() { dialControl = nil })
}

// A function URL was only checked as written when the trigger was saved, so a
// hostname that resolves to loopback, an internal service or the metadata
// service was called with every event. The address is now vetted on every
// dial, after DNS resolution.
func TestTriggerRefusesToDialAPrivateAddress(t *testing.T) {
	withDialGuard(t)
	m, _, _ := newManager(t)
	var hit int32
	fn := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&hit, 1) }))
	defer fn.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(fn.URL, "http://"))

	for _, u := range []string{fn.URL, "http://localhost:" + port} {
		m.executeTrigger(triggerJob{
			trigger: metadata.LambdaTrigger{ID: "t", FunctionURL: u},
			bucket:  "own", key: "in.txt", eventType: "s3:ObjectCreated:Put",
		})
	}
	if n := atomic.LoadInt32(&hit); n != 0 {
		t.Errorf("the function on a loopback address was called %d time(s)", n)
	}

	// An operator who runs functions on their own network can lift it.
	m.AllowPrivateEndpoints(true)
	m.executeTrigger(triggerJob{
		trigger: metadata.LambdaTrigger{ID: "t", FunctionURL: fn.URL},
		bucket:  "own", key: "in.txt", eventType: "s3:ObjectCreated:Put",
	})
	if atomic.LoadInt32(&hit) != 1 {
		t.Error("AllowPrivateEndpoints did not lift the guard")
	}
}

func TestBlockedIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.9": true, "192.168.1.1": true,
		"169.254.169.254": true, "169.254.1.1": true, "0.0.0.0": true, "::": true, "::1": true,
		"fd00:ec2::254": true, "fc00::1": true, "fe80::1": true, "::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true,
		"224.0.0.1": true, "100.64.0.1": true, "100.127.255.254": true, "100.128.0.1": false,
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700:4700::1111": false,
	} {
		if got := blockedIP(net.ParseIP(ip)); got != want {
			t.Errorf("blockedIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

// The client still refuses redirects, and the guard did not replace that.
func TestFunctionClientRefusesRedirects(t *testing.T) {
	c := newFunctionClient(0, true)
	if c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Error("the function client follows redirects")
	}
}

// With private endpoints allowed, a function URL still cannot reach a cloud
// metadata address.
func TestAllowPrivateEndpointsStillRefusesMetadata(t *testing.T) {
	c := newFunctionClient(time.Second, true)
	_, err := c.Get("http://169.254.169.254/latest/meta-data/")
	if err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Errorf("got %v, want a metadata refusal", err)
	}
}
