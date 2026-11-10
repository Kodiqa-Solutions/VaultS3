package s3

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// The dashboard upload (internal/api/objects.go, unversioned branch) writes
// engine.PutObject + store.PutObjectMeta and never touches the CopyIndex.
// The record left by an earlier S3 PUT then names the previous ETag, so the
// node treats its own correct copy as stale forever.
func TestAWriteOutsideTheS3APIDropsTheCopyRecord(t *testing.T) {
	h, store, engine, ts := newObjTestServer(t)
	idx, err := metadata.OpenCopyIndex(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	h.SetCopyIndex(idx)
	h.SetDataHolderFallback(func(http.ResponseWriter, *http.Request, string, string) (bool, bool) { return false, false })

	bucket, key := "clust", "doc.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("first version")); resp.StatusCode != http.StatusOK {
		t.Fatal("seed put failed")
	}
	// What handleUpload does for an unversioned bucket (objects.go:669-672).
	body := []byte("dashboard upload, other size")
	// Through the server's engine on a cluster, which drops the record.
	written, etag, err := storage.NewForgetOnWrite(engine, idx).PutObject(bucket, key, bytes.NewReader(body), -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: bucket, Key: key, ContentType: "application/octet-stream",
		ETag: etag, Size: written, LastModified: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("GET after a dashboard overwrite = %d %q; the local copy is correct but its record names the S3 PUT's ETag", resp.StatusCode, got)
	}
}
