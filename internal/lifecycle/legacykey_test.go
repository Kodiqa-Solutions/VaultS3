package lifecycle

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Regression (5.0.11 round 14): a legacy record "./doc" left by an earlier
// version expired under a lifecycle rule, and its file is doc's: doc lost its
// data although it was under legal hold. Only the record goes now.
func TestLifecycleExpiryOfALegacyKeyKeepsTheOtherObject(t *testing.T) {
	base := t.TempDir()
	fs, _ := storage.NewFileSystem(filepath.Join(base, "d"))
	st, _ := metadata.NewStore(filepath.Join(base, "m.db"))
	defer st.Close()
	st.CreateBucket("b")
	fs.CreateBucketDir("b")
	data := []byte("doc data, under legal hold")
	fs.PutObject("b", "doc", bytes.NewReader(data), int64(len(data)))
	st.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "doc", Size: int64(len(data)), LastModified: time.Now().Unix(), LegalHold: true})
	old := metadata.ObjectMeta{Bucket: "b", Key: "./doc", Size: int64(len(data)), LastModified: time.Now().Add(-90 * 24 * time.Hour).Unix()}
	st.PutObjectMeta(old)
	w := NewWorker(st, fs, 3600, 90)
	snap, _ := st.GetObjectMeta("b", "./doc")
	ok := w.expireCurrent(*snap, "", time.Now().Unix())
	if _, _, err := fs.GetObject("b", "doc"); err != nil {
		t.Fatalf("expired=%v: doc (legal hold) lost its data through the legacy record ./doc: %v", ok, err)
	}
}
