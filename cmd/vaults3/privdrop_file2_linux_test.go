//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/accesslog"
	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

func TestStdoutLogAfterDropChild(t *testing.T) {
	if os.Getenv("STDOUT_LOG_CHILD") == "" {
		t.Skip("helper for TestAnAccessLogOnStdoutOpensAfterTheDrop")
	}
	cfg := &config.Config{}
	cfg.Logging.Enabled = true
	cfg.Logging.FilePath = "/dev/stdout"
	if err := dropPrivileges(cfg); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := accesslog.NewAccessLogger("/dev/stdout"); err != nil {
		t.Fatalf("the access logger as the dropped user: %v", err)
	}
}

// Regression (5.0.13 review): a container started as root has a standard
// output owned by root with mode 0600, so after the drop the access logger
// could not open /dev/stdout and the server did not start. Standard output
// and error are given to the user, as Docker does for a container user.
func TestAnAccessLogOnStdoutOpensAfterTheDrop(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStdoutLogAfterDropChild$", "-test.v")
	cmd.Env = append(os.Environ(), "STDOUT_LOG_CHILD=1", "PUID=99", "PGID=100")
	r, w, _ := os.Pipe() // a pipe of the test's own, owned by root, 0600
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	out := make([]byte, 64<<10)
	n, _ := readAll(r, out)
	err := cmd.Wait()
	if err != nil || !strings.Contains(string(out[:n]), "--- PASS: TestStdoutLogAfterDropChild") {
		t.Fatalf("child: %v\n%s", err, out[:n])
	}
}

func readAll(r *os.File, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Regression (5.0.13 review): a hard link to a root file, put where the log
// goes, had that file given to the user.
func TestAHardLinkedAccessLogIsNotGiven(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root")
	}
	base := t.TempDir()
	root := filepath.Join(base, "rootsecret")
	os.WriteFile(root, []byte("x"), 0o600)
	logs := filepath.Join(base, "logs")
	os.MkdirAll(logs, 0o755)
	if err := os.Link(root, filepath.Join(logs, "access.log")); err != nil {
		t.Fatal(err)
	}
	if err := takeFile(filepath.Join(logs, "access.log"), 99, 100); err != nil {
		t.Fatal(err)
	}
	if u, _, _ := fileOwner(mustLstat(t, root)); u == 99 {
		t.Error("a root file was given away through a hard link")
	}
}

// Regression (5.0.13 review): a folder on the log's path replaced by a link
// the user made, to a root folder, had the log created there and given away.
func TestALinkTheUserPlantedOnTheLogPathIsNotFollowed(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root")
	}
	base := t.TempDir()
	rootdir := filepath.Join(base, "etc")
	os.MkdirAll(rootdir, 0o755)
	user := filepath.Join(base, "data")
	os.MkdirAll(user, 0o755)
	os.Chown(user, 99, 100)
	link := filepath.Join(user, "logs")
	os.Symlink(rootdir, link)
	os.Lchown(link, 99, 100) // the user made it
	if err := takeFile(filepath.Join(link, "access.log"), 99, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(rootdir, "access.log")); err == nil {
		t.Error("the log was created in a root folder through the user's link")
	}
	// A link root made (a system path) is followed as before.
	sys := filepath.Join(base, "syslink")
	os.Symlink(rootdir, sys)
	if err := takeFile(filepath.Join(sys, "ok.log"), 99, 100); err != nil {
		t.Fatal(err)
	}
	if u, _, _ := fileOwner(mustLstat(t, filepath.Join(rootdir, "ok.log"))); u != 99 {
		t.Error("a log through a system link was not given")
	}
}

// Regression (5.0.13 review): a FIFO swapped in for the log after the check
// blocked the open, and the start, for good.
func TestTheLogOpenDoesNotBlockOnAFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := openLogNoFollow(p)
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("opening a FIFO in place of the log blocked")
		if r, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			<-done
			r.Close()
		}
	}
}

// Regression (5.0.14 security review): the walk of the metadata folder
// (every start) followed a hard link to a root file planted in it, and gave
// that file away. A file with more than one name is left as it is.
func TestAHardLinkInAWalkedFolderIsNotGiven(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root")
	}
	base := t.TempDir()
	root := filepath.Join(base, "rootsecret")
	_ = os.WriteFile(root, []byte("x"), 0o600)
	meta := filepath.Join(base, "metadata")
	_ = os.MkdirAll(meta, 0o755)
	_ = os.WriteFile(filepath.Join(meta, "vaults3.db"), []byte("db"), 0o600)
	if err := os.Link(root, filepath.Join(meta, "planted")); err != nil {
		t.Fatal(err)
	}
	if _, err := takeOwnership(meta, ownTreeAlways, 99, 100, fileOwner); err != nil {
		t.Fatal(err)
	}
	if u, _, _ := fileOwner(mustLstat(t, root)); u == 99 {
		t.Error("a root file was given away through a hard link in the metadata folder")
	}
	if u, _, _ := fileOwner(mustLstat(t, filepath.Join(meta, "vaults3.db"))); u != 99 {
		t.Error("the folder's own file was not given")
	}
}

// Regression (5.0.14 review): the access log path skipped the system-folder
// check, so logging.file_path naming the server's binary or /etc/shadow gave
// that file to the PUID user.
func TestALogFileInASystemFolderIsNotGivenAway(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root")
	}
	p := "/usr/local/bin/vaults3-logtest"
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(p) }()
	_, err := takeOwnership(p, ownFile, 4242, 4242, fileOwner)
	if err == nil {
		t.Error("a log path in /usr/local/bin was accepted")
	}
	if u, _, _ := fileOwner(mustLstat(t, p)); u == 4242 {
		t.Fatal("a file in /usr/local/bin was given to the PUID user")
	}
	// Through a link to a system folder.
	link := filepath.Join(t.TempDir(), "logs")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	if _, err := takeOwnership(filepath.Join(link, "shadow"), ownFile, 4242, 4242, fileOwner); err == nil {
		t.Error("a log path through a link into /etc was accepted")
	}
}
