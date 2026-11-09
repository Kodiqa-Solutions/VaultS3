//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// The drop changes the user of the whole process for good, so it runs in a
// child copy of this test binary.
func TestDropPrivilegesChild(t *testing.T) {
	base := os.Getenv("PRIVDROP_CHILD_BASE")
	if base == "" {
		t.Skip("helper for TestDropPrivilegesAsRoot")
	}
	cfg := &config.Config{}
	cfg.Storage.DataDir = filepath.Join(base, "datalink") // a symlink to data
	cfg.Storage.MetadataDir = filepath.Join(base, "metadata")
	cfg.Logging.Enabled = true
	cfg.Logging.FilePath = filepath.Join(base, "shared", "access.log")
	if err := dropPrivileges(cfg); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Storage.MetadataDir, "db"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write as the dropped user: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "rootonly", "f"), []byte("x"), 0o600); err == nil {
		t.Fatal("still able to write a root-only folder")
	}
}

// Regression (5.0.13): on a NAS, Docker creates the mounted folders owned
// by root and the server (uid 1000) could not open its metadata. Started as
// root with PUID/PGID, it takes the folders, files already in them
// included, and runs as that user, unable to become root again.
func TestDropPrivilegesAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (run in a Linux container)")
	}
	base := t.TempDir()
	// The test's own temp folders are 0700; real mount points are reachable.
	os.Chmod(filepath.Dir(base), 0o755)
	os.Chmod(base, 0o755)
	os.MkdirAll(filepath.Join(base, "data", "bucket"), 0o755)
	os.WriteFile(filepath.Join(base, "data", "bucket", "obj"), []byte("old"), 0o644)
	os.MkdirAll(filepath.Join(base, "rootonly"), 0o700)
	os.Symlink(filepath.Join(base, "data"), filepath.Join(base, "datalink"))
	// The metadata folder already belongs to the user, but a database
	// restored into it as root does not.
	os.MkdirAll(filepath.Join(base, "metadata"), 0o755)
	os.Chown(filepath.Join(base, "metadata"), 99, 100)
	os.WriteFile(filepath.Join(base, "metadata", "restored.db"), []byte("x"), 0o600)
	// Another program's file next to the access log.
	os.MkdirAll(filepath.Join(base, "shared", "other-app"), 0o755)
	os.WriteFile(filepath.Join(base, "shared", "other-app", "secret"), []byte("x"), 0o600)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDropPrivilegesChild$", "-test.v")
	cmd.Env = append(os.Environ(), "PRIVDROP_CHILD_BASE="+base, "PUID=99", "PGID=100")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestDropPrivilegesChild") {
		t.Fatalf("child: %v\n%s", err, out)
	}
	for _, p := range []string{"data", "data/bucket", "data/bucket/obj", "metadata", "metadata/db", "metadata/restored.db", "shared/access.log"} {
		fi, err := os.Lstat(filepath.Join(base, p))
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if st.Uid != 99 || st.Gid != 100 {
			t.Errorf("%s owned by %d:%d, want 99:100", p, st.Uid, st.Gid)
		}
	}
	for _, p := range []string{"shared", "shared/other-app", "shared/other-app/secret"} {
		st := mustStat(t, filepath.Join(base, p))
		if st.Uid != 0 {
			t.Errorf("%s, beside the access log, was given away: %d:%d", p, st.Uid, st.Gid)
		}
	}
	// Without PUID/PGID nothing changes.
	cmd = exec.Command(os.Args[0], "-test.run=^TestDropPrivilegesChild$", "-test.v")
	cmd.Env = append(os.Environ(), "PRIVDROP_CHILD_BASE="+base, "PUID=", "PGID=")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("without PUID the child should have stayed root and written the root-only folder:\n%s", out)
	}
}

func mustStat(t *testing.T, p string) *syscall.Stat_t {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t)
}
