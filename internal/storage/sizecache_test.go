package storage

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func sizeOf(t *testing.T, fs *FileSystem, bucket string) (int64, int64) {
	t.Helper()
	s, n, err := fs.BucketSize(bucket)
	if err != nil {
		t.Fatal(err)
	}
	return s, n
}

// The walk used to run on every call. Writes and deletes through the
// FileSystem now move the cached figure by their exact bytes, so it matches
// a fresh walk without walking.
func TestBucketSizeCacheFollowsWritesAndDeletes(t *testing.T) {
	fs, err := NewFileSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs.CreateBucketDir("b")
	put := func(key, data string) {
		if _, _, err := fs.PutObject("b", key, strings.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
	}
	put("a", "12345")
	if s, n := sizeOf(t, fs, "b"); s != 5 || n != 1 {
		t.Fatalf("first measure %d/%d", s, n)
	}
	// From here on a walk would see a file planted behind the cache's back;
	// the cached figure must not.
	os.WriteFile(fs.ObjectPath("b", "planted"), []byte("xxxxxxxxxx"), 0o644)

	put("b/c", "123")                                                 // new: +3, +1
	put("a", "1234567")                                               // replace: +2
	fs.PutObjectVersion("b", "v", "0001", strings.NewReader("vv"), 2) // +2, +1
	fs.DeleteObject("b", "b/c")                                       // -3, -1
	fs.DeleteObject("b", "missing")                                   // nothing
	fs.DeleteObjectVersion("b", "v", "0001")                          // -2, -1
	fs.PutObject("b", "dir/", strings.NewReader(""), 0)               // a marker holds no file
	if s, n := sizeOf(t, fs, "b"); s != 7 || n != 1 {
		t.Fatalf("cached figure %d/%d, want 7/1 (the planted file not counted yet)", s, n)
	}
	// Invalidate re-measures, and finds the planted file.
	fs.InvalidateBucketSize("b")
	if s, n := sizeOf(t, fs, "b"); s != 17 || n != 2 {
		t.Fatalf("after invalidate %d/%d, want 17/2", s, n)
	}
	// So does age.
	os.WriteFile(fs.ObjectPath("b", "planted2"), []byte("y"), 0o644)
	prev := sizeRefresh
	sizeRefresh = 0
	defer func() { sizeRefresh = prev }()
	if s, n := sizeOf(t, fs, "b"); s != 18 || n != 3 {
		t.Fatalf("after expiry %d/%d, want 18/3", s, n)
	}
	sizeRefresh = prev

	// A deleted and recreated bucket starts from nothing.
	fs.DeleteBucketDir("b")
	fs.CreateBucketDir("b")
	if s, n := sizeOf(t, fs, "b"); s != 0 || n != 0 {
		t.Fatalf("recreated bucket %d/%d", s, n)
	}
}

// Writes racing walks leave a figure that may be off by those writes, and
// that is measured again soon after, so it settles on the walked figure.
func TestBucketSizeCacheUnderConcurrentWrites(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	fs.CreateBucketDir("b")
	for i := range 100 {
		fs.PutObject("b", "seed/"+strings.Repeat("x", i+1), strings.NewReader("seed"), 4)
	}
	prevRefresh, prevRecheck := sizeRefresh, sizeRecheck
	sizeRefresh, sizeRecheck = time.Millisecond, time.Millisecond
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 30 {
				k := "w/" + string(rune('a'+w)) + "/" + strings.Repeat("k", i+1)
				fs.PutObject("b", k, strings.NewReader("data"), 4)
				if i%3 == 0 {
					fs.DeleteObject("b", k)
				}
				fs.BucketSize("b")
			}
		}()
	}
	wg.Wait()
	sizeRefresh, sizeRecheck = time.Hour, 20*time.Millisecond
	defer func() { sizeRefresh, sizeRecheck = prevRefresh, prevRecheck }()
	sizeOf(t, fs, "b") // a walk with nothing changing
	time.Sleep(30 * time.Millisecond)
	cached, cn := sizeOf(t, fs, "b")
	walked, wn, _ := fs.walkBucketSize("b")
	if cached != walked || cn != wn {
		t.Errorf("settled at %d/%d, a walk finds %d/%d", cached, cn, walked, wn)
	}
}

// A figure that saw changes during its walk is measured again after
// sizeRecheck, not sizeRefresh.
func TestBucketSizeChangedDuringAWalkIsRecheckedSoon(t *testing.T) {
	fs, _ := NewFileSystem(t.TempDir())
	fs.CreateBucketDir("b")
	prevRefresh, prevRecheck := sizeRefresh, sizeRecheck
	sizeRefresh, sizeRecheck = time.Hour, 10*time.Millisecond
	defer func() { sizeRefresh, sizeRecheck = prevRefresh, prevRecheck }()
	walks := 0
	walk := func() (int64, int64, error) {
		walks++
		if walks == 1 {
			fs.sizes.adjust("b", 4, 1) // a write lands mid-walk
		}
		return 0, 0, nil
	}
	fs.sizes.get("b", walk)
	fs.sizes.get("b", walk)
	if walks != 1 {
		t.Fatalf("%d walks before the recheck time", walks)
	}
	time.Sleep(20 * time.Millisecond)
	fs.sizes.get("b", walk)
	fs.sizes.get("b", walk)
	if walks != 2 {
		t.Fatalf("%d walks: a figure changed during its walk was not rechecked once", walks)
	}
}
