package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The release tarball ships binaries, README and LICENSE, and the release notes
// say to run ./vaults3. A missing config at the DEFAULT path must therefore be
// an ordinary first run, not a fatal error, or the documented install cannot
// work (issue #51).
func TestLoadOrDefaultsStartsWithoutAConfigFile(t *testing.T) {
	cfg, usedDefaults, err := LoadOrDefaults(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("a missing default config was fatal: %v", err)
	}
	if !usedDefaults {
		t.Fatal("defaults were used but not reported, so nothing can tell the user")
	}
	if cfg.Server.Port != 9000 || cfg.Storage.DataDir == "" || cfg.Storage.MetadataDir == "" {
		t.Fatalf("defaults are not a usable server: %+v", cfg.Server)
	}
}

// A path the operator named is different: a typo must fail rather than start a
// server on settings nobody chose.
func TestLoadRejectsAMissingNamedConfig(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "typo.yaml")); err == nil {
		t.Fatal("Load accepted a missing file")
	}
}

// When the file is there it still wins over the defaults.
func TestLoadOrDefaultsReadsTheFileWhenPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaults3.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: 9123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, usedDefaults, err := LoadOrDefaults(path)
	if err != nil {
		t.Fatal(err)
	}
	if usedDefaults {
		t.Fatal("reported defaults although the file exists")
	}
	if cfg.Server.Port != 9123 {
		t.Fatalf("port %d, want the one in the file", cfg.Server.Port)
	}
	// Anything the file omits still comes from the defaults.
	if cfg.Storage.DataDir == "" {
		t.Fatal("a partial file dropped the defaults for everything it did not set")
	}
}

// Defaults must not enable anything optional: a server told nothing runs as a
// plain single node.
func TestDefaultsEnableNothingOptional(t *testing.T) {
	cfg := Defaults()
	if cfg.Cluster.Enabled {
		t.Error("clustering is on by default")
	}
	if cfg.Encryption.Enabled {
		t.Error("encryption at rest is on by default")
	}
	if cfg.Erasure.Enabled {
		t.Error("erasure coding is on by default")
	}
	if cfg.Auth.AdminSecretKey != "" {
		t.Errorf("defaults ship an admin secret (%q); it must be generated per installation", cfg.Auth.AdminSecretKey)
	}
}
