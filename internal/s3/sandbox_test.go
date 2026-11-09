package s3

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"

	"github.com/Kodiqa-Solutions/VaultS3/internal/middleware"
)

// Regression (5.0.14 security review): objects were served from the
// dashboard's origin under its own policy (default-src 'self'), so an HTML
// object a user uploaded, with a script that was another object, ran as the
// dashboard and read the admin's session token. Objects are sandboxed now:
// shown, but without script and in an origin of their own.
func TestObjectsAreServedSandboxed(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/sbx", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/sbx/evil.html", "<script>alert(1)</script>", "Content-Type", "text/html").must(t, "put")
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		r := e.admin(t, m, "/sbx/evil.html", "")
		if got := r.header.Get("Content-Security-Policy"); got != middleware.ObjectSandboxCSP {
			t.Errorf("%s object: CSP %q, want %q", m, got, middleware.ObjectSandboxCSP)
		}
	}
	// An error answer is sandboxed too: it can echo a key.
	if got := e.admin(t, http.MethodGet, "/sbx/missing.html", "").header.Get("Content-Security-Policy"); got != middleware.ObjectSandboxCSP {
		t.Errorf("error answer: CSP %q", got)
	}
	// A website page keeps its scripts, in its own origin.
	cfg := `<WebsiteConfiguration><IndexDocument><Suffix>evil.html</Suffix></IndexDocument></WebsiteConfiguration>`
	e.admin(t, http.MethodPut, "/sbx?website", cfg).must(t, "website")
	r := e.admin(t, http.MethodGet, "/sbx/", "")
	csp := r.header.Get("Content-Security-Policy")
	if r.code != http.StatusOK || csp != middleware.WebsiteSandboxCSP || strings.Contains(csp, "allow-same-origin") {
		t.Errorf("website page: %d, CSP %q, want %q", r.code, csp, middleware.WebsiteSandboxCSP)
	}
}

// Regression (5.0.14 security review): a key segment longer than a file
// name failed in the engine with a 500 and the on-disk path in the log.
func TestALongKeyIsKeyTooLong(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/klen", "").must(t, "bucket")
	for _, key := range []string{strings.Repeat("a", 256), "dir/" + strings.Repeat("b", 300), strings.Repeat("c/", 600)} {
		r := e.admin(t, http.MethodPut, "/klen/"+key, "x")
		if r.code != http.StatusBadRequest || !strings.Contains(r.body, "KeyTooLongError") {
			t.Errorf("key of %d bytes: %d %.120s", len(key), r.code, r.body)
		}
	}
	e.admin(t, http.MethodPut, "/klen/"+strings.Repeat("a", 255), "x").must(t, "a 255 byte segment")
}

// R9-4: on a cluster the front node sets "sandbox" before proxying, and the
// reverse proxy ADDS the holder's headers to it, so a website page proxied
// from another node carries both policies. Browsers enforce every CSP
// header, so the website's scripts never run on a cluster.
func TestAProxiedWebsitePageCarriesOnlyItsOwnPolicy(t *testing.T) {
	holder := newFixEnv(t)
	holder.admin(t, http.MethodPut, "/site", "").must(t, "bucket")
	holder.admin(t, http.MethodPut, "/site?website", `<WebsiteConfiguration><IndexDocument><Suffix>index.html</Suffix></IndexDocument></WebsiteConfiguration>`).must(t, "website")
	holder.admin(t, http.MethodPut, "/site/index.html", "<script>1</script>", "Content-Type", "text/html").must(t, "page")

	front := newFixEnv(t)
	target, _ := url.Parse(holder.ts.URL)
	rp := httputil.NewSingleHostReverseProxy(target)
	front.h.SetClusterProxy(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
		r.Header.Set("X-VaultS3-Proxy", "front")
		rp.ServeHTTP(w, r)
		return true
	})
	req, _ := http.NewRequest(http.MethodGet, front.ts.URL+"/site/", nil)
	r := send(t, req)
	if r.code != http.StatusOK {
		t.Fatalf("website via the front node: %d %s", r.code, r.body)
	}
	if csp := r.header.Values("Content-Security-Policy"); len(csp) != 1 {
		t.Errorf("proxied website page carries %d CSP headers %q: the bare \"sandbox\" one blocks its scripts", len(csp), csp)
	}
}

// R9-8: with packing on, a small object's key is a row in the pack index, not
// a file name, so a key with a segment over 255 bytes was stored and served
// before. The new entry check refuses every method on it, DELETE included:
// such objects stored before the upgrade can no longer be read or removed
// over S3.
func TestAPackedObjectWithALongSegmentStaysReachable(t *testing.T) {
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	fs, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	packed, err := storage.NewPackedEngine(fs, 64<<10, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, packed, NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil), false, "", nil)
	ts := httptest.NewServer(h)
	defer ts.Close()
	e := &fixEnv{ts: ts, store: store, h: h}
	e.admin(t, http.MethodPut, "/pkb", "").must(t, "bucket")
	key := strings.Repeat("n", 300) + ".txt"
	// What a 5.0.13 PUT did: the engine stores it, the record is written.
	if _, _, err := packed.PutObject("pkb", key, strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("the packed engine refuses the key itself: %v", err)
	}
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: "pkb", Key: key, Size: 5, ETag: `"x"`}); err != nil {
		t.Fatal(err)
	}
	if r := e.admin(t, http.MethodGet, "/pkb/"+key, ""); r.code != http.StatusOK {
		t.Errorf("GET of an existing packed object: %d %s", r.code, errCode(r.body))
	}
	tags := `<Tagging><TagSet><Tag><Key>a</Key><Value>b</Value></Tag></TagSet></Tagging>`
	if r := e.admin(t, http.MethodPut, "/pkb/"+key+"?tagging", tags); r.code != http.StatusOK {
		t.Errorf("tagging an existing packed object: %d %s", r.code, errCode(r.body))
	}
	if r := e.admin(t, http.MethodPut, "/pkb/"+strings.Repeat("m", 300), "x"); r.code != http.StatusBadRequest || !strings.Contains(r.body, "KeyTooLongError") {
		t.Errorf("a new object under a long key: %d %s", r.code, errCode(r.body))
	}
	if r := e.admin(t, http.MethodDelete, "/pkb/"+key, ""); r.code != http.StatusNoContent {
		t.Errorf("DELETE of an existing packed object: %d %s", r.code, errCode(r.body))
	}
}

// The sandbox stopped a browser from showing a PDF (its viewer is a
// script): passive types are served without it.
func TestPassiveObjectsAreNotSandboxed(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/psv", "").must(t, "bucket")
	for ct, want := range map[string]string{
		"application/pdf": "", "image/png": "", "text/plain; charset=utf-8": "", "video/mp4": "",
		"image/svg+xml": middleware.ObjectSandboxCSP, "application/xhtml+xml": middleware.ObjectSandboxCSP,
		"text/javascript": middleware.ObjectSandboxCSP, "application/x-unknown": middleware.ObjectSandboxCSP,
	} {
		e.admin(t, http.MethodPut, "/psv/o", "x", "Content-Type", ct).must(t, "put")
		if got := e.admin(t, http.MethodGet, "/psv/o", "").header.Get("Content-Security-Policy"); got != want {
			t.Errorf("%s: CSP %q, want %q", ct, got, want)
		}
	}
}
