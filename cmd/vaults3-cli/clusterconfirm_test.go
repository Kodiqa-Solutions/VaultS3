package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeClusterAPI records every cluster change it is asked to make.
func fakeClusterAPI(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"token":"t"}`)
	})
	mux.HandleFunc("/api/v1/cluster/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		fmt.Fprint(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	prev := [3]string{endpoint, accessKey, secretKey}
	endpoint, accessKey, secretKey = srv.URL, "admin", "secret"
	t.Cleanup(func() { endpoint, accessKey, secretKey = prev[0], prev[1], prev[2] })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

// withTerminal makes the CLI believe it has (or has not) a terminal, answering
// the prompt with answer.
func withTerminal(t *testing.T, tty bool, answer string) {
	t.Helper()
	prevTTY, prevIn := stdinIsTerminal, confirmInput
	stdinIsTerminal = func() bool { return tty }
	confirmInput = strings.NewReader(answer)
	t.Cleanup(func() { stdinIsTerminal, confirmInput = prevTTY, prevIn })
}

// cluster leave and cluster decommission changed the cluster on the first
// keystroke, unlike storage reclaim --apply, which asks. They now ask on a
// terminal and need --yes without one.
func TestClusterLeaveAndDecommissionAskFirst(t *testing.T) {
	for _, sub := range []string{"leave", "decommission"} {
		t.Run(sub+" no terminal, no --yes", func(t *testing.T) {
			calls := fakeClusterAPI(t)
			withTerminal(t, false, "")
			_, errOut, failed := runCLI(t, "cluster", sub, "node-2")
			if !failed || !strings.Contains(errOut, "--yes") {
				t.Errorf("want a refusal naming --yes, got failed=%v %q", failed, errOut)
			}
			if c := calls(); len(c) != 0 {
				t.Errorf("the cluster was changed without confirmation: %v", c)
			}
		})
		t.Run(sub+" answered no", func(t *testing.T) {
			calls := fakeClusterAPI(t)
			withTerminal(t, true, "n\n")
			if _, _, failed := runCLI(t, "cluster", sub, "node-2"); !failed {
				t.Error("a declined prompt reported success")
			}
			if c := calls(); len(c) != 0 {
				t.Errorf("the cluster was changed after the prompt was declined: %v", c)
			}
		})
		t.Run(sub+" answered yes", func(t *testing.T) {
			calls := fakeClusterAPI(t)
			withTerminal(t, true, "y\n")
			if _, errOut, failed := runCLI(t, "cluster", sub, "node-2"); failed {
				t.Fatalf("confirmed %s failed: %s", sub, errOut)
			}
			if len(calls()) == 0 {
				t.Error("a confirmed change was not sent")
			}
		})
		t.Run(sub+" --yes", func(t *testing.T) {
			calls := fakeClusterAPI(t)
			withTerminal(t, false, "")
			if _, errOut, failed := runCLI(t, "cluster", sub, "--yes", "node-2"); failed {
				t.Fatalf("%s --yes failed: %s", sub, errOut)
			}
			if len(calls()) == 0 {
				t.Error("--yes did not send the change")
			}
		})
	}
}

func TestClusterLeaveRefusesUnknownFlags(t *testing.T) {
	calls := fakeClusterAPI(t)
	withTerminal(t, false, "")
	_, errOut, failed := runCLI(t, "cluster", "leave", "node-2", "--yse")
	if !failed || !strings.Contains(errOut, "unknown flag") {
		t.Errorf("want an unknown-flag refusal, got failed=%v %q", failed, errOut)
	}
	if c := calls(); len(c) != 0 {
		t.Errorf("a mistyped flag still changed the cluster: %v", c)
	}
}
