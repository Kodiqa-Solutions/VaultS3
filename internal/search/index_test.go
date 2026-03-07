package search

import (
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
