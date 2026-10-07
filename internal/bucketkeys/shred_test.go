package bucketkeys

import (
	"bytes"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Two managers over one store stand in for two cluster nodes, whose stores
// hold the same replicated config. A shred on one must stop the other from
// using the destroyed key, which it kept in its cache until it restarted.
func TestShredReachesOtherNodesCache(t *testing.T) {
	store := newTestStore(t)
	store.CreateBucket("b")
	mk := bytes.Repeat([]byte{7}, 32)
	nodeA, _ := NewManager(store, mk)
	nodeB, _ := NewManager(store, mk)

	if err := nodeA.EnableBucket("b"); err != nil {
		t.Fatal(err)
	}
	oldKey, err := nodeB.KeyForVersion("b", 1) // warms B's cache
	if err != nil {
		t.Fatal(err)
	}

	if err := nodeA.ShredBucket("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeB.KeyForVersion("b", 1); err == nil {
		t.Fatal("another node still hands out the shredded key from its cache")
	}

	// Re-enabling starts again at version 1 with a NEW key. A node must not
	// seal new objects with the old, shredded key that had the same version.
	if err := nodeA.EnableBucket("b"); err != nil {
		t.Fatal(err)
	}
	keyB, v, ok, err := nodeB.CurrentKey("b")
	if err != nil || !ok || v != 1 {
		t.Fatalf("CurrentKey after re-enable: v=%d ok=%v err=%v", v, ok, err)
	}
	keyA, _, _, _ := nodeA.CurrentKey("b")
	if bytes.Equal(keyB, oldKey) || !bytes.Equal(keyB, keyA) {
		t.Fatal("after re-enable another node uses the shredded key instead of the new one")
	}
}

// After a shred the bucket must accept writes again. Its config kept
// SSEAlgorithm AES256 with no key, which EncryptionPending reads as "the key
// has not arrived yet", so every write returned 503 forever.
func TestShreddedBucketIsNotPendingForever(t *testing.T) {
	store := newTestStore(t)
	store.CreateBucket("b")
	store.PutEncryptionConfig("b", metadata.BucketEncryptionConfig{SSEAlgorithm: "AES256"})
	mgr, _ := NewManager(store, bytes.Repeat([]byte{7}, 32))
	if err := mgr.EnableBucket("b"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ShredBucket("b"); err != nil {
		t.Fatal(err)
	}
	if mgr.EncryptionPending("b") {
		t.Fatal("a shredded bucket reads as waiting for its key, so every write is refused")
	}
	if _, _, ok, _ := mgr.CurrentKey("b"); ok {
		t.Fatal("a shredded bucket still has a current key")
	}
	cfg, _ := store.GetEncryptionConfig("b")
	if cfg == nil || !cfg.Shredded || cfg.SSEAlgorithm != "" {
		t.Fatalf("shredded config is not marked as such: %+v", cfg)
	}

	// Enabling again provisions a fresh key and clears the mark.
	if err := mgr.EnableBucket("b"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = store.GetEncryptionConfig("b")
	if cfg.Shredded || cfg.SSEAlgorithm != "AES256" || cfg.KeyVersion != 1 {
		t.Fatalf("re-enabled config: %+v", cfg)
	}
}
