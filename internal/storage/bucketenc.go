package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
	"sync"

	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
)

// PerBucketEngine encrypts/decrypts objects with a per-bucket data key resolved
// from a bucketcrypto.Manager (see docs/design/per-bucket-encryption.md, phase 3).
// Buckets without a key are stored as plaintext (opt-out). Objects written before
// per-bucket mode — which lack the per-bucket header — are read with an optional
// legacy global key, or as plaintext when none is configured.
//
// All non-crypto Engine methods delegate to the embedded inner Engine.
type PerBucketEngine struct {
	Engine              // inner engine; promoted methods delegate by default
	mu     sync.RWMutex // guards mgr (set after construction, before serving)
	mgr    *bucketcrypto.Manager
	legacy cipher.AEAD // optional: decrypt legacy global-key objects
	// legacyKey is the same key as legacy, kept in raw form because the streaming
	// format derives a per-chunk AEAD rather than reusing one.
	legacyKey []byte
}

// NewPerBucketEngine wraps inner. legacyKey (32 bytes) is optional and only used
// to read objects written by the old server-wide encryption.
func NewPerBucketEngine(inner Engine, legacyKey []byte) (*PerBucketEngine, error) {
	pe := &PerBucketEngine{Engine: inner}
	if len(legacyKey) > 0 {
		if len(legacyKey) != 32 {
			return nil, fmt.Errorf("legacy key must be 32 bytes, got %d", len(legacyKey))
		}
		block, err := aes.NewCipher(legacyKey)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		pe.legacy = gcm
		pe.legacyKey = append([]byte(nil), legacyKey...)
	}
	return pe, nil
}

// SetManager wires the per-bucket key manager. Until set, every bucket is treated
// as opted-out (plaintext).
func (e *PerBucketEngine) SetManager(m *bucketcrypto.Manager) {
	e.mu.Lock()
	e.mgr = m
	e.mu.Unlock()
}

func (e *PerBucketEngine) manager() *bucketcrypto.Manager {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.mgr
}

// seal encrypts plaintext for a bucket (or returns it unchanged when the bucket
// is opted out / no manager is set).
func (e *PerBucketEngine) seal(bucket string, plaintext []byte) ([]byte, error) {
	m := e.manager()
	if m == nil {
		return plaintext, nil
	}
	out, _, err := m.Encrypt(bucket, plaintext)
	return out, err
}

// open reverses seal, picking the scheme from the blob: per-bucket header → the
// bucket's key; else the legacy global key (if configured); else plaintext.
func (e *PerBucketEngine) open(bucket string, data []byte) ([]byte, error) {
	if m := e.manager(); m != nil && bucketcrypto.HasHeader(data) {
		return m.Decrypt(bucket, data)
	}
	if e.legacy != nil {
		ns := e.legacy.NonceSize()
		if len(data) < ns {
			return nil, fmt.Errorf("encrypted data too short")
		}
		return e.legacy.Open(nil, data[:ns], data[ns:], nil)
	}
	return data, nil
}

func (e *PerBucketEngine) readAll(r ReadSeekCloser) ([]byte, error) {
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, maxEncryptedSize+int64(64)))
}

// put writes reader through the bucket's key, streaming when the bucket is
// encrypted and delegating untouched when it is opted out.
func (e *PerBucketEngine) put(bucket string, reader io.Reader, size int64,
	inner func(io.Reader, int64) (int64, string, error),
) (int64, string, error) {
	m := e.manager()
	if m == nil {
		return inner(reader, size)
	}
	dek, version, ok, err := m.CurrentKey(bucket)
	if err != nil {
		return 0, "", fmt.Errorf("bucket key: %w", err)
	}
	if !ok {
		// Opted out: stored as plaintext, exactly as before.
		return inner(reader, size)
	}
	return sealStreamToEngine(dek, uint32(version), reader, size, inner)
}

// streamKey resolves which key sealed a VS3S blob from its key version.
//
// Per-bucket DEK versions start at 1, so version 0 means the object was sealed
// with a server-wide key: either written by this server before per-bucket mode
// was turned on, or by a plain encrypting engine. Those read with the legacy
// key, which is the streaming-format counterpart of the same rule open() applies
// to whole-object blobs.
func (e *PerBucketEngine) streamKey(bucket string, keyVersion uint32) ([]byte, error) {
	if keyVersion == 0 {
		if e.legacyKey == nil {
			return nil, fmt.Errorf("decrypt: object was sealed with a server-wide key but none is configured (set encryption.legacy_key)")
		}
		return e.legacyKey, nil
	}
	m := e.manager()
	if m == nil {
		return nil, fmt.Errorf("decrypt: object is encrypted but no key manager is configured")
	}
	dek, err := m.KeyForVersion(bucket, int(keyVersion))
	if err != nil {
		return nil, fmt.Errorf("bucket key v%d: %w", keyVersion, err)
	}
	return dek, nil
}

// get resolves the format from the stored blob: VS3S streams with the key
// version named in its header, VS3X and legacy global-key blobs take the
// whole-object path they were written with.
func (e *PerBucketEngine) get(bucket string, reader ReadSeekCloser, stored int64) (ReadSeekCloser, int64, error) {
	if h, ok := peekStreamHeader(reader); ok {
		dek, err := e.streamKey(bucket, h.keyVersion)
		if err != nil {
			reader.Close()
			return nil, 0, err
		}
		sr, err := newStreamReader(reader, stored, h, dek)
		if err != nil {
			reader.Close()
			return nil, 0, fmt.Errorf("open encrypted stream: %w", err)
		}
		return sr, sr.Size(), nil
	}

	data, err := e.readAll(reader)
	if err != nil {
		return nil, 0, fmt.Errorf("read object: %w", err)
	}
	plain, err := e.open(bucket, data)
	if err != nil {
		return nil, 0, fmt.Errorf("decrypt: %w", err)
	}
	return &bytesReadSeekCloser{Reader: bytes.NewReader(plain)}, int64(len(plain)), nil
}

func (e *PerBucketEngine) PutObject(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	if IsDirMarker(key) {
		return e.Engine.PutObject(bucket, key, reader, size)
	}
	return e.put(bucket, reader, size, func(body io.Reader, storedSize int64) (int64, string, error) {
		return e.Engine.PutObject(bucket, key, body, storedSize)
	})
}

func (e *PerBucketEngine) GetObject(bucket, key string) (ReadSeekCloser, int64, error) {
	if IsDirMarker(key) {
		return e.Engine.GetObject(bucket, key)
	}
	reader, stored, err := e.Engine.GetObject(bucket, key)
	if err != nil {
		return nil, 0, err
	}
	return e.get(bucket, reader, stored)
}

func (e *PerBucketEngine) PutObjectVersion(bucket, key, versionID string, reader io.Reader, size int64) (int64, string, error) {
	return e.put(bucket, reader, size, func(body io.Reader, storedSize int64) (int64, string, error) {
		return e.Engine.PutObjectVersion(bucket, key, versionID, body, storedSize)
	})
}

func (e *PerBucketEngine) GetObjectVersion(bucket, key, versionID string) (ReadSeekCloser, int64, error) {
	reader, stored, err := e.Engine.GetObjectVersion(bucket, key, versionID)
	if err != nil {
		return nil, 0, err
	}
	return e.get(bucket, reader, stored)
}
