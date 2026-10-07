package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "192.168.0.1": true, "172.16.5.4": true, "169.254.169.254": true,
		"100.64.0.1": true, "::1": true, "fd00::1": true, "::ffff:127.0.0.1": true, "0.0.0.0": true,
		"8.8.8.8": false, "203.0.113.5": false, "2001:4860:4860::8888": false,
	} {
		if got := Blocked(net.ParseIP(addr)); got != want {
			t.Errorf("Blocked(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The check is on the address dialled, so a name resolving to loopback is
// refused even though its URL looks harmless.
func TestDialRefusesLoopbackByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	client := &http.Client{Transport: &http.Transport{DialContext: DialContext(time.Second, false)}}
	if _, err := client.Get("http://localhost:" + port + "/"); err == nil {
		t.Fatal("a request to localhost went through")
	}
	open := &http.Client{Transport: &http.Transport{DialContext: DialContext(time.Second, true)}}
	if _, err := open.Get("http://localhost:" + port + "/"); err != nil {
		t.Fatalf("with allowPrivate the request must go through: %v", err)
	}
	_ = context.Background()
}

// Allowing private destinations used to lift every check at dial time, so a
// webhook host that resolved to the metadata service after it was saved could
// read the server's cloud credentials.
func TestAllowPrivateStillRefusesMetadata(t *testing.T) {
	dial := DialContext(time.Second, true)
	for _, addr := range []string{"169.254.169.254:80", "[::ffff:169.254.169.254]:80", "[fd00:ec2::254]:80"} {
		_, err := dial(context.Background(), "tcp", addr)
		if err == nil || !strings.Contains(err.Error(), "metadata") {
			t.Errorf("dial %s with allowPrivate: %v, want a metadata refusal", addr, err)
		}
	}
	for addr, want := range map[string]bool{"169.254.1.1": true, "10.0.0.1": false, "127.0.0.1": false, "8.8.8.8": false} {
		if got := Metadata(net.ParseIP(addr)); got != want {
			t.Errorf("Metadata(%s) = %v, want %v", addr, got, want)
		}
	}
}
