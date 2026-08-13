// Package bucketcrypto is a prototype of per-bucket envelope encryption for
// VaultS3 (see docs/design/per-bucket-encryption.md). A master KEK wraps a
// per-bucket, versioned DEK; objects are encrypted with their bucket's DEK using
// AES-256-GCM. Buckets without a key are pass-through (opt-out). This package is
// self-contained and not yet wired into the live storage path — it exists to
// prove the design (per-bucket isolation, opt-out, rotation, crypto-shredding).
package bucketcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strconv"
	"sync"
)

var (
	magic    = []byte("VS3X") // marks a per-bucket-encrypted blob
	formatV1 = byte(1)
)

// headerLen is magic(4) + format(1) + keyVersion(4).
const headerLen = 4 + 1 + 4

var (
	ErrShortKey  = errors.New("bucketcrypto: key must be 32 bytes")
	ErrNoKey     = errors.New("bucketcrypto: no key for bucket/version (opted out or shredded)")
	ErrMalformed = errors.New("bucketcrypto: malformed ciphertext")
	ErrBadFormat = errors.New("bucketcrypto: unsupported format version")
)

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrShortKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// KEK is the master key-encryption-key that wraps per-bucket DEKs. It never
// touches object data.
type KEK struct{ gcm cipher.AEAD }

// NewKEK builds a KEK from a 32-byte master key.
func NewKEK(masterKey []byte) (*KEK, error) {
	gcm, err := newGCM(masterKey)
	if err != nil {
		return nil, err
	}
	return &KEK{gcm: gcm}, nil
}

func (k *KEK) wrap(dek []byte) ([]byte, error) {
	nonce, err := randomBytes(k.gcm.NonceSize())
	if err != nil {
		return nil, err
	}
	return append(nonce, k.gcm.Seal(nil, nonce, dek, nil)...), nil
}

func (k *KEK) unwrap(wrapped []byte) ([]byte, error) {
	ns := k.gcm.NonceSize()
	if len(wrapped) < ns {
		return nil, ErrMalformed
	}
	return k.gcm.Open(nil, wrapped[:ns], wrapped[ns:], nil)
}

// KeyStore persists wrapped DEKs per bucket, keyed by version. In production this
// is backed by BucketEncryptionConfig in the metadata store; the prototype ships
// an in-memory implementation.
type KeyStore interface {
	Current(bucket string) (version int, wrapped []byte, ok bool)
	Get(bucket string, version int) (wrapped []byte, ok bool)
	SetCurrent(bucket string, version int, wrapped []byte) error
	Delete(bucket string) error // shred all versions for a bucket
}

// MemKeyStore is an in-memory KeyStore for the prototype + tests.
type MemKeyStore struct {
	mu       sync.RWMutex
	current  map[string]int
	versions map[string]map[int][]byte
}

func NewMemKeyStore() *MemKeyStore {
	return &MemKeyStore{current: map[string]int{}, versions: map[string]map[int][]byte{}}
}

func (s *MemKeyStore) Current(bucket string) (int, []byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.current[bucket]
	if !ok {
		return 0, nil, false
	}
	return v, s.versions[bucket][v], true
}

func (s *MemKeyStore) Get(bucket string, version int) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.versions[bucket][version]
	return w, ok
}

func (s *MemKeyStore) SetCurrent(bucket string, version int, wrapped []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[bucket] == nil {
		s.versions[bucket] = map[int][]byte{}
	}
	s.versions[bucket][version] = wrapped
	s.current[bucket] = version
	return nil
}

func (s *MemKeyStore) Delete(bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.current, bucket)
	delete(s.versions, bucket)
	return nil
}

// Manager generates/resolves per-bucket keys and encrypts/decrypts objects.
type Manager struct {
	kek   *KEK
	keys  KeyStore
	mu    sync.RWMutex
	cache map[string][]byte // bucket\x00version -> unwrapped DEK
}

func NewManager(kek *KEK, keys KeyStore) *Manager {
	return &Manager{kek: kek, keys: keys, cache: map[string][]byte{}}
}

func cacheKey(bucket string, version int) string {
	return bucket + "\x00" + strconv.Itoa(version)
}

func (m *Manager) cacheGet(bucket string, version int) []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cache[cacheKey(bucket, version)]
}

func (m *Manager) cachePut(bucket string, version int, dek []byte) {
	m.mu.Lock()
	m.cache[cacheKey(bucket, version)] = dek
	m.mu.Unlock()
}

func (m *Manager) cacheEvict(bucket string) {
	m.mu.Lock()
	for k := range m.cache {
		if len(k) >= len(bucket)+1 && k[:len(bucket)] == bucket && k[len(bucket)] == 0 {
			delete(m.cache, k)
		}
	}
	m.mu.Unlock()
}

