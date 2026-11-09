package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

func TestColdDirMustBeSeparateFromTheDataDir(t *testing.T) {
	for _, c := range []struct {
		data, cold string
		ok         bool
	}{
		{"/srv/data", "/srv/cold", true},
		{"/srv/data", "/srv/data-cold", true},
		{"./data", "./cold_data", true},
		{"/srv/data", "/srv/data", false},
		{"/srv/data", "/srv/data/cold", false},
		{"/srv/data", "/srv/data/.cold/x", false},
		{"/srv/data/", "/srv/data/cold", false},
		{"/srv/cold/data", "/srv/cold", false},
		{"./data", "data/cold", false},
	} {
		err := checkColdDirSeparate(c.data, c.cold)
		if (err == nil) != c.ok {
			t.Errorf("data %q cold %q: err %v, want ok=%v", c.data, c.cold, err, c.ok)
		}
	}
}

// A cold directory reached through a symbolic link into the data directory
// is inside it.
func TestColdDirThroughASymlinkIntoTheDataDir(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	os.MkdirAll(filepath.Join(data, "cold"), 0o755)
	if err := os.Symlink(filepath.Join(data, "cold"), filepath.Join(dir, "coldlink")); err != nil {
		t.Skip(err)
	}
	if err := checkColdDirSeparate(data, filepath.Join(dir, "coldlink")); err == nil {
		t.Error("a symlink into the data dir passed")
	}
	if err := checkColdDirSeparate(data, filepath.Join(dir, "coldlink", "not-yet")); err == nil {
		t.Error("a missing dir under a symlink into the data dir passed")
	}
	if err := checkColdDirSeparate(data, filepath.Join(dir, "elsewhere")); err != nil {
		t.Errorf("a separate dir was refused: %v", err)
	}
}

// The highest-version-ID rule is on only for active-active without a
// cluster, decided from the config before any store opens.
func TestVersionOrderingOnlyForActiveActiveWithoutACluster(t *testing.T) {
	cfg := func(mode string, peers int, cluster bool) *config.Config {
		c := &config.Config{}
		c.Replication.Enabled = true
		c.Replication.Mode = mode
		for range peers {
			c.Replication.Peers = append(c.Replication.Peers, config.ReplicationPeer{Name: "p"})
		}
		c.Cluster.Enabled = cluster
		return c
	}
	for _, c := range []struct {
		c    *config.Config
		want bool
	}{
		{cfg("active-active", 1, false), true},
		{cfg("active-active", 1, true), false},
		{cfg("active-active", 0, false), false},
		{cfg("push", 1, false), false},
		{&config.Config{}, false},
	} {
		if got := versionOrderingFor(c.c); got != c.want {
			t.Errorf("mode %q peers %d cluster %v: %v, want %v", c.c.Replication.Mode, len(c.c.Replication.Peers), c.c.Cluster.Enabled, got, c.want)
		}
	}
}
