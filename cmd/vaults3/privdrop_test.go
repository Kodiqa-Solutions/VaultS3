package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

func TestPrivDropTarget(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		m        map[string]string
		uid, gid int
		ok, err  bool
	}{
		{map[string]string{}, 0, 0, false, false},
		{map[string]string{"PUID": "99", "PGID": "100"}, 99, 100, true, false},
		{map[string]string{"PUID": "1001"}, 1001, 1001, true, false},
		{map[string]string{"PGID": "100"}, 1000, 100, true, false},
		{map[string]string{"PUID": "abc"}, 0, 0, false, true},
		{map[string]string{"PUID": "-1"}, 0, 0, false, true},
		{map[string]string{"PUID": "99", "PGID": "x"}, 0, 0, false, true},
	} {
		uid, gid, ok, err := privDropTarget(env(c.m))
		if uid != c.uid || gid != c.gid || ok != c.ok || (err != nil) != c.err {
			t.Errorf("%v: %d:%d ok=%v err=%v", c.m, uid, gid, ok, err)
		}
	}
}

// The tree is walked only when the folder has another owner; an owned
// folder is left alone, so a large data folder is not walked on every start.
func TestTakeOwnershipWalksOnlyAForeignFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	uid, gid := os.Getuid(), os.Getgid()
	owned := func(os.FileInfo) (int, int, bool) { return uid, gid, true }
	walked, err := takeOwnership(dir, ownTree, uid, gid, owned)
	if err != nil || walked {
		t.Fatalf("an owned folder: walked=%v err=%v", walked, err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatal("the folder was not created")
	}
	os.MkdirAll(filepath.Join(dir, "b", "c"), 0o755)
	os.WriteFile(filepath.Join(dir, "b", "c", "o"), []byte("x"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(dir, "link"))
	foreign := func(os.FileInfo) (int, int, bool) { return uid + 1, gid, true }
	// Chown to our own ids always succeeds, so this checks the walk runs,
	// reports a change, and never fails on a link.
	walked, err = takeOwnership(dir, ownTree, uid, gid, foreign)
	if err != nil || !walked {
		t.Fatalf("a foreign folder: walked=%v err=%v", walked, err)
	}
	// An always-walked folder whose entries all belong to the user reports
	// no change, so a restart does not log one.
	if walked, err := takeOwnership(dir, ownTreeAlways, uid, gid, owned); err != nil || walked {
		t.Fatalf("an owned folder walked again: changed=%v err=%v", walked, err)
	}
}

func TestTakeOwnershipRefusesTheRoot(t *testing.T) {
	never := func(os.FileInfo) (int, int, bool) { return -1, -1, true }
	for _, m := range []ownMode{ownTree, ownTreeAlways, ownTop} {
		if _, err := takeOwnership("/", m, os.Getuid(), os.Getgid(), never); err == nil {
			t.Errorf("mode %d: / was not refused", m)
		}
	}
}

func TestOwnedPathsCoverEveryWrittenPath(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.DataDir, cfg.Storage.MetadataDir = "/data", "/metadata"
	cfg.Cluster.Enabled = true
	cfg.Tiering.Enabled, cfg.Tiering.ColdDataDir = true, "/cold"
	cfg.Erasure.Enabled, cfg.Erasure.DataDirs = true, []string{"/d1", "/d2"}
	cfg.Backup.Enabled = true
	cfg.Backup.Targets = []config.BackupTarget{{Type: "local", Path: "/bk"}, {Type: "s3", Path: "ignored"}}
	cfg.Vector.Enabled, cfg.Vector.PersistPath = true, "/vec/index.gob"
	cfg.Logging.Enabled, cfg.Logging.FilePath = true, "/var/log/vaults3.log"
	got := map[string]ownMode{}
	for _, o := range ownedPaths(cfg) {
		got[o.path] = o.mode
	}
	want := map[string]ownMode{"/data": ownTree, "/metadata": ownTreeAlways, "./raft-data": ownTree,
		"/cold": ownTop, "/d1": ownTop, "/d2": ownTop, "/bk": ownTop, "/vec": ownTop, "/var/log/vaults3.log": ownFile}
	if len(got) != len(want) {
		t.Errorf("paths %v, want %v", got, want)
	}
	for p, m := range want {
		if g, ok := got[p]; !ok || g != m {
			t.Errorf("%s: mode %v present %v, want %v", p, g, ok, m)
		}
	}
}
