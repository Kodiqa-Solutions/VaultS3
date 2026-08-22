package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// setup has to produce a file the server actually accepts. Writing YAML by hand
// means nothing checks it against the config type, so this loads the result the
// way the server does and compares the answers back.
func TestSetupWritesAConfigTheServerCanLoad(t *testing.T) {
	dir := t.TempDir()
	a := setupAnswers{
		configPath:  filepath.Join(dir, "vaults3.yaml"),
		dataDir:     filepath.Join(dir, "data"),
		metadataDir: filepath.Join(dir, "metadata"),
		accessLog:   filepath.Join(dir, "logs", "access.log"),
		accessKey:   "admin-key",
		buckets:     []string{"local", "scratch"},
		address:     "127.0.0.1",
		port:        9010,
	}
	if err := applySetup(&a, false); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cfg, err := config.Load(a.configPath)
	if err != nil {
		t.Fatalf("the config setup wrote does not load: %v", err)
	}
	if cfg.Server.Address != "127.0.0.1" || cfg.Server.Port != 9010 {
		t.Fatalf("server block round-tripped as %s:%d", cfg.Server.Address, cfg.Server.Port)
	}
	if cfg.Storage.DataDir != a.dataDir || cfg.Storage.MetadataDir != a.metadataDir {
		t.Fatalf("storage block round-tripped as %+v", cfg.Storage)
	}
	if cfg.Auth.AdminAccessKey != "admin-key" {
		t.Fatalf("access key round-tripped as %q", cfg.Auth.AdminAccessKey)
	}
	if strings.Join(cfg.Storage.DefaultBuckets, ",") != "local,scratch" {
		t.Fatalf("default buckets round-tripped as %v", cfg.Storage.DefaultBuckets)
	}

	// The secret must be generated, not left empty and not the published one.
	if len(cfg.Auth.AdminSecretKey) < 32 {
		t.Fatalf("generated secret is too short to be one: %q", cfg.Auth.AdminSecretKey)
	}
	if cfg.Auth.AdminSecretKey == "vaults3-secret-change-me" {
		t.Fatal("setup wrote the example secret from the docs")
	}

	// It holds a credential, so it must not be world readable.
	info, err := os.Stat(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config written with mode %o, want 600: it contains the admin secret", perm)
	}

	// The directories it named have to exist, since that is half of what a
	// first-time user was doing by hand.
	for _, d := range []string{a.dataDir, a.metadataDir, filepath.Dir(a.accessLog)} {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			t.Fatalf("%s was not created", d)
		}
	}
}

// Two runs must not produce two different secrets for one installation, and the
// second must not quietly replace a config the operator is running.
func TestSetupRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	a := setupAnswers{
		configPath:  filepath.Join(dir, "vaults3.yaml"),
		dataDir:     filepath.Join(dir, "data"),
		metadataDir: filepath.Join(dir, "metadata"),
		accessLog:   filepath.Join(dir, "logs", "access.log"),
		accessKey:   "admin-key",
		address:     "127.0.0.1",
		port:        9000,
	}
	if err := applySetup(&a, false); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}

	again := a
	again.secretKey = ""
	if err := applySetup(&again, false); err == nil {
		t.Fatal("a second setup overwrote the existing config without --force")
	}
	after, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(first) {
		t.Fatal("the refused run still changed the file")
	}

	// --force is the deliberate way through.
	forced := a
	forced.secretKey = ""
	forced.force = true
	if err := applySetup(&forced, false); err != nil {
		t.Fatalf("--force did not overwrite: %v", err)
	}
	if forced.secretKey == "" {
		t.Fatal("the forced run did not generate a new secret")
	}
}

// A port typed wrong should stop setup rather than write a config that cannot
// serve.
func TestSetupRejectsAnUnusablePort(t *testing.T) {
	dir := t.TempDir()
	a := setupAnswers{
		configPath:  filepath.Join(dir, "vaults3.yaml"),
		dataDir:     filepath.Join(dir, "data"),
		metadataDir: filepath.Join(dir, "metadata"),
		accessLog:   filepath.Join(dir, "access.log"),
		accessKey:   "admin-key",
		address:     "127.0.0.1",
		port:        70000,
	}
	if err := applySetup(&a, false); err == nil {
		t.Fatal("setup accepted port 70000")
	}
	if _, err := os.Stat(a.configPath); err == nil {
		t.Fatal("setup wrote a config despite refusing the port")
	}
}

// Nothing optional may appear in the file. An operator reading it should see the
// settings they chose, not a wall of disabled subsystems, and a feature nobody
// asked for must not be switched on by a setup wizard.
func TestSetupWritesNoOptionalSubsystems(t *testing.T) {
	a := setupAnswers{
		configPath: "vaults3.yaml", dataDir: "./data", metadataDir: "./metadata",
		accessLog: "./logs/access.log", accessKey: "k", secretKey: "s",
		address: "127.0.0.1", port: 9000,
	}
	out := renderConfig(&a)
	for _, forbidden := range []string{"cluster:", "encryption:", "replication:", "erasure:", "tiering:", "backup:", "lambda:", "vector:"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("generated config mentions %q, which nobody asked for", forbidden)
		}
	}
}
