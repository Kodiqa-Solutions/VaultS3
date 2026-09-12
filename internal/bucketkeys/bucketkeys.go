// Package bucketkeys wires the per-bucket envelope-encryption core
// (internal/bucketcrypto) to the metadata store: per-bucket wrapped data keys are
// persisted in each bucket's BucketEncryptionConfig. See
// docs/design/per-bucket-encryption.md (phase 2).
package bucketkeys

import (
	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// storeKeyStore adapts the metadata store's BucketEncryptionConfig to a
// bucketcrypto.KeyStore. Read-modify-write preserves the bucket's other
// encryption settings (SSE algorithm, KMS key id).
type storeKeyStore struct{ store metadata.StoreAPI }

func (s storeKeyStore) load(bucket string) *metadata.BucketEncryptionConfig {
	cfg, err := s.store.GetEncryptionConfig(bucket)
	if err != nil || cfg == nil {
		return &metadata.BucketEncryptionConfig{}
	}
	return cfg
}

func (s storeKeyStore) Current(bucket string) (int, []byte, bool) {
	cfg := s.load(bucket)
	if cfg.KeyVersion == 0 {
		return 0, nil, false
	}
	return cfg.KeyVersion, cfg.WrappedDEKs[cfg.KeyVersion], true
}

// EncryptionPending reports a bucket whose config asks for encryption while no
// key version has reached this node. Enabling encryption writes the algorithm
// and the key as two separate Raft entries, so a node can hold the first without
// the second, and until this existed that state was indistinguishable from a
// bucket that had opted out: the write path took the plaintext branch and the
// object stayed readable on disk in a bucket that had just been told to encrypt.
func (s storeKeyStore) EncryptionPending(bucket string) bool {
	cfg := s.load(bucket)
	// Only the per-bucket AES256 path provisions a key here. SSE-KMS keeps its
	// key in the KMS and never sets a version, so treating it as pending would
	// refuse every write to a KMS bucket for as long as it existed.
	return cfg.SSEAlgorithm == "AES256" && cfg.KeyVersion == 0
}

func (s storeKeyStore) Get(bucket string, version int) ([]byte, bool) {
	cfg := s.load(bucket)
	w, ok := cfg.WrappedDEKs[version]
	return w, ok
}

func (s storeKeyStore) SetCurrent(bucket string, version int, wrapped []byte) error {
	cfg := s.load(bucket)
	if cfg.WrappedDEKs == nil {
		cfg.WrappedDEKs = map[int][]byte{}
	}
	cfg.WrappedDEKs[version] = wrapped
	cfg.KeyVersion = version
	if cfg.SSEAlgorithm == "" {
		cfg.SSEAlgorithm = "AES256"
	}
	return s.store.PutEncryptionConfig(bucket, *cfg)
}

// Delete crypto-shreds: it clears the key material from the bucket's config while
// leaving the row, so the ciphertext is unrecoverable.
func (s storeKeyStore) Delete(bucket string) error {
	cfg := s.load(bucket)
	cfg.KeyVersion = 0
	cfg.WrappedDEKs = nil
	return s.store.PutEncryptionConfig(bucket, *cfg)
}

// NewManager builds a per-bucket key manager backed by the metadata store, using
// masterKey as the KEK. Returns (nil, nil) when masterKey is empty — per-bucket
// encryption is simply unavailable, and callers fall back to existing behavior.
func NewManager(store metadata.StoreAPI, masterKey []byte) (*bucketcrypto.Manager, error) {
	if len(masterKey) == 0 {
		return nil, nil
	}
	kek, err := bucketcrypto.NewKEK(masterKey)
	if err != nil {
		return nil, err
	}
	return bucketcrypto.NewManager(kek, storeKeyStore{store: store}), nil
}
