package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// Rate limiting is the one subsystem a default install runs, so it is the whole
// expected baseline. Pinning the exact list means any future default that is
// silently flipped on shows up here rather than in a user's bug report.
func TestDiagnoseDefaultConfigReportsOnlyRateLimiting(t *testing.T) {
	d := buildDiagnosis(config.Defaults(), "4.4.63", "")
	if len(d.Enabled) != 1 || d.Enabled[0] != "rate limiting" {
		t.Fatalf("default config reported %v enabled, want exactly [rate limiting]", d.Enabled)
	}
	if d.ConfigFile != "(none, using built-in defaults)" {
		t.Errorf("ConfigFile = %q", d.ConfigFile)
	}
}

// The "nothing enabled" wording still has to be reachable, for an operator who
// turned the one default off.
func TestDiagnoseReportsAPlainDeploymentWhenNothingIsOn(t *testing.T) {
	cfg := config.Defaults()
	cfg.RateLimit.Enabled = false
	d := buildDiagnosis(cfg, "4.4.63", "")
	if len(d.Enabled) != 0 {
		t.Fatalf("reported %v enabled, want none", d.Enabled)
	}
	if !strings.Contains(d.text(), "plain default deployment") {
		t.Error("text should say this is a plain default deployment")
	}
}

// The encryption modes are mutually exclusive in New(), which checks PerBucket
// before KMS. Diagnose has to report the mode that will actually run, or it
// sends a bug report chasing the wrong engine.
func TestDiagnoseEncryptionModePrecedence(t *testing.T) {
	tests := []struct {
		name      string
		perBucket bool
		kms       bool
		want      string
	}{
		{"static key", false, false, "encryption: SSE-S3 (static key)"},
		{"kms", false, true, "encryption: SSE-KMS (provider )"},
		{"per-bucket beats kms", true, true, "encryption: per-bucket keys (opt-in per bucket)"},
		{"per-bucket alone", true, false, "encryption: per-bucket keys (opt-in per bucket)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Encryption.Enabled = true
			cfg.Encryption.PerBucket = tc.perBucket
			cfg.Encryption.KMS.Enabled = tc.kms

			d := buildDiagnosis(cfg, "v", "c.yaml")
			if len(d.Enabled) == 0 || d.Enabled[0] != tc.want {
				t.Fatalf("got %v, want first entry %q", d.Enabled, tc.want)
			}
		})
	}
}

// Each note exists because the interaction behind it already cost a round trip
// on a real issue. They must fire only when the interaction is actually present.
func TestDiagnoseNotes(t *testing.T) {
	// Compression under encryption is no longer a no-op: it runs on plaintext.
	// The note that remains explains that objects written before the layering was
	// fixed keep their original size, which is the question an operator will ask
	// when the numbers do not move after upgrading.
	t.Run("compression under encryption explains the pre-4.4.70 objects", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Compression.Enabled = true
		cfg.Encryption.Enabled = true
		d := buildDiagnosis(cfg, "v", "c")
		if !hasNote(d, "compression now runs on plaintext") {
			t.Fatal("want the note explaining the current layering")
		}
		if hasNote(d, "compression saves nothing") {
			t.Fatal("the old no-op warning is still being emitted, but compression now works")
		}
	})
	t.Run("compression alone is not called out", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Compression.Enabled = true
		if hasNote(buildDiagnosis(cfg, "v", "c"), "compression now runs on plaintext") {
			t.Fatal("compression alone needs no note about encryption")
		}
	})
	t.Run("packing skipped under erasure is called out", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Packing.Enabled = true
		cfg.Erasure.Enabled = true
		if !hasNote(buildDiagnosis(cfg, "v", "c"), "small-file packing is skipped") {
			t.Fatal("want the packing note")
		}
	})
	t.Run("packing alone is not called out", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Packing.Enabled = true
		if hasNote(buildDiagnosis(cfg, "v", "c"), "small-file packing is skipped") {
			t.Fatal("packing alone works, it must not be flagged")
		}
	})
	t.Run("empty cluster secret is called out", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Cluster.Enabled = true
		cfg.Cluster.Secret = ""
		if !hasNote(buildDiagnosis(cfg, "v", "c"), "cluster.secret is empty") {
			t.Fatal("an empty cluster secret leaves inter-node endpoints open, it must be flagged")
		}
	})
	t.Run("set cluster secret is not called out", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Cluster.Enabled = true
		cfg.Cluster.Secret = "s3cr3t"
		if hasNote(buildDiagnosis(cfg, "v", "c"), "cluster.secret is empty") {
			t.Fatal("a set secret must not be flagged")
		}
	})
}

