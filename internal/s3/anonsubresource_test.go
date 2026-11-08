package s3

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// Regression (5.0.11, present in 5.0.10): an anonymous s3:GetObject grant
// waived authentication for every GET of a key, so a public bucket answered an
// anonymous ?tagging, ?acl, ?attributes, ?legal-hold, ?uploadId and
// ?versionId too: every object's tags, and every old version to anyone who
// knew its ID, even after the key was deleted. Only the read of the object
// itself is public now.
func TestAnonymousGetObjectGrantCoversOnlyTheObject(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pubv", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/pubv?versioning", `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`).must(t, "versioning")
	old := e.admin(t, http.MethodPut, "/pubv/doc", "OLD-SECRET-DRAFT", "X-Amz-Tagging", "secret=topsecret")
	old.must(t, "old version")
	oldID := old.header.Get("X-Amz-Version-Id")
	e.admin(t, http.MethodPut, "/pubv/doc", "published", "X-Amz-Tagging", "secret=topsecret").must(t, "current version")
	up := e.startUpload(t, "pubv", "doc")
	if err := e.store.PutBucketPolicy("pubv", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pubv/*"}]}`)); err != nil {
		t.Fatal(err)
	}
	anon := func(method, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, e.ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := anon(http.MethodGet, "/pubv/doc"); code != http.StatusOK || body != "published" {
		t.Fatalf("anonymous GET of the public object: %d %q, want 200", code, body)
	}
	if code, _ := anon(http.MethodHead, "/pubv/doc"); code != http.StatusOK {
		t.Errorf("anonymous HEAD of the public object: %d, want 200", code)
	}
	for _, q := range []string{"?tagging", "?acl", "?attributes", "?legal-hold", "?retention", "?uploadId=" + up, "?versionId=" + oldID} {
		code, body := anon(http.MethodGet, "/pubv/doc"+q)
		if code != http.StatusForbidden || strings.Contains(body, "topsecret") || strings.Contains(body, "OLD-SECRET") {
			t.Errorf("anonymous GET %s: %d %s, want 403 (only s3:GetObject is public)", q, code, regexp.MustCompile(`\s+`).ReplaceAllString(body, " "))
		}
	}
	if code, _ := anon(http.MethodHead, "/pubv/doc?versionId="+oldID); code != http.StatusForbidden {
		t.Errorf("anonymous HEAD ?versionId: %d, want 403", code)
	}
	// After a delete marker hides the key, its old version is not readable.
	e.admin(t, http.MethodDelete, "/pubv/doc", "").must(t, "delete marker")
	if code, body := anon(http.MethodGet, "/pubv/doc?versionId="+oldID); code == http.StatusOK {
		t.Errorf("anonymous GET of an old version after a delete: 200 %q", body)
	}
}

// A policy that grants one of those actions to everyone makes it public, as
// on AWS (5.0.11 round 7): s3:GetObjectVersion and s3:GetObjectTagging granted
// to "*" serve ?versionId and ?tagging anonymously, and nothing else is opened.
func TestAnonymousGrantOfAnotherReadActionIsHonoured(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pubx", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/pubx?versioning", `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`).must(t, "versioning")
	old := e.admin(t, http.MethodPut, "/pubx/doc", "old", "X-Amz-Tagging", "a=b")
	old.must(t, "old")
	e.admin(t, http.MethodPut, "/pubx/doc", "new").must(t, "new")
	if err := e.store.PutBucketPolicy("pubx", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:GetObjectVersion","s3:GetObjectTagging"],"Resource":"arn:aws:s3:::pubx/*"}]}`)); err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/pubx/doc?versionId=" + old.header.Get("X-Amz-Version-Id")); code != http.StatusOK || body != "old" {
		t.Errorf("anonymous ?versionId with s3:GetObjectVersion granted: %d %q, want 200 old", code, body)
	}
	if code, _ := get("/pubx/doc?tagging"); code != http.StatusOK {
		t.Errorf("anonymous ?tagging with s3:GetObjectTagging granted: %d, want 200", code)
	}
	if code, _ := get("/pubx/doc?acl"); code != http.StatusForbidden {
		t.Errorf("anonymous ?acl, not granted: %d, want 403", code)
	}
}

// As on AWS, an anonymous read cannot override the response headers, which
// made any public object a text/html page on the dashboard's origin.
func TestAnonymousReadCannotOverrideResponseHeaders(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pubo", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/pubo/img.png", "<script>alert(1)</script>", "Content-Type", "image/png").must(t, "object")
	if err := e.store.PutBucketPolicy("pubo", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pubo/*"}]}`)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(e.ts.URL + "/pubo/img.png?response-content-type=text/html")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Content-Type") == "text/html" {
		t.Errorf("anonymous response-content-type override: %d %q, want 400", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// A made-up signature does not get an override past the check: such a
	// request is authenticated, and refused (5.0.11 round 8).
	for name, set := range map[string]func(*http.Request){
		"a made-up Authorization header": func(r *http.Request) { r.Header.Set("Authorization", "garbage") },
		"a made-up X-Amz-Signature":      func(r *http.Request) { q := r.URL.Query(); q.Set("X-Amz-Signature", "00"); r.URL.RawQuery = q.Encode() },
	} {
		req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/pubo/img.png?response-content-type=text/html", nil)
		set(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK || resp.Header.Get("Content-Type") == "text/html" {
			t.Errorf("an override with %s: %d %q, want it refused", name, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	if got := e.admin(t, http.MethodGet, "/pubo/img.png?response-content-type=text/plain", ""); got.header.Get("Content-Type") != "text/plain" {
		t.Errorf("a signed override: Content-Type %q, want text/plain", got.header.Get("Content-Type"))
	}
}

// A signed request is authenticated even on a public object, and the public
// grant then covers it, as a "Principal":"*" grant does on AWS (5.0.11 round
// 9): a valid signed override from a key with no grant of its own was refused.
// An explicit Deny in the key's own policy still wins.
func TestPublicGrantCoversSignedCallersUnlessDenied(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/pubs", "").must(t, "bucket")
	e.admin(t, http.MethodPut, "/pubs/f.txt", "hello", "Content-Type", "text/plain").must(t, "object")
	if err := e.store.PutBucketPolicy("pubs", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pubs/*"}]}`)); err != nil {
		t.Fatal(err)
	}
	ak, sk := e.addUser(t, "elsewhere", allow(`"s3:GetObject"`, `"arn:aws:s3:::other/*"`))
	if got := e.as(t, ak, sk, http.MethodGet, "/pubs/f.txt?response-content-type=application/octet-stream", ""); got.code != http.StatusOK || got.header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("a signed override from a key the public grant covers: %d %q, want 200 with the override", got.code, got.header.Get("Content-Type"))
	}
	dk, dsk := e.addUser(t, "denied", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"s3:GetObject","Resource":"arn:aws:s3:::pubs/*"}]}`)
	if got := e.as(t, dk, dsk, http.MethodGet, "/pubs/f.txt?response-content-type=text/html", ""); got.code != http.StatusForbidden {
		t.Errorf("a signed override from a key its own policy denies: %d, want 403", got.code)
	}
}

// Regression (5.0.11 round 10): a key beginning with .vs/ or .ec/, or with a
// segment beginning .vaults3-tmp-, named the storage engine's own files: a
// write of ".vs/X/null" replaced X's content, and such keys vanished from
// listings. They are refused on every way a key enters.
func TestReservedKeysAreRefused(t *testing.T) {
	e := newFixEnv(t)
	e.admin(t, http.MethodPut, "/rsv", "").must(t, "bucket")
	for _, key := range []string{".vs", ".vs/X/null", ".ec/a", "a/.vaults3-tmp-123"} {
		if r := e.admin(t, http.MethodPut, "/rsv/"+key, "x"); r.code != http.StatusBadRequest {
			t.Errorf("PUT %s: %d %s, want 400", key, r.code, r.body)
		}
	}
	e.admin(t, http.MethodPut, "/rsv/a/.vs/b", "x").must(t, ".vs below the top level is an ordinary name")
}
