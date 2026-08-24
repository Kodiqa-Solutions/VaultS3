package main

import "testing"

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
