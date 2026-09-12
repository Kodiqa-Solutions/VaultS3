package bucketkeys

import (
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

type fakeStore struct {
	metadata.StoreAPI
	cfg metadata.BucketEncryptionConfig
}

func (f *fakeStore) GetEncryptionConfig(string) (*metadata.BucketEncryptionConfig, error) {
	c := f.cfg
	return &c, nil
}

func TestEncryptionPendingOnlyForPerBucketAES(t *testing.T) {
	cases := []struct {
		name string
		cfg  metadata.BucketEncryptionConfig
		want bool
	}{
		{"AES256 with no key yet", metadata.BucketEncryptionConfig{SSEAlgorithm: "AES256"}, true},
		{"AES256 with a key", metadata.BucketEncryptionConfig{SSEAlgorithm: "AES256", KeyVersion: 1}, false},
		{"no encryption at all", metadata.BucketEncryptionConfig{}, false},
		// SSE-KMS never provisions a per-bucket key, so it is never "pending".
		// Treating it as pending refuses every write to the bucket, forever.
		{"aws:kms", metadata.BucketEncryptionConfig{SSEAlgorithm: "aws:kms", KMSKeyID: "k1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := storeKeyStore{store: &fakeStore{cfg: tc.cfg}}
			if got := s.EncryptionPending("b"); got != tc.want {
				t.Fatalf("EncryptionPending = %v, want %v", got, tc.want)
			}
		})
	}
}
