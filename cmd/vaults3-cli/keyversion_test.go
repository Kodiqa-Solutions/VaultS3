package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeServer answers like a server of the given versions, with a key create
// response shaped the way that version shapes it, and counts key issues.
func fakeServer(t *testing.T, versions []string, response string) *int32 {
	t.Helper()
	var issued int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"token":"t"}`)
	})
	mux.HandleFunc("/api/v1/cluster/info", func(w http.ResponseWriter, _ *http.Request) {
		var nodes []string
		for _, v := range versions {
			nodes = append(nodes, fmt.Sprintf(`{"reachable":true,"version":%q}`, v))
		}
		fmt.Fprintf(w, `{"nodes":[%s]}`, strings.Join(nodes, ","))
	})
	mux.HandleFunc("/api/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt32(&issued, 1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, response)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	prev := [3]string{endpoint, accessKey, secretKey}
	endpoint, accessKey, secretKey = srv.URL, "admin", "secret"
	t.Cleanup(func() { endpoint, accessKey, secretKey = prev[0], prev[1], prev[2] })
	return &issued
}

const oldServerKey = `{"accessKey":"AKOLD","secretKey":"SKOLD","createdAt":"x"}`

// A server older than 4.4.79 takes the request and does something else: it
// ignores --user-policies and grants every bucket, and any new key rewrites the
// user's other keys. Refuse before issuing anything, for every mode.
func TestKeyCreateRefusesAnOlderServerBeforeIssuing(t *testing.T) {
	for _, mode := range [][]string{{"--user-policies"}, {"--bucket", "b"}, {"--all-buckets"}} {
		issued := fakeServer(t, []string{"v4.4.78"}, oldServerKey)
		args := append([]string{"key", "create", "alice"}, mode...)
		_, errOut, failed := runCLI(t, args...)
		if !failed || !strings.Contains(errOut, "4.4.79 or later") {
			t.Errorf("%v: want a refusal naming the version, got failed=%v %q", mode, failed, errOut)
		}
		if n := atomic.LoadInt32(issued); n != 0 {
			t.Errorf("%v: a key was issued on the old server (%d)", mode, n)
		}
	}
}

// On a cluster every node authorizes the key, so one old node is enough.
func TestKeyCreateCountsTheOldestNode(t *testing.T) {
	issued := fakeServer(t, []string{"v4.4.79", "v4.4.77", "v4.4.80"}, oldServerKey)
	_, errOut, failed := runCLI(t, "key", "create", "alice", "--all-buckets")
	if !failed || !strings.Contains(errOut, "v4.4.77") {
		t.Errorf("want a refusal naming the oldest node, got failed=%v %q", failed, errOut)
	}
	if atomic.LoadInt32(issued) != 0 {
		t.Error("a key was issued on a cluster with an old node")
	}
}

// A version that cannot be compared, such as "main" or "dev", is not refused.
// If the answer then does not say what was granted, --user-policies must not be
// reported as honoured.
func TestKeyCreateWarnsWhenAnUnversionedServerDoesNotSay(t *testing.T) {
	issued := fakeServer(t, []string{"main"}, oldServerKey)
	out, errOut, failed := runCLI(t, "key", "create", "alice", "--user-policies")
	if failed {
		t.Fatalf("unversioned server refused: %s", errOut)
	}
	if atomic.LoadInt32(issued) != 1 {
		t.Fatal("no key was issued")
	}
	if !strings.Contains(out, "WARNING") || strings.Contains(out, "exactly what the user's own policies allow") {
		t.Errorf("output claims the limit was honoured: %q", out)
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]bool{
		"v4.4.79": true, "4.4.79": true, "v4.4.79-local": true, "v4.4.79+meta": true,
		"main": false, "dev": false, "": false, "v4.4": false, "v4.x.1": false,
	} {
		if _, ok := parseVersion(in); ok != want {
			t.Errorf("parseVersion(%q) ok=%v, want %v", in, ok, want)
		}
	}
	for _, c := range []struct {
		v      string
		before bool
	}{{"v4.4.78", true}, {"v4.4.79", false}, {"v4.4.79-local", false}, {"v4.5.0", false}, {"v4.3.99", true}, {"v5.0.0", false}, {"v3.9.200", true}} {
		if got := versionBefore(c.v, 4, 4, 79); got != c.before {
			t.Errorf("versionBefore(%s, 4.4.79) = %v, want %v", c.v, got, c.before)
		}
	}
}
