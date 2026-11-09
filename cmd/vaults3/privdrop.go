package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// The image runs as uid 1000. A NAS (Unraid, Synology, TrueNAS) mounts host
// folders that Docker creates owned by root, so the server could not open
// its own metadata ("permission denied") and never started. The convention
// there is PUID/PGID: the container starts as root, takes ownership of its
// folders, and runs as that user. Nothing changes unless PUID or PGID is set.

// privDropTarget reads PUID and PGID. ok is false when neither is set; a
// value that is not a number, or is negative, is an error.
func privDropTarget(getenv func(string) string) (uid, gid int, ok bool, err error) {
	ps, gs := getenv("PUID"), getenv("PGID")
	if ps == "" && gs == "" {
		return 0, 0, false, nil
	}
	parse := func(name, v string, def int) (int, error) {
		if v == "" {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s=%q is not a user or group number", name, v)
		}
		return n, nil
	}
	if uid, err = parse("PUID", ps, 1000); err != nil {
		return 0, 0, false, err
	}
	if gid, err = parse("PGID", gs, uid); err != nil {
		return 0, 0, false, err
	}
	return uid, gid, true, nil
}

// How much of a path the dropped user is given.
type ownMode int

const (
	// ownTree: the folder and everything in it, walked when the folder has
	// another owner. For folders that hold only VaultS3's own files.
	ownTree ownMode = iota
	// ownTreeAlways: the same, walked on every start. For the metadata
	// folder, which is small: a database restored into it as root (with
	// docker exec, which runs as root in a container started as root) is
	// given back at the next start instead of failing it.
	ownTreeAlways
	// ownTop: only the folder itself, created when missing. For folders a
	// setting can point anywhere (a cold tier, erasure disks, a backup
	// target), which may hold other programs' files.
	ownTop
	// ownFile: only the file itself, created when missing (the access log).
	// Its folder can be /var/log, or /.
	ownFile
)

type ownedPath struct {
	path string
	mode ownMode
}

// ownedPaths are the paths the server writes to, which the dropped user must
// own.
func ownedPaths(cfg *config.Config) []ownedPath {
	var out []ownedPath
	add := func(p string, m ownMode) {
		if p != "" {
			out = append(out, ownedPath{p, m})
		}
	}
	add(cfg.Storage.DataDir, ownTree)
	add(cfg.Storage.MetadataDir, ownTreeAlways)
	if cfg.Cluster.Enabled {
		d := cfg.Cluster.DataDir
		if d == "" {
			d = "./raft-data" // cluster.Config's default
		}
		add(d, ownTree)
	}
	if cfg.Tiering.Enabled {
		add(cfg.Tiering.ColdDataDir, ownTop)
	}
	if cfg.Erasure.Enabled {
		for _, d := range cfg.Erasure.DataDirs {
			add(d, ownTop)
		}
	}
	if cfg.Backup.Enabled {
		for _, tg := range cfg.Backup.Targets {
			if tg.Type == "local" {
				add(tg.Path, ownTop)
			}
		}
	}
	if cfg.Vector.Enabled && cfg.Vector.PersistPath != "" {
		add(filepath.Dir(cfg.Vector.PersistPath), ownTop) // the default is inside metadata_dir
	}
	if cfg.Logging.Enabled && cfg.Logging.FilePath != "" {
		add(cfg.Logging.FilePath, ownFile)
	}
	return out
}

// takeOwnership gives p to uid:gid as mode says, creating it when missing,
// and reports whether a folder walk changed anything.
// A folder given whole is walked only when it has another owner (unless
// ownTreeAlways), so a large data folder is walked once, on the first start;
// it is changed last, so an interrupted walk is redone. A path that is a
// symbolic link is resolved first, and links inside a folder are changed
// themselves, never followed. The file system root is refused.
func takeOwnership(p string, mode ownMode, uid, gid int, owner func(os.FileInfo) (int, int, bool)) (walked bool, err error) {
	if mode == ownFile {
		return false, takeFile(p, uid, gid)
	}
	if err := os.MkdirAll(p, 0o750); err != nil {
		return false, err
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false, err
	}
	if real, err = filepath.Abs(real); err != nil {
		return false, err
	}
	if real == string(filepath.Separator) {
		return false, fmt.Errorf("%s is the file system root; refusing to give it away", p)
	}
	if sys := systemPath(real); sys != "" {
		return false, fmt.Errorf("%s is %s, a system folder; refusing to give it away", p, sys)
	}
	if mode == ownTop {
		return false, os.Lchown(real, uid, gid)
	}
	fi, err := os.Lstat(real)
	if err != nil {
		return false, err
	}
	if u, g, known := owner(fi); mode == ownTree && known && u == uid && g == gid {
		return false, nil
	}
	skippedLinks := false
	defer func() {
		if skippedLinks {
			slog.Warn("left files with more than one name as they are; give them to the PUID user by hand if the server needs them", "dir", p)
		}
	}()
	// Only entries with another owner are changed, and reported.
	foreign := func(fi os.FileInfo) bool {
		u, g, known := owner(fi)
		return !known || u != uid || g != gid
	}
	err = filepath.WalkDir(real, func(q string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if q == real {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !foreign(fi) {
			return nil
		}
		// A file with another name elsewhere may be a link to a file outside
		// the folder (a root file linked in), which this would give away.
		if !fi.IsDir() && linkCount(fi) > 1 {
			skippedLinks = true
			return nil
		}
		walked = true
		return os.Lchown(q, uid, gid)
	})
	if err != nil {
		return walked, err
	}
	if foreign(fi) {
		walked = true
		return walked, os.Lchown(real, uid, gid)
	}
	return walked, nil
}

// systemPath names the system folder p is or lies in, or "". A setting such
// as tiering.cold_data_dir can point anywhere, and the server, still root,
// would give that folder to the PUID user: pointing it at /usr/local/bin, by
// whoever can edit the config, gave that user the server's own binary and the
// next start ran their code as root.
func systemPath(p string) string {
	clean := filepath.Clean(p)
	for _, sys := range []string{"/bin", "/sbin", "/usr", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/boot", "/dev", "/proc", "/sys", "/root", "/run", "/var/run", "/var/lib/dpkg", "/var/lib/apk", "/var/spool"} {
		// A folder above one is refused too: given whole, /var would hand
		// over /var/lib/dpkg with it.
		if clean == sys || strings.HasPrefix(clean, sys+"/") || strings.HasPrefix(sys, clean+"/") {
			return sys
		}
	}
	// Folders that hold other software or other users' files, refused as
	// they are (a folder inside one, such as /opt/vaults3, is fine).
	for _, shared := range []string{"/opt", "/home", "/srv", "/mnt", "/media", "/tmp", "/var/tmp", "/var/lib", "/var/log"} {
		if clean == shared {
			return shared
		}
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			dir := filepath.Dir(real)
			if clean == dir || strings.HasPrefix(dir, clean+"/") || strings.HasPrefix(clean, dir+"/") {
				return "the server's own folder"
			}
		}
	}
	return ""
}
