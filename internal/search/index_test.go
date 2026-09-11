package search

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

func newTestIndex(max int) *Index {
	return NewIndex(nil, max)
}

func TestIndex_UpdateAndSearch(t *testing.T) {
	idx := newTestIndex(100)

	idx.Update("mybucket", "docs/readme.txt", metadata.ObjectMeta{
		Size:         100,
		ContentType:  "text/plain",
		LastModified: 1700000000,
		ETag:         "abc123",
	})

	results := idx.Search("readme", "", 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Key != "docs/readme.txt" {
		t.Errorf("expected key docs/readme.txt, got %s", results[0].Key)
	}
}

func TestIndex_SearchByContentType(t *testing.T) {
	idx := newTestIndex(100)

	idx.Update("mybucket", "photo.jpg", metadata.ObjectMeta{
		ContentType: "image/jpeg",
	})
	idx.Update("mybucket", "doc.pdf", metadata.ObjectMeta{
		ContentType: "application/pdf",
	})

	results := idx.Search("type:image", "", 10)
	if len(results) != 1 || results[0].Key != "photo.jpg" {
		t.Errorf("expected photo.jpg for type:image, got %v", results)
	}
}

func TestIndex_SearchByTag(t *testing.T) {
	idx := newTestIndex(100)

	idx.Update("mybucket", "tagged.txt", metadata.ObjectMeta{
		Tags: map[string]string{"env": "prod", "team": "backend"},
	})
	idx.Update("mybucket", "other.txt", metadata.ObjectMeta{
		Tags: map[string]string{"env": "dev"},
	})

	results := idx.Search("tag:env=prod", "", 10)
	if len(results) != 1 || results[0].Key != "tagged.txt" {
		t.Errorf("expected tagged.txt for tag:env=prod, got %v", results)
	}
}

func TestIndex_BucketFilter(t *testing.T) {
	idx := newTestIndex(100)

	idx.Update("bucket1", "file.txt", metadata.ObjectMeta{ContentType: "text/plain"})
	idx.Update("bucket2", "file.txt", metadata.ObjectMeta{ContentType: "text/plain"})

	results := idx.Search("file", "bucket1", 10)
	if len(results) != 1 || results[0].Bucket != "bucket1" {
		t.Errorf("expected only bucket1, got %v", results)
	}
}

func TestIndex_Remove(t *testing.T) {
	idx := newTestIndex(100)

	idx.Update("mybucket", "file.txt", metadata.ObjectMeta{ContentType: "text/plain"})
	idx.Remove("mybucket", "file.txt")

	results := idx.Search("file", "", 10)
	if len(results) != 0 {
		t.Errorf("expected 0 results after remove, got %d", len(results))
	}
}

func TestIndex_LRUEviction(t *testing.T) {
	idx := newTestIndex(3)

	idx.Update("b", "1.txt", metadata.ObjectMeta{})
	idx.Update("b", "2.txt", metadata.ObjectMeta{})
	idx.Update("b", "3.txt", metadata.ObjectMeta{})
	idx.Update("b", "4.txt", metadata.ObjectMeta{}) // should evict 1.txt

	if idx.Count() != 3 {
		t.Errorf("expected count=3 after eviction, got %d", idx.Count())
	}

	results := idx.Search("1.txt", "", 10)
	if len(results) != 0 {
		t.Error("expected 1.txt to be evicted")
	}

	results = idx.Search("4.txt", "", 10)
	if len(results) != 1 {
		t.Error("expected 4.txt to exist")
	}
	if !idx.Truncated() {
		t.Error("an index that evicted on Update must report truncated")
	}
}

func TestSearchText(t *testing.T) {
	got := SearchText("", "Report.PDF", "application/pdf", map[string]string{"Env": "Prod"})
	if got != "report.pdf application/pdf env=prod" {
		t.Fatalf("SearchText = %q", got)
	}
	if got := SearchText("", "folder", "", nil); got != "folder" {
		t.Fatalf("SearchText(folder) = %q", got)
	}
}

func TestIndex_EmptySearch(t *testing.T) {
	idx := newTestIndex(100)
	results := idx.Search("", "", 10)
	if results != nil {
		t.Errorf("expected nil for empty query, got %v", results)
	}
}

func TestIndex_Count(t *testing.T) {
	idx := newTestIndex(100)
	if idx.Count() != 0 {
		t.Error("expected 0 on empty index")
	}
	idx.Update("b", "k", metadata.ObjectMeta{})
	if idx.Count() != 1 {
		t.Error("expected 1 after update")
	}
}

func TestParseQuery_Match(t *testing.T) {
	tags := map[string]string{"env": "prod"}
	cases := []struct {
		q                       string
		text, contentType, etag string
		tags                    map[string]string
		want                    bool
	}{
		{"aa", "aa-1", "", "", nil, true},
		{"AA", "aa-1", "", "", nil, true}, // the query is lower-cased; text must already be
		{"aa", "bb-1", "", "", nil, false},
		{"aa 1", "aa-1", "", "", nil, true},  // terms AND together
		{"aa 2", "aa-1", "", "", nil, false}, // terms AND together
		{"type:pdf", "x", "application/pdf", "", nil, true},
		{"type:pdf", "x", "text/plain", "", nil, false},
		{"type:pdf", "folder", "", "", nil, false}, // folders have no type
		{"tag:env", "x", "", "", tags, true},
		{"tag:env=PROD", "x", "", "", tags, true},
		{"tag:env=dev", "x", "", "", tags, false},
		{"tag:env", "folder", "", "", nil, false},
		{"tag:", "tag:", "", "", nil, true},                                     // a bare "tag:" is just text
		{"etag:D41D", "x", "", `"d41d8cd98f00b204e9800998ecf8427e"`, nil, true}, // prefix, case-insensitive, quotes ignored
		{"etag:8cd9", "x", "", "d41d8cd98f00b204e9800998ecf8427e", nil, false},  // a prefix, not a substring
		{"etag:d41d", "folder", "", "", nil, false},                             // folders have no ETag
		// Filter prefixes, tag keys and tag values are all case-insensitive.
		{"Tag:env=PROD", "x", "", "", tags, true},
		{"Type:PDF", "x", "application/pdf", "", nil, true},
		{"ETag:D41D", "x", "", "d41d8cd98f00b204e9800998ecf8427e", nil, true},
		{"TAG:Env", "x", "", "", map[string]string{"Env": "1"}, true},
		{"tag:env=prod", "x", "", "", map[string]string{"ENV": "Prod"}, true},
	}
	for _, c := range cases {
		if got := ParseQuery(c.q).Match(c.text, c.contentType, c.etag, c.tags); got != c.want {
			t.Errorf("ParseQuery(%q).Match(%q, %q, %q, %v) = %v, want %v", c.q, c.text, c.contentType, c.etag, c.tags, got, c.want)
		}
	}
	if !ParseQuery("   ").IsEmpty() || ParseQuery("x").IsEmpty() || ParseQuery("type:x").IsEmpty() || ParseQuery("etag:x").IsEmpty() {
		t.Error("IsEmpty is wrong")
	}
	// A filter with nothing after the colon is no query at all — it used to
	// match every object.
	if !ParseQuery("type:").IsEmpty() || !ParseQuery("etag:").IsEmpty() {
		t.Error("an empty filter must be an empty query")
	}
}

func newBuiltIndex(t *testing.T, objects, max int) *Index {
	t.Helper()
	store, err := metadata.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < objects; i++ {
		if err := store.PutObjectMeta(metadata.ObjectMeta{
			Bucket: "b", Key: fmt.Sprintf("docs/file-%04d.txt", i), Size: 1, ContentType: "text/plain",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A delete marker is not an object.
	if err := store.PutObjectMeta(metadata.ObjectMeta{Bucket: "b", Key: "gone", DeleteMarker: true}); err != nil {
		t.Fatal(err)
	}
	idx := NewIndex(store, max)
	if err := idx.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return idx
}

func TestIndex_BuildFromStore(t *testing.T) {
	idx := newBuiltIndex(t, 25, 100)
	if idx.Count() != 25 {
		t.Fatalf("Count = %d, want 25", idx.Count())
	}
	if idx.Truncated() {
		t.Fatal("index under the cap reported truncated")
	}
	if got := idx.Search("file-0007", "b", 10); len(got) != 1 || got[0].Key != "docs/file-0007.txt" {
		t.Fatalf("Search = %+v", got)
	}
	if got := idx.Search("gone", "", 10); len(got) != 0 {
		t.Fatalf("delete marker was indexed: %+v", got)
	}
}

func TestIndex_BuildStopsAtTheCap(t *testing.T) {
	idx := newBuiltIndex(t, 25, 10)
	if idx.Count() != 10 {
		t.Fatalf("Count = %d, want the cap 10", idx.Count())
	}
	if !idx.Truncated() {
		t.Fatal("an index that stopped scanning during Build must report truncated")
	}
	// The scan stops once full, so the index holds the first ten keys in store
	// order rather than the last ten.
	if got := idx.Search("file-0009", "", 10); len(got) != 1 {
		t.Fatalf("file-0009 (inside the cap) not indexed: %+v", got)
	}
	if got := idx.Search("file-0010", "", 10); len(got) != 0 {
		t.Fatalf("file-0010 (past the cap) was indexed: %+v", got)
	}
}

// Plain terms must not match the ETag or the modification date: "4435" against a
// store of MD5s hits one object in about two thousand by accident.
func TestIndex_PlainTermsIgnoreETagAndDate(t *testing.T) {
	idx := newTestIndex(100)
	idx.Update("b", "photo.png", metadata.ObjectMeta{
		ContentType:  "image/png",
		ETag:         `"ab4435cd98f00b204e9800998ecf8427e"`,
		LastModified: 1700000000, // 2023-11-14
	})
	for _, q := range []string{"4435", "2023-11-14", "2023"} {
		if got := idx.Search(q, "", 10); len(got) != 0 {
			t.Errorf("Search(%q) matched via ETag/date: %+v", q, got)
		}
	}
	if got := idx.Search("etag:AB4435", "", 10); len(got) != 1 {
		t.Errorf("etag: prefix filter did not match: %+v", got)
	}
	if got := idx.Search("photo etag:ab44", "", 10); len(got) != 1 {
		t.Errorf("etag: does not AND with text terms: %+v", got)
	}
}
