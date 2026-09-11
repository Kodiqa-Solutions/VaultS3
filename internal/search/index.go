package search

import (
	"container/list"
	"errors"
	"strings"
	"sync"

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

	q := ParseQuery(query)
	if q.IsEmpty() {
		return nil
	}

	var results []Result
	for _, e := range idx.entries {
		if bucket != "" && e.bucket != bucket {
			continue
		}
		if !q.Match(e.text, e.contentType, e.etag, e.tags) {
			continue
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

// Query is a parsed search string. The grammar is shared by the global search
// and the per-folder filter in the file browser so both answer the same way:
// whitespace-separated terms, all of which must match (AND); a term is a
// case-insensitive substring unless it is a "tag:key" / "tag:key=value" filter
// or a "type:<content-type substring>" filter.
type Query struct {
	tagFilters []tagFilter
	typeFilter string
	etagFilter string // lower-cased prefix of the ETag, quotes stripped
	textTerms  []string
}

// ParseQuery splits a raw query into text terms and tag:/type:/etag: filters.
// Text terms are substrings of the object's bucket, key, content type and tags;
// the ETag is only reachable through etag: so that a short numeric or hex term
// cannot collide with the 32 hex digits of every MD5.
func ParseQuery(query string) Query {
	var q Query
	for _, part := range strings.Fields(strings.TrimSpace(query)) {
		lower := strings.ToLower(part)
		if strings.HasPrefix(lower, "tag:") {
			tf := parseTagFilter(strings.TrimPrefix(lower, "tag:"))
			if tf.key != "" {
				q.tagFilters = append(q.tagFilters, tf)
				continue
			}
		}
		if strings.HasPrefix(lower, "type:") {
			q.typeFilter = strings.TrimPrefix(lower, "type:")
			continue
		}
		if strings.HasPrefix(lower, "etag:") {
			q.etagFilter = normalizeETag(strings.TrimPrefix(lower, "etag:"))
			continue
		}
		q.textTerms = append(q.textTerms, lower)
	}
	return q
}

// IsEmpty reports whether the query has no terms and no filters at all.
func (q Query) IsEmpty() bool {
	return len(q.textTerms) == 0 && len(q.tagFilters) == 0 && q.typeFilter == "" && q.etagFilter == ""
}

// Match reports whether an object described by text (already lower-cased; the
// haystack the text terms are searched in), its content type, ETag and tags
// satisfies every term and filter of the query. An etag: filter matches a
// prefix of the ETag, so a user can paste the first few characters.
func (q Query) Match(text, contentType, etag string, tags map[string]string) bool {
	if !matchTagFilters(tags, q.tagFilters) {
		return false
	}
	if q.typeFilter != "" && !strings.Contains(strings.ToLower(contentType), q.typeFilter) {
		return false
	}
	if q.etagFilter != "" && !strings.HasPrefix(normalizeETag(etag), q.etagFilter) {
		return false
	}
	for _, term := range q.textTerms {
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
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

// matchTagFilters is case-insensitive on keys as well as values, like every
// other part of the query: the whole query is lower-cased before matching, so
// a tag: filter must not be the one place that demands exact case.
func matchTagFilters(tags map[string]string, filters []tagFilter) bool {
	for _, f := range filters {
		v, ok := lookupTagFold(tags, f.key)
		if !ok {
			return false
		}
		if f.value != "" && !strings.EqualFold(v, f.value) {
			return false
		}
	}
	return true
}

func lookupTagFold(tags map[string]string, key string) (string, bool) {
	for k, v := range tags {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// normalizeETag lower-cases an ETag and strips the quotes S3 wraps it in.
func normalizeETag(etag string) string {
	return strings.Trim(strings.ToLower(etag), `"`)
}

func buildSearchText(bucket, key string, meta metadata.ObjectMeta) string {
	return SearchText(bucket, key, meta.ContentType, meta.Tags)
}

// SearchText is the lower-cased haystack plain query terms are matched against:
// bucket, key (or, for the folder filter, the child's name), content type and
// tags as k=v. The global index and the folder filter both build it here so a
// term means the same thing in both. The ETag and the modification date are
// deliberately left out — they are hex digits and numbers that random short
// terms match by accident (any four hex digits hit roughly one MD5 in 2,000).
// The ETag is reachable through the etag: filter instead.
func SearchText(bucket, key, contentType string, tags map[string]string) string {
	var b strings.Builder
	for _, part := range []string{bucket, key, contentType} {
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strings.ToLower(part))
	}
	for k, v := range tags {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(k))
		b.WriteByte('=')
		b.WriteString(strings.ToLower(v))
	}
	return b.String()
}
