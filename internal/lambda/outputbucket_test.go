package lambda

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

func newManager(t *testing.T) (*TriggerManager, *metadata.Store, *storage.FileSystem) {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fs, _ := storage.NewFileSystem(filepath.Join(dir, "data"))
	for _, b := range []string{"own", "victim"} {
		store.CreateBucket(b)
		fs.CreateBucketDir(b)
	}
	return NewTriggerManager(store, fs, config.LambdaConfig{Enabled: true, TimeoutSecs: 5, MaxResponseSize: 1 << 20}), store, fs
}

// The function's response is written by the server itself, past IAM, object
// lock and quotas. A trigger stored before 5.0.0 can still name another
// bucket, and its output must not be written there.
func TestTriggerOutputNeverLandsInAnotherBucket(t *testing.T) {
	m, store, _ := newManager(t)
	fn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("PWNED")) }))
	defer fn.Close()

	m.executeTrigger(triggerJob{
		trigger: metadata.LambdaTrigger{ID: "t", FunctionURL: fn.URL, OutputBucket: "victim", OutputKeyTemplate: "a.txt"},
		bucket:  "own", key: "in.txt", eventType: "s3:ObjectCreated:Put",
	})
	if _, err := store.GetObjectMeta("victim", "a.txt"); err == nil {
		t.Error("a trigger on bucket own wrote its output into bucket victim")
	}

	m.executeTrigger(triggerJob{
		trigger: metadata.LambdaTrigger{ID: "t", FunctionURL: fn.URL, OutputBucket: "own", OutputKeyTemplate: "out.txt"},
		bucket:  "own", key: "in.txt", eventType: "s3:ObjectCreated:Put",
	})
	if _, err := store.GetObjectMeta("own", "out.txt"); err != nil {
		t.Errorf("output to the trigger's own bucket was not written: %v", err)
	}
}

// The function URL is validated when the trigger is saved. Following a redirect
// at call time sent the request wherever the function pointed it.
func TestTriggerDoesNotFollowRedirects(t *testing.T) {
	m, _, _ := newManager(t)
	var hit int32
	inner := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&hit, 1) }))
	defer inner.Close()
	fn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL, http.StatusTemporaryRedirect)
	}))
	defer fn.Close()

	m.executeTrigger(triggerJob{
		trigger: metadata.LambdaTrigger{ID: "t", FunctionURL: fn.URL},
		bucket:  "own", key: "in.txt", eventType: "s3:ObjectCreated:Put",
	})
	if atomic.LoadInt32(&hit) != 0 {
		t.Error("the call followed the function's redirect")
	}
}
