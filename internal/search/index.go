package search

import (
	"container/list"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Result represents a search result entry.
type Result struct {
	Bucket       string            `json:"bucket"`
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	ContentType  string            `json:"content_type"`
	LastModified int64             `json:"last_modified"`
	ETag         string            `json:"etag"`
	Tags         map[string]string `json:"tags,omitempty"`
}

// entry is an internal index entry with minimal fields.
type entry struct {
	bucket       string
	key          string
	size         int64
	contentType  string
	lastModified int64
	etag         string
	tags         map[string]string
	text         string
}

// Index provides in-memory full-text search over object metadata.
type Index struct {
	mu         sync.RWMutex
	entries    map[string]*entry
	lru        *list.List
	lruElems   map[string]*list.Element
	store      metadata.StoreAPI
	maxEntries int
	truncated  bool // the cap dropped objects (Build stopped early or Update evicted): incomplete
}

const defaultMaxSearchEntries = 50000

// NewIndex creates a new search index with the given max entries cap.
func NewIndex(store metadata.StoreAPI, maxEntries int) *Index {
	if maxEntries <= 0 {
		maxEntries = defaultMaxSearchEntries
	}
	return &Index{
		entries:    make(map[string]*entry, maxEntries),
		lru:        list.New(),
		lruElems:   make(map[string]*list.Element, maxEntries),
		store:      store,
		maxEntries: maxEntries,
	}
}

// Build populates the index by scanning object metadata from BoltDB. The scan
// stops as soon as the index is full: an index that keeps whichever maxEntries
// objects it saw first is no less arbitrary than one that keeps whichever it saw
// last, and stopping avoids decoding and evicting every object past the cap —
// at 250k objects and a 50k cap that is four fifths of the scan. The store
// iterates in bucket/key order, so on a capped store the buckets that sort last
// are the ones left out; raise memory.max_search_entries above the object
// count for a complete index.
func (idx *Index) Build() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.entries = make(map[string]*entry, idx.maxEntries)
	idx.lru = list.New()
	idx.lruElems = make(map[string]*list.Element, idx.maxEntries)
	idx.truncated = false

	err := idx.store.IterateAllObjects(func(bucket, key string, meta metadata.ObjectMeta) bool {
		if meta.DeleteMarker {
			return true // skip delete markers
		}
		if len(idx.entries) >= idx.maxEntries {
			idx.truncated = true
			return false // full: stop scanning
		}
		mk := bucket + "/" + key
		idx.entries[mk] = newEntry(bucket, key, meta)
		idx.lruElems[mk] = idx.lru.PushFront(mk)
		return true
	})

	if errors.Is(err, metadata.ErrStopIteration) {
		return nil
	}
	return err
}

// Update adds or updates an entry in the index.
func (idx *Index) Update(bucket, key string, meta metadata.ObjectMeta) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	mk := bucket + "/" + key

	if meta.DeleteMarker {
		idx.evictLocked(mk)
		return
	}

	if elem, exists := idx.lruElems[mk]; exists {
		idx.lru.MoveToFront(elem)
	} else {
		elem := idx.lru.PushFront(mk)
		idx.lruElems[mk] = elem
	}

	idx.entries[mk] = newEntry(bucket, key, meta)

	for len(idx.entries) > idx.maxEntries {
		oldest := idx.lru.Back()
		if oldest == nil {
			break
		}
		idx.truncated = true
		idx.evictLocked(oldest.Value.(string))
	}
}

// Remove deletes an entry from the index.
func (idx *Index) Remove(bucket, key string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.evictLocked(bucket + "/" + key)
}

func (idx *Index) evictLocked(mk string) {
	if elem, ok := idx.lruElems[mk]; ok {
		idx.lru.Remove(elem)
		delete(idx.lruElems, mk)
	}
	delete(idx.entries, mk)
}

// Search finds objects matching the query string.
func (idx *Index) Search(query, bucket string, limit int) []Result {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}

	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}

	var tagFilters []tagFilter
	var typeFilter string
	var textTerms []string

	for _, part := range strings.Fields(query) {
		lower := strings.ToLower(part)
		if strings.HasPrefix(lower, "tag:") {
			tf := parseTagFilter(strings.TrimPrefix(part, "tag:"))
			if tf.key != "" {
				tagFilters = append(tagFilters, tf)
				continue
			}
		}
		if strings.HasPrefix(lower, "type:") {
			typeFilter = strings.ToLower(strings.TrimPrefix(part, "type:"))
			continue
		}
		textTerms = append(textTerms, strings.ToLower(part))
	}

	var results []Result
	for _, e := range idx.entries {
		if bucket != "" && e.bucket != bucket {
			continue
		}

		if !matchTagFilters(e.tags, tagFilters) {
			continue
		}

		if typeFilter != "" && !strings.Contains(strings.ToLower(e.contentType), typeFilter) {
			continue
		}

		if len(textTerms) > 0 {
			allMatch := true
			for _, term := range textTerms {
				if !strings.Contains(e.text, term) {
					allMatch = false
					break
				}
			}
			if !allMatch {
				continue
			}
		}

		results = append(results, Result{
			Bucket:       e.bucket,
			Key:          e.key,
			Size:         e.size,
			ContentType:  e.contentType,
			LastModified: e.lastModified,
			ETag:         e.etag,
			Tags:         e.tags,
		})

		if len(results) >= limit {
			break
		}
	}

	return results
}

// Count returns the number of indexed entries.
func (idx *Index) Count() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.entries)
}

// Truncated reports whether the index has dropped objects — because Build
// stopped at the entry cap, or because a later Update evicted the least recently
// touched entry to stay under it — so that searches may miss objects.
func (idx *Index) Truncated() bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.truncated
}

func newEntry(bucket, key string, meta metadata.ObjectMeta) *entry {
	return &entry{
		bucket:       bucket,
		key:          key,
		size:         meta.Size,
		contentType:  meta.ContentType,
		lastModified: meta.LastModified,
		etag:         meta.ETag,
		tags:         meta.Tags,
		text:         buildSearchText(bucket, key, meta),
	}
}

type tagFilter struct {
	key   string
	value string
}

func parseTagFilter(s string) tagFilter {
	parts := strings.SplitN(s, "=", 2)
	tf := tagFilter{key: parts[0]}
	if len(parts) == 2 {
		tf.value = parts[1]
	}
	return tf
}

func matchTagFilters(tags map[string]string, filters []tagFilter) bool {
	for _, f := range filters {
		v, ok := tags[f.key]
		if !ok {
			return false
		}
		if f.value != "" && !strings.EqualFold(v, f.value) {
			return false
		}
	}
	return true
}

func buildSearchText(bucket, key string, meta metadata.ObjectMeta) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(bucket))
	b.WriteByte(' ')
	b.WriteString(strings.ToLower(key))
	b.WriteByte(' ')
	b.WriteString(strings.ToLower(meta.ContentType))
	b.WriteByte(' ')
	b.WriteString(strings.ToLower(meta.ETag))
	for k, v := range meta.Tags {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(k))
		b.WriteByte('=')
		b.WriteString(strings.ToLower(v))
	}
	if meta.LastModified > 0 {
		b.WriteByte(' ')
		b.WriteString(time.Unix(meta.LastModified, 0).Format("2006-01-02"))
	}
	return b.String()
}
