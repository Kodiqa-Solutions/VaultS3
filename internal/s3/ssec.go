package s3

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// ssecReader is an in-memory ReadSeekCloser over decrypted SSE-C plaintext. It
// is only used for objects in the pre-streaming whole-object format, which have
// to be buffered because one tag covers everything; new objects stream (see
// ssecOpenStored).
type ssecReader struct{ *bytes.Reader }

func (ssecReader) Close() error { return nil }

// SSE-C: server-side encryption with customer-provided keys. The client supplies
// a 32-byte key per request; the server encrypts/decrypts with it and stores only
// the key's MD5 (for verification) — never the key itself. This is the
// operator-blind option: lose the key and the data is unrecoverable.
//
// Objects are sealed in the chunked VS3S streaming format from
// internal/storage/streamcrypt.go, the same one server-side encryption moved to
// in 4.4.53 (issue #49), so a GET or Range request decrypts a chunk at a time
// instead of buffering the object. Before this SSE-C sealed the whole object as
// one AES-GCM message (ssecSeal), and those objects are still read by ssecOpen.
//
// Headers (mirroring S3):
//
//	x-amz-server-side-encryption-customer-algorithm: AES256
//	x-amz-server-side-encryption-customer-key:        base64(32-byte key)
//	x-amz-server-side-encryption-customer-key-MD5:    base64(md5(key))

const (
	hdrSSECAlgo   = "X-Amz-Server-Side-Encryption-Customer-Algorithm"
	hdrSSECKey    = "X-Amz-Server-Side-Encryption-Customer-Key"
	hdrSSECKeyMD5 = "X-Amz-Server-Side-Encryption-Customer-Key-Md5"
)

type sseCustomerKey struct {
	key    []byte // 32 bytes
	keyMD5 string // base64(md5(key))
}

// parseSSECHeaders extracts and validates the SSE-C headers. Returns (nil, nil)
// when no SSE-C headers are present.
func parseSSECHeaders(r *http.Request) (*sseCustomerKey, error) {
	algo := r.Header.Get(hdrSSECAlgo)
	if algo == "" {
		return nil, nil
	}
	if algo != "AES256" {
		return nil, fmt.Errorf("unsupported SSE-C algorithm %q", algo)
	}
	key, err := base64.StdEncoding.DecodeString(r.Header.Get(hdrSSECKey))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("SSE-C key must be base64 of 32 bytes")
	}
	sum := md5.Sum(key)
	want := base64.StdEncoding.EncodeToString(sum[:])
	if got := r.Header.Get(hdrSSECKeyMD5); got != "" && got != want {
		return nil, fmt.Errorf("SSE-C key MD5 mismatch")
	}
	return &sseCustomerKey{key: key, keyMD5: want}, nil
}

func ssecGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ssecSealStream encrypts reader with the customer key in the streaming format
// and hands the ciphertext to put (an engine PutObject). It returns the
// plaintext length, which is the size the object must record: the stored length
// is larger by the header and one tag per chunk.
func ssecSealStream(k *sseCustomerKey, reader io.Reader, size int64,
	put func(sealed io.Reader, storedSize int64) (int64, string, error),
) (int64, string, error) {
	return storage.SealStreamWithKey(k.key, reader, size, put)
}

// ssecOpenStored serves a stored SSE-C object as plaintext, picking the format
// from the bytes themselves the way the encrypting engines do: a VS3S header
// means a streaming reader that seeks without materialising anything; anything
// else is the pre-streaming whole-object seal, which is read and opened in full
// because it cannot be done any other way. Existing objects therefore keep
// working with no migration, and are converted by being rewritten.
//
// On success the returned reader owns (or wraps) reader; on failure reader is
// left for the caller to close.
func ssecOpenStored(k *sseCustomerKey, reader storage.ReadSeekCloser, stored int64) (storage.ReadSeekCloser, int64, error) {
	out, size, ok, err := storage.OpenStreamWithKey(k.key, reader, stored)
	if err != nil {
		return nil, 0, err
	}
	if ok {
		return out, size, nil
	}
	sealed, err := io.ReadAll(out)
	if err != nil {
		return nil, 0, err
	}
	plain, err := ssecOpen(k, sealed)
	if err != nil {
		return nil, 0, err
	}
	return ssecReader{bytes.NewReader(plain)}, int64(len(plain)), nil
}

// ssecSeal encrypts plaintext with the customer key as one AES-GCM message
// (nonce prepended). This is the format SSE-C wrote before it moved to the
// streaming one; it is kept so tests can produce such objects and prove they
// still read. New writes go through ssecSealStream.
func ssecSeal(k *sseCustomerKey, plaintext []byte) ([]byte, error) {
	gcm, err := ssecGCM(k.key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, gcm.Seal(nil, nonce, plaintext, nil)...), nil
}

// ssecOpen decrypts data produced by ssecSeal (the whole-object format) with the
// customer key.
func ssecOpen(k *sseCustomerKey, data []byte) ([]byte, error) {
	gcm, err := ssecGCM(k.key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, fmt.Errorf("SSE-C ciphertext too short")
	}
	return gcm.Open(nil, data[:ns], data[ns:], nil)
}
