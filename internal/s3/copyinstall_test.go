package s3

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Regression (5.0.16 soak): a copy arriving from another node drops the
// record, replaces the file and records it again under the key's exclusive
// lock. A read in that window found the previous object's file with no
// record, the same size (a multipart object, which has no content check),
// and served it. Reads now open and judge under the shared lock.
func TestAReadWaitsForACopyBeingInstalled(t *testing.T) {
	h, store, engine, ts := newObjTestServer(t)
	idx, err := metadata.OpenCopyIndex(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	h.SetCopyIndex(idx)
	h.SetDataHolderFallback(func(http.ResponseWriter, *http.Request, string, string) (bool, bool) { return false, false })
	if err := store.CreateBucket("cib"); err != nil {
		t.Fatal(err)
	}
	old, newer := strings.Repeat("A", 64), strings.Repeat("B", 64)
	if _, _, err := engine.PutObject("cib", "k", strings.NewReader(old), 64); err != nil {
		t.Fatal(err)
	}
	// The metadata already names the new object (committed before its copy
	// is sent), a multipart one of the same size.
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: "cib", Key: "k", Size: 64, ETag: `"new-2"`, LastModified: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	// The copy is being installed: lock held, record dropped, file still old.
	unlock := storage.LockObjectKey("cib", "k")
	idx.Forget("cib", "k", "")
	got := make(chan string, 1)
	go func() {
		resp := doSigned(t, http.MethodGet, ts.URL+"/cib/k", nil)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		got <- string(b)
	}()
	select {
	case b := <-got:
		unlock()
		t.Fatalf("a read during a copy install answered at once with %.8s...", b)
	case <-time.After(300 * time.Millisecond):
	}
	if _, _, err := engine.PutObject("cib", "k", strings.NewReader(newer), 64); err != nil {
		t.Fatal(err)
	}
	idx.Record("cib", "k", "", `"new-2"`, 64)
	unlock()
	if b := <-got; b != newer {
		t.Errorf("the read served %.8s..., want the installed copy", b)
	}
}
