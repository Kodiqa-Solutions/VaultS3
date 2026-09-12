package config

import "testing"

// SSE-KMS takes its keys from the KMS, so a static `key` is meaningless there.
// Demanding one made the documented KMS configuration fail to start.
func TestKMSModeNeedsNoStaticKey(t *testing.T) {
	cfg, err := parse([]byte("encryption:\n  enabled: true\n  kms:\n    enabled: true\n    provider: \"local\"\n    local_key: \"aa\"\n"))
	if err != nil {
		t.Fatalf("KMS mode must start without a static key: %v", err)
	}
	if !cfg.Encryption.KMS.Enabled {
		t.Fatal("KMS should be enabled")
	}
	// Without KMS a missing or malformed key is still a refusal, because in that
	// mode the key is the only thing that encrypts anything.
	if _, err := parse([]byte("encryption:\n  enabled: true\n  key: \"\"\n")); err == nil {
		t.Fatal("static-key mode with no key must still be refused")
	}
}
