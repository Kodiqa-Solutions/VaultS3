package api

import "testing"

// The receiving node decides how to store a replica from its own view of the
// bucket's encryption config, which arrives by Raft and can be behind. Guessing
// "not encrypted" writes the copy in the clear, permanently, in a bucket whose
// entire purpose is that it is not.
func TestBucketCryptoReady(t *testing.T) {
	t.Run("sender says plaintext, nothing to wait for", func(t *testing.T) {
		barriers := 0
		h := &APIHandler{
			bucketEncrypted: func(string) bool { return false },
			metaBarrier:     func() error { barriers++; return nil },
		}
		if !h.bucketCryptoReady("b", false) {
			t.Fatal("a plaintext bucket must not be blocked")
		}
		if barriers != 0 {
			t.Fatalf("an unencrypted bucket must not pay for a barrier, got %d", barriers)
		}
	})

	t.Run("both agree it is encrypted, no barrier needed", func(t *testing.T) {
		barriers := 0
		h := &APIHandler{
			bucketEncrypted: func(string) bool { return true },
			metaBarrier:     func() error { barriers++; return nil },
		}
		if !h.bucketCryptoReady("b", true) {
			t.Fatal("this node already knows the bucket is encrypted")
		}
		if barriers != 0 {
			t.Fatalf("no barrier needed once the config is known, got %d", barriers)
		}
	})

	t.Run("this node is behind, the barrier catches it up", func(t *testing.T) {
		caughtUp := false
		h := &APIHandler{
			bucketEncrypted: func(string) bool { return caughtUp },
			metaBarrier:     func() error { caughtUp = true; return nil },
		}
		if !h.bucketCryptoReady("b", true) {
			t.Fatal("after the barrier this node knows the bucket is encrypted")
		}
	})

	t.Run("still behind after the barrier, refuse the write", func(t *testing.T) {
		h := &APIHandler{
			bucketEncrypted: func(string) bool { return false },
			metaBarrier:     func() error { return nil },
		}
		if h.bucketCryptoReady("b", true) {
			t.Fatal("writing now would store the copy in the clear, it must be refused")
		}
	})

	t.Run("guard not wired, stay out of the way", func(t *testing.T) {
		h := &APIHandler{}
		if !h.bucketCryptoReady("b", true) {
			t.Fatal("a single-node server has no cluster race to protect against")
		}
	})
}

// The header is spelled in two packages because internal/api already depends on
// internal/cluster, so the constant cannot be shared. Pin the literal in both.
func TestBucketEncryptedHeaderLiteral(t *testing.T) {
	if BucketEncryptedHeader != "X-Vaults3-Bucket-Encrypted" {
		t.Fatalf("header renamed to %q, update internal/cluster too", BucketEncryptedHeader)
	}
}
