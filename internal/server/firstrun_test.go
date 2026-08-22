package server

import (
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
)

func newCredTestStore(t *testing.T) *metadata.Store {
	t.Helper()
	store, err := metadata.NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newCredTestAuth(cfg *config.Config, store *metadata.Store) *s3.Authenticator {
	return s3.NewAuthenticator(cfg.Auth.AdminAccessKey, cfg.Auth.AdminSecretKey, store, nil, nil)
}

// A server told no secret used to fall back to the one printed in the repo, so
// anyone who downloaded the binary and exposed port 9000 was running a server
// whose password is public (issue #51). It must mint its own instead.
func TestFirstRunGeneratesAnAdminSecret(t *testing.T) {
	store := newCredTestStore(t)
	cfg := config.Defaults()
	cfg.Auth.AdminSecretKey = ""
	auth := newCredTestAuth(cfg, store)

	source, err := resolveAdminCredentials(cfg, store, auth)
	if err != nil {
		t.Fatal(err)
	}
	if source != adminCredsGenerated {
		t.Fatalf("credential source %v, want generated", source)
	}
	if len(cfg.Auth.AdminSecretKey) < 32 {
		t.Fatalf("generated secret is too short to be one: %q", cfg.Auth.AdminSecretKey)
	}
	if cfg.Auth.AdminSecretKey == publishedPlaceholderSecret {
		t.Fatal("the generated secret is the published placeholder")
	}
	if cfg.Auth.AdminAccessKey == "" {
		t.Fatal("no access key was chosen alongside the generated secret")
	}

	// It must be persisted, or the operator is shown a secret the next start
	// would not accept.
	ak, sk, err := store.GetAdminCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if ak != cfg.Auth.AdminAccessKey || sk != cfg.Auth.AdminSecretKey {
		t.Fatalf("store holds %q/%q, server is using %q/%q", ak, sk, cfg.Auth.AdminAccessKey, cfg.Auth.AdminSecretKey)
	}
}

// The second start must reuse the first start's secret. Generating a new one
// every boot would lock the operator out of their own installation.
func TestGeneratedSecretIsReusedOnTheNextStart(t *testing.T) {
	store := newCredTestStore(t)
	cfg := config.Defaults()
	cfg.Auth.AdminSecretKey = ""
	if _, err := resolveAdminCredentials(cfg, store, newCredTestAuth(cfg, store)); err != nil {
		t.Fatal(err)
	}
	first := cfg.Auth.AdminSecretKey

	next := config.Defaults()
	next.Auth.AdminSecretKey = ""
	source, err := resolveAdminCredentials(next, store, newCredTestAuth(next, store))
	if err != nil {
		t.Fatal(err)
	}
	if source != adminCredsFromStore {
		t.Fatalf("second start used %v, want the persisted credentials", source)
	}
	if next.Auth.AdminSecretKey != first {
		t.Fatal("the second start minted a different secret, locking out the first one")
	}
}

// An operator who set a secret keeps it. Generating over a configured value
// would be a silent lockout on upgrade.
func TestConfiguredSecretIsNotReplaced(t *testing.T) {
	store := newCredTestStore(t)
	cfg := config.Defaults()
	cfg.Auth.AdminAccessKey = "my-key"
	cfg.Auth.AdminSecretKey = "my-own-secret-value"

	source, err := resolveAdminCredentials(cfg, store, newCredTestAuth(cfg, store))
	if err != nil {
		t.Fatal(err)
	}
	if source != adminCredsFromConfig {
		t.Fatalf("credential source %v, want config", source)
	}
	if cfg.Auth.AdminSecretKey != "my-own-secret-value" {
		t.Fatalf("configured secret was replaced with %q", cfg.Auth.AdminSecretKey)
	}
}

// Credentials already in the store win over the config, which is what a
// dashboard password change relies on and what keeps an existing installation
// working across this change.
func TestPersistedCredentialsBeatTheConfig(t *testing.T) {
	store := newCredTestStore(t)
	if err := store.SetAdminCredentials("stored-key", "stored-secret"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Auth.AdminAccessKey = "config-key"
	cfg.Auth.AdminSecretKey = "config-secret"

	source, err := resolveAdminCredentials(cfg, store, newCredTestAuth(cfg, store))
	if err != nil {
		t.Fatal(err)
	}
	if source != adminCredsFromStore {
		t.Fatalf("credential source %v, want store", source)
	}
	if cfg.Auth.AdminSecretKey != "stored-secret" || cfg.Auth.AdminAccessKey != "stored-key" {
		t.Fatalf("config won over the store: %q/%q", cfg.Auth.AdminAccessKey, cfg.Auth.AdminSecretKey)
	}
}

// The first thing a new user sees must be a URL they can open. A wildcard bind
// is where the server listens, not somewhere a browser can go.
func TestDashboardURLIsReachable(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:9000":   "127.0.0.1:9000",
		"[::]:9000":      "127.0.0.1:9000",
		"127.0.0.1:9000": "127.0.0.1:9000",
		"10.1.2.3:9000":  "10.1.2.3:9000",
	}
	for in, want := range cases {
		if got := displayHost(in); got != want {
			t.Errorf("displayHost(%q) = %q, want %q", in, got, want)
		}
	}
}
