package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// The exit code is the whole interface: Docker reads it and nothing else.
// 2 for a usage mistake keeps it distinguishable from 1, an unhealthy server,
// so a broken HEALTHCHECK line does not look like a failing service.
func TestHealthcheckArgHandling(t *testing.T) {
	if got := runHealthcheck([]string{"--help"}); got != 0 {
		t.Errorf("--help exited %d, want 0", got)
	}
	if got := runHealthcheck([]string{"--nonsense"}); got != 2 {
		t.Errorf("an unknown argument exited %d, want 2 (usage error)", got)
	}
	if got := runHealthcheck([]string{"-config"}); got != 2 {
		t.Errorf("-config with no value exited %d, want 2 (usage error)", got)
	}
}

// With no server listening the probe must report unhealthy, not crash and not
// pass. A config path that does not exist is fine: the server runs on defaults
// in that case, so the check should follow it rather than refuse.
func TestHealthcheckReportsUnhealthyWhenNothingIsListening(t *testing.T) {
	got := runHealthcheck([]string{"-config", t.TempDir() + "/absent.yaml"})
	if got != 1 {
		t.Errorf("probe against a dead server exited %d, want 1", got)
	}
}

// The probe added server.base_path to /health. The server serves /health at
// its root whatever the base path (the proxy strips the prefix), so every
// container with VAULTS3_BASE_PATH set was reported unhealthy.
func TestHealthcheckProbesTheRootWithABasePath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.Error(w, "not here", http.StatusForbidden)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	cfgPath := filepath.Join(t.TempDir(), "vaults3.yaml")
	cfg := fmt.Sprintf("server:\n  address: \"127.0.0.1\"\n  port: %d\n  base_path: \"/vaults3\"\n", port)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runHealthcheck([]string{"-config", cfgPath}); got != 0 {
		t.Errorf("healthy server behind a base path exited %d, want 0", got)
	}
}
