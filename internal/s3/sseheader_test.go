package s3

import (
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
)

// The SSE response header used to come from the global encryption flag alone.
// In per-bucket mode that is a different question, and the server was telling
// clients that objects in buckets which never opted in were encrypted at rest
// when they were plaintext on disk (issue #53).
func TestSSEHeaderReflectsTheBucketNotTheGlobalFlag(t *testing.T) {
	t.Run("encryption off says nothing", func(t *testing.T) {
		h := &ObjectHandler{encryptionEnabled: false}
		if h.sseHeaderApplies("any") {
			t.Fatal("no encryption configured, so no object is encrypted")
		}
	})

	t.Run("server-wide key covers every bucket", func(t *testing.T) {
		h := &ObjectHandler{encryptionEnabled: true} // not per-bucket mode
		if !h.sseHeaderApplies("any") {
			t.Fatal("one server-wide key encrypts every object, the header is correct")
		}
	})

	// SSE-KMS requires an `encryption.key` it never uses, so a key manager exists
	// even though no bucket has a per-bucket key. Reading the answer off that
	// manager reported every KMS bucket as unencrypted.
	t.Run("SSE-KMS covers every bucket despite a key manager", func(t *testing.T) {
		h := &ObjectHandler{encryptionEnabled: true, keyMgr: newSSEHeaderMgr(t)}
		if !h.sseHeaderApplies("any") {
			t.Fatal("SSE-KMS encrypts every object, the header must say so")
		}
	})
}

// The per-bucket case needs a real manager, since opt-in is its state.
func TestSSEHeaderPerBucket(t *testing.T) {
	mgr := newSSEHeaderMgr(t)
	// perBucketMode is what makes encryption a per-bucket question at all.
	h := &ObjectHandler{encryptionEnabled: true, perBucketMode: true, keyMgr: mgr}

	if h.sseHeaderApplies("optedout") {
		t.Fatal("a bucket that never opted in stores plaintext, claiming AES256 is a false claim")
	}
	if err := mgr.EnableBucket("optedin"); err != nil {
		t.Fatal(err)
	}
	if !h.sseHeaderApplies("optedin") {
		t.Fatal("a bucket that opted in really is encrypted, the header must say so")
	}
}

func newSSEHeaderMgr(t *testing.T) *bucketcrypto.Manager {
	t.Helper()
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i)
	}
	kek, err := bucketcrypto.NewKEK(mk)
	if err != nil {
		t.Fatal(err)
	}
	return bucketcrypto.NewManager(kek, bucketcrypto.NewMemKeyStore())
}
