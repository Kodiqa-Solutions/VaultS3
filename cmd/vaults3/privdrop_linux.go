//go:build linux

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

func fileOwner(fi os.FileInfo) (int, int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// dropPrivileges, started as root with PUID or PGID set, gives the server's
// folders to that user and group and switches to them for good. It must run
// before the server opens any file.
func dropPrivileges(cfg *config.Config) error {
	uid, gid, ok, err := privDropTarget(os.Getenv)
	if err != nil || !ok {
		return err
	}
	if os.Geteuid() != 0 {
		if os.Getuid() != uid {
			slog.Warn("PUID/PGID are set but the container does not start as root, so they are ignored; start it as root (--user 0:0) to use them",
				"running_uid", os.Getuid(), "PUID", uid)
		}
		return nil
	}
	if uid == 0 {
		slog.Warn("PUID=0: the server keeps running as root")
		return nil
	}
	// The container's standard output and error belong to root when it
	// starts as root. The server writes its log to them as it is, but an
	// access log at /dev/stdout is opened again by path, which the dropped
	// user could not. Docker gives them to the container's user the same way
	// when it starts as that user.
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if err := f.Chown(uid, gid); err != nil {
			slog.Debug("could not give standard output to the PUID/PGID user", "error", err)
		}
	}
	for _, o := range ownedPaths(cfg) {
		walked, err := takeOwnership(o.path, o.mode, uid, gid, fileOwner)
		if err != nil {
			return fmt.Errorf("give %s to %d:%d: %w", o.path, uid, gid, err)
		}
		if walked {
			slog.Info("gave a folder to the PUID/PGID user", "dir", o.path, "uid", uid, "gid", gid)
		}
	}
	// Supplementary groups first, while still root. Since Go 1.16 these
	// apply to every thread of the process.
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("set groups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("set group %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("set user %d: %w", uid, err)
	}
	if os.Geteuid() != uid || os.Getegid() != gid {
		return fmt.Errorf("still running as %d:%d after switching to %d:%d", os.Geteuid(), os.Getegid(), uid, gid)
	}
	// Root cannot be taken back.
	if err := syscall.Setuid(0); err == nil {
		return fmt.Errorf("could switch back to root after dropping to %d", uid)
	}
	slog.Info("running as the PUID/PGID user", "uid", uid, "gid", gid)
	return nil
}

// takeFile gives the access log file to uid:gid. Only a regular file with a
// single name is changed, through the open handle, and nothing the dropped
// user could have planted is followed: a symbolic link at the file, or at a
// folder on its path that the user owns, would hand its target, a root file
// or folder, to that user. /dev/stdout and other non-regular files are left
// as they are (standard output is given over separately). The file is opened
// without blocking, so a FIFO put in its place cannot stall the start.
func takeFile(p string, uid, gid int) error {
	abs, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	for d := filepath.Dir(abs); ; d = filepath.Dir(d) {
		if fi, err := os.Lstat(d); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if u, _, ok := fileOwner(fi); ok && u == uid {
				slog.Warn("the access log's path goes through a link the PUID user owns; leaving the log as it is", "path", p, "link", d)
				return nil
			}
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return err
	}
	if fi, err := os.Lstat(abs); err == nil && !fi.Mode().IsRegular() {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	// A log path in a system folder (logging.file_path pointed at the
	// server's binary, or /etc/shadow) would give that file away, as a
	// folder setting would. Links in its folders are resolved first.
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return err
	}
	if sys := systemPath(filepath.Join(dir, filepath.Base(abs))); sys != "" {
		return fmt.Errorf("the access log %s is in %s, a system folder; refusing to give it away", p, sys)
	}
	f, err := openLogNoFollow(abs)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || !ok || sys.Nlink != 1 {
		return nil
	}
	return f.Chown(uid, gid)
}

// openLogNoFollow opens the access log for the ownership change: it creates
// it if missing, never follows a link at it, and does not block on a FIFO.
func openLogNoFollow(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o640)
}

// linkCount is how many names a file has.
func linkCount(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
