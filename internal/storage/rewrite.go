package storage

import (
	"fmt"
	"io"
)

// LegacyFormatEngine is implemented by an at-rest wrapper that can still hold
// objects in a superseded on-disk format.
//
// Encryption before 4.4.53 sealed each object as one AES-GCM message. Such an
// object cannot be streamed on read, and not for want of trying: the
// authentication tag covers the whole message and sits at the end, so releasing
// plaintext before verifying it would mean serving unauthenticated bytes, which
// is the one thing an AEAD exists to prevent. The current format authenticates
// per chunk, which is what makes a streaming read safe.
//
// So an object written before 4.4.53 pays its own size in latency and memory on
// every read, for as long as it exists, and rotating keys does not help because
// rotation mints a new key version without rewriting any bodies. The only way
// off it is to rewrite the object, which is what this interface exists for.
type LegacyFormatEngine interface {
	// IsLegacyObject reports whether the stored object uses the superseded
	// format. A false with no error means it is already current.
	IsLegacyObject(bucket, key string) (bool, error)
	// RewriteObject re-reads the object and writes it back in the current
	// format. The underlying write is atomic (temp file then rename), so an
	// interrupted rewrite leaves the original in place.
	RewriteObject(bucket, key string) error
}

// isLegacySealed reports whether the stored bytes are encrypted in the
// superseded whole-object format.
//
// The absence of the streaming magic is not enough on its own: the legacy format
// has no magic of its own (it is nonce, ciphertext, tag), so on disk it is
// indistinguishable from plaintext. A bucket that never opted in stores
// plaintext, which would otherwise be reported as legacy and rewritten
// pointlessly. The caller therefore has to say whether this bucket is encrypted
// at all, and only then does a missing magic mean legacy.
func isLegacySealed(inner Engine, bucket, key string, encrypted bool) (bool, error) {
	if !encrypted {
		return false, nil
	}
	rc, _, err := inner.GetObject(bucket, key)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	buf := make([]byte, len(streamMagic))
	if _, err := io.ReadFull(rc, buf); err != nil {
		// Shorter than a header, so it cannot be a stream. An empty object is
		// not encrypted at all and needs no rewrite.
		return false, nil
	}
	return string(buf) != streamMagic, nil
}

// rewriteThrough reads an object out through the wrapper, which decrypts it,
// and writes it back through the same wrapper, which seals it in the current
// format. Plaintext never leaves the process.
//
// A legacy object is materialised to be decrypted, which is the very cost being
// removed, so this is a one-off price per object and callers should migrate
// objects one at a time rather than fanning out.
func rewriteThrough(w Engine, bucket, key string) error {
	rc, size, err := w.GetObject(bucket, key)
	if err != nil {
		return fmt.Errorf("read %s/%s: %w", bucket, key, err)
	}
	defer rc.Close()
	if _, _, err := w.PutObject(bucket, key, rc, size); err != nil {
		return fmt.Errorf("rewrite %s/%s: %w", bucket, key, err)
	}
	return nil
}

// IsLegacyObject implements LegacyFormatEngine. This engine encrypts every
// object it stores, so anything without the streaming magic is legacy.
func (e *EncryptedEngine) IsLegacyObject(bucket, key string) (bool, error) {
	return isLegacySealed(e.inner, bucket, key, true)
}

// RewriteObject implements LegacyFormatEngine.
func (e *EncryptedEngine) RewriteObject(bucket, key string) error {
	return rewriteThrough(e, bucket, key)
}

// IsLegacyObject implements LegacyFormatEngine. Only a bucket that has opted in
// holds encrypted objects, so a bucket that has not is never legacy.
func (e *PerBucketEngine) IsLegacyObject(bucket, key string) (bool, error) {
	m := e.manager()
	encrypted := m != nil && m.IsEncrypted(bucket)
	return isLegacySealed(e.Engine, bucket, key, encrypted)
}

// RewriteObject implements LegacyFormatEngine.
func (e *PerBucketEngine) RewriteObject(bucket, key string) error {
	return rewriteThrough(e, bucket, key)
}
