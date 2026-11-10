package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Regression (5.0.15 review): replica-put held the key's exclusive lock while it reads the body from the
// sending node, so a client write of that key on this node (writeObject takes
// LockObjectKeyShared before reading its own body) waits for the whole
// transfer, however slow or stalled the sender is.
func TestAStalledReplicaCopyNeverHoldsClientWritesBack(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("rb"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: "rb", Key: "k", Size: 1 << 20, ETag: `"e"`}); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	req := httptest.NewRequest(http.MethodPost, "/cluster/replica-put?bucket=rb&key=k", pr)
	req.ContentLength = 1 << 20
	req.Header.Set(clusterSecretHeader, "s")
	req.Header.Set(ReplicaETagHeader, `"e"`)
	done := make(chan struct{})
	go func() { h.ClusterReplicaPutHandler("s")(httptest.NewRecorder(), req); close(done) }()
	_, _ = pw.Write(make([]byte, 10)) // the sender sends a little, then stalls
	time.Sleep(200 * time.Millisecond)
	got := make(chan struct{})
	go func() { unlock := storage.LockObjectKeyShared("rb", "k"); unlock(); close(got) }()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		_ = pw.CloseWithError(io.ErrUnexpectedEOF)
		<-done
		t.Fatal("a client write of the key waited 2s+ behind a stalled replica copy (exclusive lock held across the body read)")
	}
	_ = pw.CloseWithError(io.ErrUnexpectedEOF)
	<-done
}
