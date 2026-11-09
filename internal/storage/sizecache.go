package storage

import (
	"os"
	"sync"
	"time"
)

// sizeRefresh is how old a cached bucket size may get before BucketSize walks
// the bucket again. A variable so a test can shorten it.
var sizeRefresh = time.Minute

// sizeRecheck is how soon a figure is measured again when files changed
// while it was being walked. Such a change may be counted twice (the walk
// passed the new file, and the write added it too) or not at all, so the
// figure is right to within those few changes, and only briefly.
var sizeRecheck = 5 * time.Second

// sizeCache keeps each bucket's size on disk, so BucketSize does not walk the
// whole bucket on every call. A quota check calls it on every write, and the
// metrics endpoint for every bucket on every scrape, and a bucket with
// versioning holds every version: a large one was walked file by file each
// time.
//
// The FileSystem's own writes and deletes adjust the cached figures by the
// exact bytes they add or remove. Anything that changes files without it (the
// tiering mover, the reclaim scan) is caught by the next walk, which happens
// once a figure is older than sizeRefresh, or at once after Invalidate.
//
// A quota check compares against this figure before the write, as it
// compared against a walk, so concurrent uploads could already pass it
// together; the cache does not make it exact, and does not make it looser.
type sizeCache struct {
	mu      sync.Mutex
	buckets map[string]*bucketSize
}

type bucketSize struct {
	size, count int64
	valid       bool
	at          time.Time
	// While a walk runs, changes are also kept here and added to its
	// result: the walk may already have passed the file that changed.
	walking        bool
	changed        bool // something changed during the walk
	dSize, dCount  int64
	done           chan struct{}
	invalidatedNow bool
}

func (c *sizeCache) entry(bucket string) *bucketSize {
	if c.buckets == nil {
		c.buckets = map[string]*bucketSize{}
	}
	e := c.buckets[bucket]
	if e == nil {
		e = &bucketSize{}
		c.buckets[bucket] = e
	}
	return e
}

// get returns the cached size, walking the bucket with walk when there is
// no fresh figure. One walk runs per bucket at a time; a caller that finds
// one running takes the previous figure, or waits for the first.
func (c *sizeCache) get(bucket string, walk func() (int64, int64, error)) (int64, int64, error) {
	c.mu.Lock()
	e := c.entry(bucket)
	for {
		if e.valid && !e.invalidatedNow && time.Since(e.at) < e.ttl() {
			s, n := e.size, e.count
			c.mu.Unlock()
			return s, n, nil
		}
		if !e.walking {
			break
		}
		if e.valid {
			s, n := e.size, e.count
			c.mu.Unlock()
			return s, n, nil
		}
		done := e.done
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		if c.buckets[bucket] != e { // the bucket was deleted meanwhile
			e = c.entry(bucket)
		}
	}
	e.walking, e.changed, e.dSize, e.dCount, e.invalidatedNow = true, false, 0, 0, false
	e.done = make(chan struct{})
	c.mu.Unlock()

	size, count, err := walk()

	c.mu.Lock()
	defer c.mu.Unlock()
	e.walking = false
	close(e.done)
	if err != nil {
		return 0, 0, err
	}
	e.size, e.count = max(0, size+e.dSize), max(0, count+e.dCount)
	e.valid, e.at = true, time.Now()
	return e.size, e.count, nil
}

func (e *bucketSize) ttl() time.Duration {
	if e.changed {
		return min(sizeRecheck, sizeRefresh)
	}
	return sizeRefresh
}

// adjust records a change made through the FileSystem.
func (c *sizeCache) adjust(bucket string, dSize, dCount int64) {
	if dSize == 0 && dCount == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.buckets[bucket]
	if e == nil {
		return
	}
	if e.walking {
		e.dSize += dSize
		e.dCount += dCount
		e.changed = true
	}
	if e.valid {
		e.size, e.count = max(0, e.size+dSize), max(0, e.count+dCount)
	}
}

// invalidate makes the next get walk the bucket.
func (c *sizeCache) invalidate(bucket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.buckets[bucket]; e != nil {
		e.invalidatedNow = true
	}
}

// drop forgets a deleted bucket.
func (c *sizeCache) drop(bucket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.buckets, bucket)
}

// regularSize is the size of the regular file at path, and whether there is
// one.
func regularSize(path string) (int64, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	return fi.Size(), true
}
