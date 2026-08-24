package server

import (
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
)

// Compression wraps first, so encryption is the OUTER engine and the compressor
// is handed ciphertext. That saves nothing (measured 1.00x) while still costing
// CPU, and the server used to log a plain "compression enabled" either way, so an
// operator had no way to learn it from the running server. These pin which
// combinations must warn and which must not.
func TestCompressionDefeatedBy(t *testing.T) {
	tests := []struct {
		name      string
		compress  bool
		encrypt   bool
		perBucket bool
		kms       bool
		wantBy    string
		wantScope string
	}{
		{name: "compression alone compresses", compress: true},
		{name: "encryption alone says nothing", encrypt: true},
		{name: "neither says nothing"},
		{
			name:     "SSE-S3 defeats compression for everything",
			compress: true, encrypt: true,
			wantBy: "SSE-S3 encryption", wantScope: "any object",
		},
		{
			name:     "SSE-KMS defeats compression for everything",
			compress: true, encrypt: true, kms: true,
			wantBy: "SSE-KMS encryption", wantScope: "any object",
		},
		{
			// Not total: an opted-out bucket is stored as plaintext and still
			// compresses, so the warning must not claim every object.
			name:     "per-bucket defeats compression only where opted in",
			compress: true, encrypt: true, perBucket: true,
			wantBy: "per-bucket encryption", wantScope: "objects in buckets that opted into encryption",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Compression.Enabled = tc.compress
			cfg.Encryption.Enabled = tc.encrypt
			cfg.Encryption.PerBucket = tc.perBucket
			cfg.Encryption.KMS.Enabled = tc.kms

			by, scope := compressionDefeatedBy(cfg)
			if by != tc.wantBy {
				t.Errorf("by = %q, want %q", by, tc.wantBy)
			}
			if scope != tc.wantScope {
				t.Errorf("scope = %q, want %q", scope, tc.wantScope)
			}
		})
	}
}

// per-bucket must win over kms when both flags are set, because New() checks
// PerBucket first and builds a PerBucketEngine, not a KMSEncryptedEngine.
func TestCompressionDefeatedByPerBucketWinsOverKMS(t *testing.T) {
	cfg := config.Defaults()
	cfg.Compression.Enabled = true
	cfg.Encryption.Enabled = true
	cfg.Encryption.PerBucket = true
	cfg.Encryption.KMS.Enabled = true

	if by, _ := compressionDefeatedBy(cfg); by != "per-bucket encryption" {
		t.Fatalf("by = %q, want per-bucket encryption (New checks PerBucket first)", by)
	}
}
