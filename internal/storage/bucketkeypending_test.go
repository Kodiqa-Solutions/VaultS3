package storage

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
)

// pendingKeyStore is a node that has the bucket's encryption config but not yet
// its key: the state a follower is in between the two Raft entries that enabling
// encryption writes.
type pendingKeyStore struct{ bucketcrypto.KeyStore }

func (pendingKeyStore) EncryptionPending(string) bool { return true }

// A node that cannot see the bucket's key must refuse the write. Falling back to
// plaintext there is indistinguishable, in the code, from a bucket that opted
// out, and the object it writes stays readable on disk forever in a bucket whose
// whole purpose is that it is not.
func TestPutRefusesWhenBucketKeyHasNotArrived(t *testing.T) {
	mk := make([]byte, 32)
	rand.Read(mk)
	kek, err := bucketcrypto.NewKEK(mk)
	if err != nil {
		t.Fatal(err)
	}
	mgr := bucketcrypto.NewManager(kek, pendingKeyStore{bucketcrypto.NewMemKeyStore()})

	fs, _ := NewFileSystem(t.TempDir())
	pe, _ := NewPerBucketEngine(fs, nil)
	pe.SetManager(mgr)
	fs.CreateBucketDir("enc")

	secret := []byte("PLAINTEXT-MUST-NOT-REACH-DISK")
	_, _, err = pe.PutObject("enc", "o", bytes.NewReader(secret), int64(len(secret)))
	if !errors.Is(err, ErrBucketKeyUnavailable) {
		t.Fatalf("write must be refused while the key is missing, got err=%v", err)
	}
	if raw, rerr := os.ReadFile(fs.ObjectPath("enc", "o")); rerr == nil && bytes.Contains(raw, secret) {
		t.Fatal("the object was written in the clear into a bucket that asked for encryption")
	}
}

// A bucket that genuinely opted out must still store plaintext, or this guard
// would break every unencrypted bucket on the server.
func TestPutStillStoresPlaintextForOptedOutBuckets(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	pe, _ := NewPerBucketEngine(fs, nil)
	pe.SetManager(newMgr(t)) // MemKeyStore: never pending
	fs.CreateBucketDir("plain")

	body := []byte("this bucket opted out")
	if _, _, err := pe.PutObject("plain", "o", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("an opted-out bucket must still accept writes: %v", err)
	}
	raw, _ := os.ReadFile(fs.ObjectPath("plain", "o"))
	if !bytes.Contains(raw, body) {
		t.Fatal("an opted-out bucket stores plaintext, as before")
	}
}
