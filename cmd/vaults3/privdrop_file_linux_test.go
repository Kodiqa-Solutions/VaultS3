//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression (5.0.13 review): logging.file_path: /dev/stdout (the usual container way to log).
func TestAccessLogOnStdoutWithPUID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root")
	}
	fi, _ := os.Stdout.Stat()
	t.Logf("stdout mode %v", fi.Mode())
	_, err := takeOwnership("/dev/stdout", ownFile, 99, 100, fileOwner)
	t.Logf("takeOwnership(/dev/stdout): %v", err)
	if err != nil {
		t.Fatalf("PUID set + logging to /dev/stdout: server would not start: %v", err)
	}
}

// Regression (5.0.13 review): the access log path, in a folder the dropped user can write, replaced by
// a symlink to a root file: the next start, as root, gives that file away.
func TestAPlantedAccessLogLinkGivesNothingAway(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root")
	}
	base := t.TempDir()
	os.WriteFile(filepath.Join(base, "rootfile"), []byte("x"), 0o600)
	logs := filepath.Join(base, "logs")
	os.MkdirAll(logs, 0o755)
	os.Chown(logs, 99, 100) // the dropped user's folder
	os.Symlink(filepath.Join(base, "rootfile"), filepath.Join(logs, "access.log"))
	if _, err := takeOwnership(filepath.Join(logs, "access.log"), ownFile, 99, 100, fileOwner); err != nil {
		t.Fatal(err)
	}
	u, _, _ := fileOwner(mustLstat(t, filepath.Join(base, "rootfile")))
	if u == 99 {
		t.Fatalf("a root-owned file reached through a symlink the dropped user planted was given to it")
	}
}

func mustLstat(t *testing.T, p string) os.FileInfo {
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// A regular log file, new or existing, is given to the user.
func TestARegularAccessLogIsGiven(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root")
	}
	base := t.TempDir()
	for _, p := range []string{filepath.Join(base, "new", "access.log"), filepath.Join(base, "existing.log")} {
		if p == filepath.Join(base, "existing.log") {
			os.WriteFile(p, []byte("old"), 0o600)
		}
		if _, err := takeOwnership(p, ownFile, 99, 100, fileOwner); err != nil {
			t.Fatal(err)
		}
		if u, g, _ := fileOwner(mustLstat(t, p)); u != 99 || g != 100 {
			t.Errorf("%s owned by %d:%d", p, u, g)
		}
	}
	if u, _, _ := fileOwner(mustLstat(t, filepath.Join(base, "new"))); u == 99 {
		t.Error("the log's folder was given away too")
	}
}
