package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
)

// EncryptedEngine wraps another Engine and encrypts/decrypts data transparently
// with AES-256-GCM.
//
// Writes use the chunked streaming format (see streamcrypt.go) so neither a PUT
// nor a GET holds more than a chunk of the object. Objects written by earlier
// versions used one GCM seal over the whole object and are still read, by the
// legacy path, which necessarily buffers because a single tag covers everything
// (issue #49).
type EncryptedEngine struct {
	inner Engine
	gcm   cipher.AEAD
	key   []byte
}

// NewEncryptedEngine creates an encrypting wrapper around the given engine.
// key must be exactly 32 bytes (256 bits).
func NewEncryptedEngine(inner Engine, key []byte) (*EncryptedEngine, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	return &EncryptedEngine{inner: inner, gcm: gcm, key: append([]byte(nil), key...)}, nil
}

func (e *EncryptedEngine) CreateBucketDir(bucket string) error {
	return e.inner.CreateBucketDir(bucket)
}

func (e *EncryptedEngine) DeleteBucketDir(bucket string) error {
	return e.inner.DeleteBucketDir(bucket)
}

// maxEncryptedSize bounds the objects that still use the whole-object format:
// those written before the streaming format existed, which have to be held in
// memory to be authenticated. New writes stream and have no such limit.
const maxEncryptedSize int64 = 1 * 1024 * 1024 * 1024

func (e *EncryptedEngine) PutObject(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	if IsDirMarker(key) {
		// Directory markers hold no bytes; store as a plain directory, no crypto.
		return e.inner.PutObject(bucket, key, reader, size)
	}
	return sealStreamToEngine(e.key, 0, reader, size, func(sealed io.Reader, storedSize int64) (int64, string, error) {
		return e.inner.PutObject(bucket, key, sealed, storedSize)
	})
}

func (e *EncryptedEngine) GetObject(bucket, key string) (ReadSeekCloser, int64, error) {
	if IsDirMarker(key) {
		return e.inner.GetObject(bucket, key)
	}
	reader, stored, err := e.inner.GetObject(bucket, key)
	if err != nil {
		return nil, 0, err
	}
	return e.decrypt(reader, stored)
}

// decrypt picks the format from the blob itself: a VS3S header means the object
// streams, anything else is the original whole-object format.
func (e *EncryptedEngine) decrypt(reader ReadSeekCloser, stored int64) (ReadSeekCloser, int64, error) {
	if h, ok := peekStreamHeader(reader); ok {
		sr, err := newStreamReader(reader, stored, h, e.key)
		if err != nil {
			reader.Close()
			return nil, 0, fmt.Errorf("open encrypted stream: %w", err)
		}
		return sr, sr.Size(), nil
	}
	defer reader.Close()
	plaintext, err := openLegacyWhole(reader, stored, e.gcm)
	if err != nil {
		return nil, 0, fmt.Errorf("decrypt: %w", err)
	}
	return &bytesReadSeekCloser{Reader: bytes.NewReader(plaintext)}, int64(len(plaintext)), nil
}

func (e *EncryptedEngine) DeleteObject(bucket, key string) error {
	return e.inner.DeleteObject(bucket, key)
}

func (e *EncryptedEngine) ObjectExists(bucket, key string) bool {
	return e.inner.ObjectExists(bucket, key)
}

func (e *EncryptedEngine) ObjectSize(bucket, key string) (int64, error) {
	// For encrypted objects, the file size on disk is larger than the plaintext.
	// We need to decrypt to get the real size, but that's expensive.
	// Return the on-disk size — callers should use metadata for accurate size.
	return e.inner.ObjectSize(bucket, key)
}

func (e *EncryptedEngine) ListObjects(bucket, prefix, startAfter string, maxKeys int) ([]ObjectInfo, bool, error) {
	return e.inner.ListObjects(bucket, prefix, startAfter, maxKeys)
}

func (e *EncryptedEngine) BucketSize(bucket string) (int64, int64, error) {
	return e.inner.BucketSize(bucket)
}

func (e *EncryptedEngine) PutObjectVersion(bucket, key, versionID string, reader io.Reader, size int64) (int64, string, error) {
	return sealStreamToEngine(e.key, 0, reader, size, func(sealed io.Reader, storedSize int64) (int64, string, error) {
		return e.inner.PutObjectVersion(bucket, key, versionID, sealed, storedSize)
	})
}

func (e *EncryptedEngine) GetObjectVersion(bucket, key, versionID string) (ReadSeekCloser, int64, error) {
	reader, stored, err := e.inner.GetObjectVersion(bucket, key, versionID)
	if err != nil {
		return nil, 0, err
	}
	return e.decrypt(reader, stored)
}

func (e *EncryptedEngine) DeleteObjectVersion(bucket, key, versionID string) error {
	return e.inner.DeleteObjectVersion(bucket, key, versionID)
}

func (e *EncryptedEngine) DataDir() string {
	return e.inner.DataDir()
}

func (e *EncryptedEngine) ObjectPath(bucket, key string) string {
	return e.inner.ObjectPath(bucket, key)
}

// bytesReadSeekCloser wraps a bytes.Reader to implement ReadSeekCloser.
type bytesReadSeekCloser struct {
	*bytes.Reader
}

func (b *bytesReadSeekCloser) Close() error {
	return nil
}
