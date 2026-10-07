package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// The updater installed any binary whose hash matched a checksums file from the
// same release, which proves the download is intact but not who published it.
// It must install only checksums signed by the built-in release key.
func TestChecksumsMustBeSignedByTheReleaseKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	sums := []byte("abc123  vaults3-linux-amd64.tar.gz\n")

	if err := verifyChecksums(pub, sums, ed25519.Sign(priv, sums)); err != nil {
		t.Errorf("a valid raw signature was refused: %v", err)
	}
	b64 := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)) + "\n")
	if err := verifyChecksums(pub, sums, b64); err != nil {
		t.Errorf("a valid base64 signature was refused: %v", err)
	}
	if err := verifyChecksums(pub, sums, ed25519.Sign(other, sums)); err == nil {
		t.Error("a signature from another key was accepted")
	}
	tampered := append([]byte("ffff"), sums[4:]...)
	if err := verifyChecksums(pub, tampered, ed25519.Sign(priv, sums)); err == nil {
		t.Error("a tampered checksums file was accepted")
	}

	saved := releaseSigningKey
	defer func() { releaseSigningKey = saved }()
	releaseSigningKey = ""
	if _, err := releasePublicKey(); err == nil {
		t.Error("a build with no release key claimed it could verify a release")
	}
	releaseSigningKey = base64.StdEncoding.EncodeToString(pub)
	if k, err := releasePublicKey(); err != nil || !k.Equal(pub) {
		t.Errorf("the built-in key did not decode: %v", err)
	}
}
