package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// The file browser's folder filter must only see the folder's direct children:
// in a/ the query "aa" finds a/aa-1 and a/aa-2, not a/bb-1/aa.pdf.
func newSearchFixture(t *testing.T) (*APIHandler, string) {
	t.Helper()
	h, store := newTestAPI(t)
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	put := func(key, ctype string, tags map[string]string) {
		if err := store.PutObjectMeta(metadata.ObjectMeta{
			Bucket: "b", Key: key, Size: 1, ContentType: ctype, LastModified: 1700000000, Tags: tags,
			ETag: `"` + "e7ag" + key + `"`,
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("a/aa-1", "text/plain", nil)
	put("a/AA-2.pdf", "application/pdf", map[string]string{"env": "prod"})
	put("a/bb-1/aa.pdf", "application/pdf", nil)
	put("a/bb-1/other", "text/plain", nil)
	put("a/cc-1/deep/aa.txt", "text/plain", nil)
	put("aardvark", "text/plain", nil)
	return h, getToken(t, h)
}

func searchFolder(t *testing.T, h *APIHandler, token string, params url.Values) (int, objectListResponse) {
	t.Helper()
	rr := doRequest(h, "GET", "/buckets/b/search?"+params.Encode(), nil, token)
	var resp objectListResponse
	if rr.Code == http.StatusOK {
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rr.Code, resp
}

func keysOf(items []objectListItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Key)
	}
	return out
}

func assertKeys(t *testing.T, got []objectListItem, want ...string) {
	t.Helper()
	g := keysOf(got)
	if len(g) != len(want) {
		t.Fatalf("got %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("got %v, want %v", g, want)
		}
	}
}

func TestSearchObjects_MatchesDirectChildrenOnly(t *testing.T) {
	h, token := newSearchFixture(t)

	code, resp := searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"aa"}})
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	// Case-insensitive, name only: bb-1/aa.pdf and cc-1/deep/aa.txt are not
	// direct children, and the folders bb-1 / cc-1 themselves do not contain "aa".
	assertKeys(t, resp.Objects, "a/AA-2.pdf", "a/aa-1")
	if resp.Truncated || resp.NextStartAfter != "" {
		t.Fatalf("a folder scanned to its end must not be truncated: %+v", resp)
	}
	for _, it := range resp.Objects {
		if it.IsPrefix {
			t.Fatalf("%s reported as a folder", it.Key)
		}
	}
}

func TestSearchObjects_MatchesFolderNames(t *testing.T) {
	h, token := newSearchFixture(t)
	_, resp := searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"bb"}})
	assertKeys(t, resp.Objects, "a/bb-1/")
	if !resp.Objects[0].IsPrefix {
		t.Fatal("bb-1 must be reported as a folder")
	}
}

func TestSearchObjects_RootPrefixMatchesOnFullChildName(t *testing.T) {
	h, token := newSearchFixture(t)
	// At the root, "aa" matches the file aardvark but not the folder a/ — the
	// folder's name is just "a".
	_, resp := searchFolder(t, h, token, url.Values{"q": {"aa"}})
	assertKeys(t, resp.Objects, "aardvark")
}

func TestSearchObjects_FiltersAreTheGlobalSearchGrammar(t *testing.T) {
	h, token := newSearchFixture(t)

	// type: applies to files only; a folder has no content type.
	_, resp := searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"type:pdf"}})
	assertKeys(t, resp.Objects, "a/AA-2.pdf")

	// Terms AND together.
	_, resp = searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"aa type:plain"}})
	assertKeys(t, resp.Objects, "a/aa-1")

	// tag:k=v
	_, resp = searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"tag:env=prod"}})
	assertKeys(t, resp.Objects, "a/AA-2.pdf")

	_, resp = searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"tag:env=staging"}})
	assertKeys(t, resp.Objects)

	// Plain terms see the content type and tags too, as the global search does.
	_, resp = searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"prod"}})
	assertKeys(t, resp.Objects, "a/AA-2.pdf")
	_, resp = searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"application"}})
	assertKeys(t, resp.Objects, "a/AA-2.pdf")

	// etag: is a prefix filter on the ETag; plain terms never see the ETag.
	_, resp = searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"etag:E7AGa/aa-1"}})
	assertKeys(t, resp.Objects, "a/aa-1")
	_, resp = searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"e7ag"}})
	assertKeys(t, resp.Objects)
}

func TestSearchObjects_RequiresQuery(t *testing.T) {
	h, token := newSearchFixture(t)
	if code, _ := searchFolder(t, h, token, url.Values{"prefix": {"a/"}}); code != http.StatusBadRequest {
		t.Fatalf("missing q: status %d, want 400", code)
	}
	if code, _ := searchFolder(t, h, token, url.Values{"prefix": {"a/"}, "q": {"   "}}); code != http.StatusBadRequest {
		t.Fatalf("blank q: status %d, want 400", code)
	}
	rr := doRequest(h, "GET", "/buckets/nope/search?q=x", nil, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown bucket: status %d, want 404", rr.Code)
	}
}

// A page fills with maxKeys matches and hands back a cursor from which the
// next call continues, so a client can page through a folder with many hits.
func TestSearchObjects_PagesThroughMatches(t *testing.T) {
	h, token := newSearchFixture(t)
	var all []string
	cursor := ""
	for i := 0; i < 10; i++ {
		params := url.Values{"prefix": {"a/"}, "q": {"-"}, "maxKeys": {"1"}}
		if cursor != "" {
			params.Set("startAfter", cursor)
		}
		_, resp := searchFolder(t, h, token, params)
		if len(resp.Objects) > 1 {
			t.Fatalf("maxKeys=1 but page holds %d items: %v", len(resp.Objects), keysOf(resp.Objects))
		}
		all = append(all, keysOf(resp.Objects)...)
		if !resp.Truncated {
			break
		}
		if resp.NextStartAfter == "" {
			t.Fatal("truncated without a cursor")
		}
		cursor = resp.NextStartAfter
	}
	// Every direct child of a/ has a "-" in its name.
	want := map[string]bool{"a/aa-1": true, "a/AA-2.pdf": true, "a/bb-1/": true, "a/cc-1/": true}
	if len(all) != len(want) {
		t.Fatalf("paged keys %v, want the 4 children of a/", all)
	}
	for _, k := range all {
		if !want[k] {
			t.Fatalf("unexpected key %s in %v", k, all)
		}
	}
}

func TestSearchObjects_RequiresAuth(t *testing.T) {
	h, _ := newSearchFixture(t)
	rr := doRequest(h, "GET", "/buckets/b/search?q=aa", nil, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rr.Code)
	}
}
