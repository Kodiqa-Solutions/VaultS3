package metrics

import (
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

type countingEngine struct {
	storage.Engine
	walks int32
}

func (e *countingEngine) BucketSize(bucket string) (int64, int64, error) {
	atomic.AddInt32(&e.walks, 1)
	return e.Engine.BucketSize(bucket)
}

// /metrics is unauthenticated and walked every bucket's directory on every
// scrape. Repeated scrapes within a minute must not walk again.
func TestScrapesDoNotWalkBucketsEveryTime(t *testing.T) {
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fs, _ := storage.NewFileSystem(filepath.Join(dir, "data"))
	store.CreateBucket("b1")
	fs.CreateBucketDir("b1")
	eng := &countingEngine{Engine: fs}
	c := NewCollector(store, eng)
	for i := 0; i < 20; i++ {
		c.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
	}
	if n := atomic.LoadInt32(&eng.walks); n != 1 {
		t.Errorf("20 scrapes walked the bucket %d times, want 1", n)
	}
}