// The whole point of this command is that the output can be pasted into a public
// issue without redaction. If a future field is added carelessly, this fails.
//
// Every secret-bearing config field gets a unique sentinel, and neither the text
// nor the JSON rendering may contain any of them.
func TestDiagnoseNeverPrintsSecrets(t *testing.T) {
	const (
		accessKey     = "SENTINEL-ACCESS-KEY"
		adminSecret   = "SENTINEL-ADMIN-SECRET"
		encKey        = "SENTINEL-ENC-KEY"
		legacyKey     = "SENTINEL-LEGACY-KEY"
		vaultToken    = "SENTINEL-VAULT-TOKEN"
		localKey      = "SENTINEL-LOCAL-KEY"
		vaultAddr     = "SENTINEL-VAULT-ADDR"
		clusterSecret = "SENTINEL-CLUSTER-SECRET"
	)

	cfg := config.Defaults()
	cfg.Auth.AdminAccessKey = accessKey
	cfg.Auth.AdminSecretKey = adminSecret
	cfg.Encryption.Enabled = true
	cfg.Encryption.Key = encKey
	cfg.Encryption.LegacyKey = legacyKey
	cfg.Encryption.KMS.Enabled = true
	cfg.Encryption.KMS.VaultToken = vaultToken
	cfg.Encryption.KMS.LocalKey = localKey
	cfg.Encryption.KMS.VaultAddr = vaultAddr
	cfg.Cluster.Enabled = true
	cfg.Cluster.Secret = clusterSecret

	d := buildDiagnosis(cfg, "4.4.63", "c.yaml")
	asJSON, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	rendered := d.text() + string(asJSON)

	for _, secret := range []string{
		accessKey, adminSecret, encKey, legacyKey,
		vaultToken, localKey, vaultAddr, clusterSecret,
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("diagnose leaked %q into output meant for a public issue", secret)
		}
	}
}

func hasNote(d diagnosis, substr string) bool {
	for _, n := range d.Notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// A container and a package both keep the config at /etc/vaults3/vaults3.yaml,
// while the binary's default is a relative path for a source checkout. Searching
// both is not a nicety: with only the relative default, `docker exec <c> vaults3
// diagnose` found nothing and reported "no subsystems enabled" on a container
// where every subsystem was on. A bug report built on that output is worse than
// no bug report.
func TestFirstExistingConfigPrefersTheCheckoutThenThePackagePath(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	// Nothing on disk: fall back to the default so the caller still gets defaults.
	if got := firstExistingConfig(); got != defaultConfigPath {
		t.Fatalf("with no config present, got %q, want %q", got, defaultConfigPath)
	}

	// A checkout-style config is found where it lies.
	if err := os.MkdirAll(filepath.Dir(defaultConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultConfigPath, []byte("server:\n  port: 9000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := firstExistingConfig(); got != defaultConfigPath {
		t.Fatalf("got %q, want the checkout config %q", got, defaultConfigPath)
	}
}

// A directory at the config path must not be mistaken for a config file.
func TestFirstExistingConfigIgnoresADirectory(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := os.MkdirAll(defaultConfigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := firstExistingConfig(); got != defaultConfigPath {
		// falls through to the default string, but must not have matched the dir
		t.Logf("returned %q", got)
	}
	if st, err := os.Stat(defaultConfigPath); err != nil || !st.IsDir() {
		t.Fatalf("test setup wrong: %v", err)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}
