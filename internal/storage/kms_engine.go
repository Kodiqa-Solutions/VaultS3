package storage

import (
	"bytes"
	"fmt"
	"io"
)

// KMSEncryptedEngine wraps another Engine and encrypts/decrypts data using
// KMS-managed keys (SSE-KMS). Unlike EncryptedEngine which uses a static key,
// this engine fetches data encryption keys from a KMS provider (HashiCorp Vault
// or a local key) and supports key rotation.
type KMSEncryptedEngine struct {
	inner   Engine
	kms     *KMS
	keyName string
}

// NewKMSEncryptedEngine creates an encrypting wrapper using KMS for key management.
func NewKMSEncryptedEngine(inner Engine, kms *KMS, keyName string) (*KMSEncryptedEngine, error) {
	// Validate KMS is reachable by fetching the key once
	if _, err := kms.GetDataKey(keyName); err != nil {
		return nil, fmt.Errorf("KMS key fetch failed: %w", err)
	}
	return &KMSEncryptedEngine{inner: inner, kms: kms, keyName: keyName}, nil
}

func (e *KMSEncryptedEngine) CreateBucketDir(bucket string) error {
	return e.inner.CreateBucketDir(bucket)
}

func (e *KMSEncryptedEngine) DeleteBucketDir(bucket string) error {
	return e.inner.DeleteBucketDir(bucket)
}

// dataKey fetches the DEK the streaming format seals with. The KMS caches it, so
// this is not a round trip per request.
func (e *KMSEncryptedEngine) dataKey() ([]byte, error) {
	return e.kms.GetDataKey(e.keyName)
}

// decrypt serves a stored blob, streaming when it carries a VS3S header and
// falling back to the whole-object KMS path for objects written before it.
func (e *KMSEncryptedEngine) decrypt(reader ReadSeekCloser, stored int64) (ReadSeekCloser, int64, error) {
	reader, stored, uerr := openSealed(reader, stored)
	if uerr != nil {
		return nil, 0, uerr
	}
	if h, ok := peekStreamHeader(reader); ok {
		dek, err := e.dataKey()
		if err != nil {
			reader.Close()
			return nil, 0, fmt.Errorf("kms key: %w", err)
		}
		sr, err := newStreamReader(reader, stored, h, dek)
		if err != nil {
			reader.Close()
			return nil, 0, fmt.Errorf("open encrypted stream: %w", err)
		}
		return sr, sr.Size(), nil
	}
	defer reader.Close()
	encrypted, err := io.ReadAll(io.LimitReader(reader, maxEncryptedSize+1024))
	if err != nil {
		return nil, 0, fmt.Errorf("read encrypted: %w", err)
	}
	plaintext, err := e.kms.Decrypt(e.keyName, encrypted)
	if err != nil {
		return nil, 0, fmt.Errorf("kms decrypt: %w", err)
	}
	return &bytesReadSeekCloser{Reader: bytes.NewReader(plaintext)}, int64(len(plaintext)), nil
}

func (e *KMSEncryptedEngine) PutObject(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	if IsDirMarker(key) {
		return e.inner.PutObject(bucket, key, reader, size)
	}
	dek, err := e.dataKey()
	if err != nil {
		return 0, "", fmt.Errorf("kms key: %w", err)
	}
	return sealStreamToEngine(dek, 0, reader, size, func(sealed io.Reader, storedSize int64) (int64, string, error) {
		return e.inner.PutObject(bucket, key, sealed, storedSize)
	})
}

func (e *KMSEncryptedEngine) GetObject(bucket, key string) (ReadSeekCloser, int64, error) {
	if IsDirMarker(key) {
		return e.inner.GetObject(bucket, key)
	}
	reader, stored, err := e.inner.GetObject(bucket, key)
	if err != nil {
		return nil, 0, err
	}
	return e.decrypt(reader, stored)
}

func (e *KMSEncryptedEngine) DeleteObject(bucket, key string) error {
	return e.inner.DeleteObject(bucket, key)
}

func (e *KMSEncryptedEngine) ObjectExists(bucket, key string) bool {
	return e.inner.ObjectExists(bucket, key)
}

func (e *KMSEncryptedEngine) ObjectSize(bucket, key string) (int64, error) {
	return e.inner.ObjectSize(bucket, key)
}

func (e *KMSEncryptedEngine) ListObjects(bucket, prefix, startAfter string, maxKeys int) ([]ObjectInfo, bool, error) {
	return e.inner.ListObjects(bucket, prefix, startAfter, maxKeys)
}

func (e *KMSEncryptedEngine) BucketSize(bucket string) (int64, int64, error) {
	return e.inner.BucketSize(bucket)
}

func (e *KMSEncryptedEngine) PutObjectVersion(bucket, key, versionID string, reader io.Reader, size int64) (int64, string, error) {
	dek, err := e.dataKey()
	if err != nil {
		return 0, "", fmt.Errorf("kms key: %w", err)
	}
	return sealStreamToEngine(dek, 0, reader, size, func(sealed io.Reader, storedSize int64) (int64, string, error) {
		return e.inner.PutObjectVersion(bucket, key, versionID, sealed, storedSize)
	})
}

func (e *KMSEncryptedEngine) GetObjectVersion(bucket, key, versionID string) (ReadSeekCloser, int64, error) {
	reader, stored, err := e.inner.GetObjectVersion(bucket, key, versionID)
	if err != nil {
		return nil, 0, err
	}
	return e.decrypt(reader, stored)
}

func (e *KMSEncryptedEngine) DeleteObjectVersion(bucket, key, versionID string) error {
	return e.inner.DeleteObjectVersion(bucket, key, versionID)
}

func (e *KMSEncryptedEngine) DataDir() string {
	return e.inner.DataDir()
}

func (e *KMSEncryptedEngine) ObjectPath(bucket, key string) string {
	return e.inner.ObjectPath(bucket, key)
}
