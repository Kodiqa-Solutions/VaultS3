package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vault provider failed at startup with a hex decoding error that pointed
// nowhere near the cause. It must be refused with a message that names it.
func TestVaultKMSProviderIsRefusedClearly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("encryption:\n  enabled: true\n  kms:\n    enabled: true\n    provider: vault\n    vault_addr: http://v:8200\n"), 0600)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "Vault KMS provider is not supported") {
		t.Fatalf("want a clear refusal, got %v", err)
	}
}
