//go:build !windows

package fuse

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// realS3 runs the real S3 handler, signature checks included, for bucket "b"
// with the credentials testCfg uses.
type realS3 struct {
	url    string
	store  *metadata.Store
	engine *storage.FileSystem
}

func newRealS3(t *testing.T) realS3 {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	engine, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	if err := engine.CreateBucketDir("b"); err != nil {
		t.Fatal(err)
	}
	auth := s3.NewAuthenticator("AKIATEST", "secretkey", store, nil, nil)
	srv := httptest.NewServer(s3.NewHandler(store, engine, auth, false, "", nil))
	t.Cleanup(srv.Close)
	return realS3{srv.URL, store, engine}
}

func (r realS3) put(t *testing.T, key, body string) {
	t.Helper()
	n, etag, err := r.engine.PutObject("b", key, strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: key, Size: n, ETag: etag, LastModified: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
}

func (r realS3) get(t *testing.T, key string) (string, bool) {
	t.Helper()
	if _, err := r.store.GetObjectMeta("b", key); err != nil {
		return "", false
	}
	rd, _, err := r.engine.GetObject("b", key)
	if err != nil {
		return "", false
	}
	defer rd.Close()
	b, _ := io.ReadAll(rd)
	return string(b), true
}

func (r realS3) fs() *VaultFS {
	return &VaultFS{
		cfg:       testCfg(r.url),
		client:    &http.Client{Timeout: 5 * time.Second},
		metaCache: NewMetaCache(time.Second, time.Second),
	}
}

// rm 'a?b' on the mount deleted the object "a": the key was formatted into the
// URL, so the '?' ended the path.
func TestFUSEUnlinkRemovesTheKeyNamedNotABystander(t *testing.T) {
	srv := newRealS3(t)
	srv.put(t, "a", "bystander")
	srv.put(t, "a?b", "target")
	srv.put(t, "a#b", "target 2")

	for _, name := range []string{"a?b", "a#b"} {
		if errno := srv.fs().Unlink(context.Background(), name); errno != 0 {
			t.Fatalf("Unlink(%q) errno=%v", name, errno)
		}
		if _, ok := srv.get(t, name); ok {
			t.Errorf("%q is still there after Unlink", name)
		}
		if got, ok := srv.get(t, "a"); !ok || got != "bystander" {
			t.Errorf("Unlink(%q) removed or changed the bystander \"a\"", name)
		}
	}
}

func TestFUSEReadAndWriteUseTheKeyNamed(t *testing.T) {
	srv := newRealS3(t)
	srv.put(t, "a", "bystander")
	srv.put(t, "a?b", "the real a?b")
	cfg := testCfg(srv.url)
	client := &http.Client{Timeout: 5 * time.Second}

	h := &VaultFileHandle{cfg: cfg, client: client, key: "a?b", size: int64(len("the real a?b"))}
	rr, errno := h.Read(context.Background(), make([]byte, 64), 0)
	if errno != 0 {
		t.Fatalf("Read errno=%v", errno)
	}
	if b, _ := rr.Bytes(make([]byte, 64)); string(b) != "the real a?b" {
		t.Errorf("read %q for a?b", b)
	}

	wh := &VaultWriteHandle{cfg: cfg, client: client, key: "a%41b", metaCache: NewMetaCache(time.Second, time.Second)}
	wh.Write(context.Background(), []byte("written"), 0)
	if errno := wh.Flush(context.Background()); errno != 0 {
		t.Fatalf("Flush errno=%v", errno)
	}
	if got, ok := srv.get(t, "a%41b"); !ok || got != "written" {
		t.Errorf("a%%41b = %q (present %v)", got, ok)
	}
	if _, ok := srv.get(t, "aAb"); ok {
		t.Error("writing a%41b created aAb")
	}
}

// A 403 or 404 answer had its XML error body served, and cached, as the file's
// contents.
func TestFUSEReadReturnsAnErrnoForAnErrorAnswer(t *testing.T) {
	srv := newRealS3(t)
	srv.put(t, "k", "0123456789")

	for _, withCache := range []bool{false, true} {
		var cache *BlockCache
		if withCache {
			cache = NewBlockCache(1 << 20)
		}
		missing := &VaultFileHandle{cfg: testCfg(srv.url), client: http.DefaultClient, key: "gone", size: 10, blockCache: cache}
		if _, errno := missing.Read(context.Background(), make([]byte, 10), 0); errno != syscall.ENOENT {
			t.Errorf("cache=%v: read of a missing object errno=%v, want ENOENT", withCache, errno)
		}

		bad := testCfg(srv.url)
		bad.SecretKey = "wrong"
		denied := &VaultFileHandle{cfg: bad, client: http.DefaultClient, key: "k", size: 10, blockCache: cache}
		if _, errno := denied.Read(context.Background(), make([]byte, 10), 0); errno != syscall.EACCES {
			t.Errorf("cache=%v: denied read errno=%v, want EACCES", withCache, errno)
		}
		if withCache && cache.Get("b", "k", 0) != nil {
			t.Error("an error answer was cached as file data")
		}
	}
}

// A refused delete returned success, so rm reported the file gone.
func TestFUSEUnlinkReportsARefusal(t *testing.T) {
	srv := newRealS3(t)
	srv.put(t, "k", "still here")
	v := srv.fs()
	v.cfg.SecretKey = "wrong"
	if errno := v.Unlink(context.Background(), "k"); errno != syscall.EACCES {
		t.Errorf("refused Unlink errno=%v, want EACCES", errno)
	}
	if _, ok := srv.get(t, "k"); !ok {
		t.Fatal("the object went away anyway")
	}
}

// Write appended whatever arrived and ignored the offset, so writes delivered
// out of order, or a program seeking back to patch a header, corrupted the
// object.
func TestFUSEWriteHonoursTheOffset(t *testing.T) {
	endpoint, store := newS3Stub(t)
	wh := &VaultWriteHandle{cfg: testCfg(endpoint), client: http.DefaultClient, key: "f", metaCache: NewMetaCache(time.Second, time.Second)}
	ctx := context.Background()
	for _, w := range []struct {
		data string
		off  int64
	}{{"world", 6}, {"hello ", 0}, {"HEADER", 0}, {"!", 13}} {
		if n, errno := wh.Write(ctx, []byte(w.data), w.off); errno != 0 || int(n) != len(w.data) {
			t.Fatalf("Write(%q, %d) n=%d errno=%v", w.data, w.off, n, errno)
		}
	}
	if errno := wh.Flush(ctx); errno != 0 {
		t.Fatalf("Flush errno=%v", errno)
	}
	got, _ := store.Load("f")
	if want := "HEADERworld\x00\x00!"; string(got.([]byte)) != want {
		t.Errorf("object = %q, want %q", got, want)
	}
	if _, errno := wh.Write(ctx, []byte("x"), -1); errno != syscall.EINVAL {
		t.Errorf("negative offset errno=%v, want EINVAL", errno)
	}
	if _, errno := wh.Write(ctx, []byte("x"), maxWriteSize); errno != syscall.EFBIG {
		t.Errorf("write past the size limit errno=%v, want EFBIG", errno)
	}
}

// Listing a directory whose name holds a space failed the signature check, and
// a key holding '&' was listed XML-escaped as "&amp;".
func TestFUSEListsDirectoriesWithReservedCharacters(t *testing.T) {
	srv := newRealS3(t)
	srv.put(t, "my dir/x&y.txt", "1")
	entries, err := srv.fs().doListObjects("my dir/", "/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var keys []string
	for _, e := range entries {
		keys = append(keys, e.key)
	}
	if strings.Join(keys, ",") != "my dir/x&y.txt" {
		t.Errorf("listed %q", keys)
	}

	bad := srv.fs()
	bad.cfg.SecretKey = "wrong"
	if _, err := bad.doListObjects("", "/"); err == nil {
		t.Error("a refused listing read as an empty directory")
	}
}
