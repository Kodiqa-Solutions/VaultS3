//go:build !windows

package main

import (
	"strings"
	"testing"
	"time"
)

// The mount flag loop ended on "break", which only leaves the switch, so an
// unknown flag or a flag with no value spun forever at full CPU. Each case runs
// with a deadline so a regression fails instead of hanging the suite.
func TestParseMountFlagsNeverSpins(t *testing.T) {
	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"--bogus", "b", "/mnt"}, "unknown mount option --bogus"},
		{[]string{"--cache-size"}, "--cache-size needs a value"},
		{[]string{"--metadata-ttl"}, "--metadata-ttl needs a value"},
		{[]string{"--cache-size", "x", "b", "/mnt"}, "non-negative integer"},
		{[]string{"--metadata-ttl=-1", "b", "/mnt"}, "non-negative integer"},
	}
	for _, c := range cases {
		done := make(chan error, 1)
		go func() {
			_, _, _, err := parseMountFlags(c.args)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%v: err=%v, want %q", c.args, err, c.wantErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%v: flag parsing did not return", c.args)
		}
	}
}

func TestParseMountFlagsReadsValues(t *testing.T) {
	cache, ttl, rest, err := parseMountFlags([]string{"--cache-size", "128", "--metadata-ttl=9", "b", "/mnt"})
	if err != nil || cache != 128 || ttl != 9 || strings.Join(rest, " ") != "b /mnt" {
		t.Errorf("got cache=%d ttl=%d rest=%v err=%v", cache, ttl, rest, err)
	}
	cache, ttl, rest, err = parseMountFlags([]string{"b", "/mnt"})
	if err != nil || cache != 64 || ttl != 5 || len(rest) != 2 {
		t.Errorf("defaults: cache=%d ttl=%d rest=%v err=%v", cache, ttl, rest, err)
	}
}