// IsEncrypted reports whether a bucket has a key (i.e. opted in).
func (m *Manager) IsEncrypted(bucket string) bool {
	_, _, ok := m.keys.Current(bucket)
	return ok
}

// HasHeader reports whether a blob carries the per-bucket encryption header (so a
// reader can tell per-bucket ciphertext apart from legacy/plaintext data without
// trying to decrypt).
func HasHeader(data []byte) bool {
	return len(data) >= len(magic) && string(data[:len(magic)]) == string(magic)
}

// EnableBucket gives a bucket a fresh DEK (v1) if it has none.
func (m *Manager) EnableBucket(bucket string) error {
	if _, _, ok := m.keys.Current(bucket); ok {
		return nil
	}
	return m.newVersion(bucket, 1)
}

// Rotate generates the next DEK version, retaining old versions so existing
// objects still decrypt.
func (m *Manager) Rotate(bucket string) error {
	v, _, ok := m.keys.Current(bucket)
	if !ok {
		return m.newVersion(bucket, 1)
	}
	return m.newVersion(bucket, v+1)
}

func (m *Manager) newVersion(bucket string, version int) error {
	dek, err := randomBytes(32)
	if err != nil {
		return err
	}
	wrapped, err := m.kek.wrap(dek)
	if err != nil {
		return err
	}
	if err := m.keys.SetCurrent(bucket, version, wrapped); err != nil {
		return err
	}
	m.cachePut(bucket, version, dek)
	return nil
}

// ShredBucket deletes every key version for a bucket. Its ciphertext becomes
// permanently unrecoverable (crypto-shredding).
func (m *Manager) ShredBucket(bucket string) error {
	m.cacheEvict(bucket)
	return m.keys.Delete(bucket)
}

func (m *Manager) dek(bucket string, version int) ([]byte, error) {
	if d := m.cacheGet(bucket, version); d != nil {
		return d, nil
	}
	wrapped, ok := m.keys.Get(bucket, version)
	if !ok {
		return nil, ErrNoKey
	}
	d, err := m.kek.unwrap(wrapped)
	if err != nil {
		return nil, err
	}
	m.cachePut(bucket, version, d)
	return d, nil
}

// CurrentKey returns the bucket's current data key and its version, so a caller
// that encrypts a stream rather than a buffer can seal chunk by chunk with the
// same key this package would have used. ok is false for an opted-out bucket,
// which is stored as plaintext.
//
// The key stays inside the process (it is the same unwrapped DEK the cache
// already holds); handing it out avoids forcing every object through a
// whole-object Encrypt, which is what made encrypted reads scale with object
// size (issue #49).
func (m *Manager) CurrentKey(bucket string) (key []byte, version int, ok bool, err error) {
	version, _, ok = m.keys.Current(bucket)
	if !ok {
		return nil, 0, false, nil
	}
	dek, err := m.dek(bucket, version)
	if err != nil {
		return nil, 0, false, err
	}
	return dek, version, true, nil
}

// KeyForVersion returns the data key for a specific key version, which a reader
// takes from the blob's header. A shredded or unknown version yields ErrNoKey.
func (m *Manager) KeyForVersion(bucket string, version int) ([]byte, error) {
	return m.dek(bucket, version)
}

// Encrypt encrypts plaintext for a bucket. Buckets without a key are pass-through:
// the plaintext is returned unchanged and encrypted=false.
func (m *Manager) Encrypt(bucket string, plaintext []byte) (out []byte, encrypted bool, err error) {
	version, _, ok := m.keys.Current(bucket)
	if !ok {
		return plaintext, false, nil
	}
	dek, err := m.dek(bucket, version)
	if err != nil {
		return nil, false, err
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, false, err
	}
	nonce, err := randomBytes(gcm.NonceSize())
	if err != nil {
		return nil, false, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)

	out = make([]byte, 0, headerLen+len(nonce)+len(ct))
	out = append(out, magic...)
	out = append(out, formatV1)
	out = binary.BigEndian.AppendUint32(out, uint32(version))
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, true, nil
}

// Decrypt reverses Encrypt. Blobs without the magic header are returned unchanged
// (plaintext / opt-out / legacy handled upstream).
func (m *Manager) Decrypt(bucket string, data []byte) ([]byte, error) {
	if len(data) < 4 || string(data[:4]) != string(magic) {
		return data, nil
	}
	if len(data) < headerLen {
		return nil, ErrMalformed
	}
	if data[4] != formatV1 {
		return nil, ErrBadFormat
	}
	version := int(binary.BigEndian.Uint32(data[5:9]))
	dek, err := m.dek(bucket, version)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < headerLen+ns {
		return nil, ErrMalformed
	}
	nonce := data[headerLen : headerLen+ns]
	ct := data[headerLen+ns:]
	return gcm.Open(nil, nonce, ct, nil)
}
