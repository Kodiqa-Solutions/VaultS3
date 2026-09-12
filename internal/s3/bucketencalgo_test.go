package s3

import (
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A per-bucket-mode server encrypts only with a per-bucket AES256 key. It used
// to accept SSEAlgorithm aws:kms, report the bucket as KMS encrypted, and then
// store every object and every replica in the clear, because nothing in that
// mode knows how to honour KMS. Refusing is the only answer that matches what
// the data on disk will actually be.
func TestPutBucketEncryptionRejectsAlgorithmsItCannotHonour(t *testing.T) {
	body := `<ServerSideEncryptionConfiguration><Rule>` +
		`<ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm>` +
		`<KMSMasterKeyID>k1</KMSMasterKeyID></ApplyServerSideEncryptionByDefault>` +
		`</Rule></ServerSideEncryptionConfiguration>`

	t.Run("per-bucket mode refuses aws:kms", func(t *testing.T) {
		h := &BucketHandler{store: newEncAlgoStore(), perBucketMode: true}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/b?encryption", strings.NewReader(body))
		h.PutBucketEncryption(w, r, "b")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: silently storing plaintext is the bug", w.Code)
		}
	})

	t.Run("a KMS-mode server still accepts it", func(t *testing.T) {
		h := &BucketHandler{store: newEncAlgoStore(), perBucketMode: false}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/b?encryption", strings.NewReader(body))
		h.PutBucketEncryption(w, r, "b")
		if w.Code != http.StatusOK {
			t.Fatalf("a server whose engine encrypts everything can honour this, got %d", w.Code)
		}
	})
}

// encAlgoStore is the slice of the store this handler path touches.
type encAlgoStore struct {
	metadata.StoreAPI
	cfg metadata.BucketEncryptionConfig
}

func newEncAlgoStore() *encAlgoStore { return &encAlgoStore{} }

func (s *encAlgoStore) BucketExists(string) bool { return true }
func (s *encAlgoStore) PutEncryptionConfig(_ string, c metadata.BucketEncryptionConfig) error {
	s.cfg = c
	return nil
}
