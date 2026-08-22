package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
)

// First-run credentials (issue #51).
//
// A server that is told no admin secret used to fall back to one printed in this
// repository, so anyone who downloaded the binary, started it and exposed port
// 9000 was running a server whose password is public. A warning in the log is
// not a control: the people most likely to miss it are exactly the people who
// never set a secret.
//
// So a server with nothing configured and nothing persisted now mints its own
// secret, stores it, and prints it once. Everything else is left alone: an
// explicitly configured secret is honoured, and a store that already holds
// credentials keeps them, which is what makes this safe to upgrade into.

// defaultAdminAccessKey is the access key a first run gets when none is set. It
// is not a secret, only a username, so a predictable one is fine and makes the
// docs match what people see.
const defaultAdminAccessKey = "vaults3-admin"

// publishedPlaceholderSecret is the value shipped in the sample config and the
// docs. It is not a password, and a server still running it deserves to be told
// so on every start.
const publishedPlaceholderSecret = "vaults3-secret-change-me"

// adminCredentialSource says where the credentials in use came from, so the
// caller can report a first run without repeating the decision.
type adminCredentialSource int

const (
	adminCredsFromConfig adminCredentialSource = iota
	adminCredsFromStore
	adminCredsGenerated
)

// resolveAdminCredentials decides the admin credentials for this start and
// applies them, in a fixed order of authority:
//
//  1. credentials already persisted in the metadata store, which is where a
//     change made through the dashboard lands and where a previous first run
//     saved what it generated;
//  2. whatever the config or environment sets;
//  3. a freshly generated secret, persisted so the next start reuses it.
func resolveAdminCredentials(cfg *config.Config, store *metadata.Store, auth *s3.Authenticator) (adminCredentialSource, error) {
	if ak, sk, err := store.GetAdminCredentials(); err == nil && ak != "" && sk != "" {
		cfg.Auth.AdminAccessKey = ak
		cfg.Auth.AdminSecretKey = sk
		auth.UpdateAdminCredentials(ak, sk)
		return adminCredsFromStore, nil
	}

	if cfg.Auth.AdminSecretKey != "" {
		if cfg.Auth.AdminAccessKey == "" {
			cfg.Auth.AdminAccessKey = defaultAdminAccessKey
			auth.UpdateAdminCredentials(cfg.Auth.AdminAccessKey, cfg.Auth.AdminSecretKey)
		}
		return adminCredsFromConfig, nil
	}

	secret, err := randomSecret(24)
	if err != nil {
		return adminCredsFromConfig, fmt.Errorf("generate admin secret: %w", err)
	}
	if cfg.Auth.AdminAccessKey == "" {
		cfg.Auth.AdminAccessKey = defaultAdminAccessKey
	}
	cfg.Auth.AdminSecretKey = secret
	auth.UpdateAdminCredentials(cfg.Auth.AdminAccessKey, secret)
	// Persist before announcing it: a secret shown to an operator that the next
	// start would not accept is worse than no secret at all.
	if err := store.SetAdminCredentials(cfg.Auth.AdminAccessKey, secret); err != nil {
		return adminCredsFromConfig, fmt.Errorf("save generated admin credentials: %w", err)
	}
	return adminCredsGenerated, nil
}

// randomSecret returns n bytes of crypto/rand as hex.
func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// announceAdminCredentials prints the credentials a first run generated, once,
// where an operator will actually see them. They are not written again: from the
// next start they live only in the metadata store.
func announceAdminCredentials(accessKey, secretKey, dashboard string) {
	fmt.Printf(`
──────────────────────────────────────────────────────────────
 VaultS3 generated an admin secret for this new installation.
 It is shown once. Store it somewhere safe.

   Access key:  %s
   Secret key:  %s

   Dashboard:   %s

 Change it from the dashboard, or set VAULTS3_ACCESS_KEY and
 VAULTS3_SECRET_KEY to credentials of your own.
──────────────────────────────────────────────────────────────

`, accessKey, secretKey, dashboard)
}

// warnPlaceholderSecret complains about the sample secret from the docs.
func warnPlaceholderSecret(secret string) {
	if secret == publishedPlaceholderSecret {
		slog.Warn("this server is using the example admin secret from the VaultS3 documentation, which is public; " +
			"set VAULTS3_SECRET_KEY, or clear admin_secret_key to have one generated")
	}
}

// displayHost turns a bind address into one someone can paste into a browser.
// A wildcard bind is what a server listens on, not somewhere a client can go,
// and printing "http://0.0.0.0:9000/dashboard/" as the first thing a new user
// sees invites them to try exactly that.
func displayHost(addr string) string {
	host, port, ok := splitHostPort(addr)
	if !ok {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1:" + port
	}
	return addr
}

func splitHostPort(addr string) (host, port string, ok bool) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}
