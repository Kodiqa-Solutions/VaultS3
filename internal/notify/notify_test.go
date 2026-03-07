package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

func newTestStore(t *testing.T) *metadata.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// mockBackend implements Backend for testing.
type mockBackend struct {
	name     string
	messages [][]byte
	closed   bool
}

func (m *mockBackend) Name() string { return m.name }
func (m *mockBackend) Publish(_ context.Context, payload []byte) error {
	m.messages = append(m.messages, payload)
	return nil
}
func (m *mockBackend) Close() error {
	m.closed = true
	return nil
}

func TestNewDispatcher(t *testing.T) {
	store := newTestStore(t)
	d := NewDispatcher(store, 2, 10, 5, 3)
	if d == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	if d.maxWorkers != 2 {
		t.Errorf("expected 2 workers, got %d", d.maxWorkers)
	}
	if d.maxRetries != 3 {
		t.Errorf("expected 3 retries, got %d", d.maxRetries)
	}
}

func TestDispatcher_StartStop(t *testing.T) {
	store := newTestStore(t)
	d := NewDispatcher(store, 2, 10, 5, 3)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	cancel()
	d.Stop()
}

func TestDispatcher_AddBackend(t *testing.T) {
	store := newTestStore(t)
	d := NewDispatcher(store, 1, 10, 5, 3)

	b := &mockBackend{name: "test-backend"}
	d.AddBackend(b)

	if len(d.backends) != 1 {
		t.Errorf("expected 1 backend, got %d", len(d.backends))
	}
}

func TestDispatcher_BackendClose(t *testing.T) {
	store := newTestStore(t)
	d := NewDispatcher(store, 1, 10, 5, 3)

	b := &mockBackend{name: "test"}
	d.AddBackend(b)

	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	cancel()
	d.Stop()

	if !b.closed {
		t.Error("expected backend to be closed")
	}
}

func TestDispatcher_DispatchToBackend(t *testing.T) {
	store := newTestStore(t)
	// Create a bucket with notification config
	store.CreateBucket("test-bucket")
	store.PutNotificationConfig("test-bucket", metadata.BucketNotificationConfig{
		Webhooks: []metadata.NotificationEndpointConfig{},
	})

	d := NewDispatcher(store, 1, 10, 5, 3)
	b := &mockBackend{name: "test"}
	d.AddBackend(b)

	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)

	d.Dispatch("test-bucket", "file.txt", "s3:ObjectCreated:Put", 1024, "etag123", "")

	// Give time for async processing
	time.Sleep(50 * time.Millisecond)

	cancel()
	d.Stop()

	if len(b.messages) != 1 {
		t.Errorf("expected 1 message to backend, got %d", len(b.messages))
	}
}

func TestDispatcher_WebhookDelivery(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newTestStore(t)
	store.CreateBucket("test-bucket")
	store.PutNotificationConfig("test-bucket", metadata.BucketNotificationConfig{
		Webhooks: []metadata.NotificationEndpointConfig{
			{
				ID:       "wh1",
				Endpoint: server.URL,
				Events:   []string{"s3:ObjectCreated:*"},
			},
		},
	})

	d := NewDispatcher(store, 2, 10, 5, 3)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)

	d.Dispatch("test-bucket", "file.txt", "s3:ObjectCreated:Put", 100, "etag", "")

	time.Sleep(200 * time.Millisecond)
	cancel()
	d.Stop()

	if received.Load() != 1 {
		t.Errorf("expected 1 webhook call, got %d", received.Load())
	}
}

func TestDispatcher_EventFiltering(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newTestStore(t)
	store.CreateBucket("test-bucket")
	store.PutNotificationConfig("test-bucket", metadata.BucketNotificationConfig{
		Webhooks: []metadata.NotificationEndpointConfig{
			{
				ID:       "wh1",
				Endpoint: server.URL,
				Events:   []string{"s3:ObjectRemoved:*"},
			},
		},
	})

	d := NewDispatcher(store, 1, 10, 5, 3)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)

	// This should NOT match the webhook (ObjectCreated vs ObjectRemoved)
	d.Dispatch("test-bucket", "file.txt", "s3:ObjectCreated:Put", 100, "etag", "")

	time.Sleep(100 * time.Millisecond)
	cancel()
	d.Stop()

	if received.Load() != 0 {
		t.Errorf("expected 0 webhook calls (filtered), got %d", received.Load())
	}
}

func TestDispatcher_KeyFilter(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newTestStore(t)
	store.CreateBucket("test-bucket")
	store.PutNotificationConfig("test-bucket", metadata.BucketNotificationConfig{
		Webhooks: []metadata.NotificationEndpointConfig{
			{
				ID:       "wh1",
				Endpoint: server.URL,
				Events:   []string{"s3:ObjectCreated:*"},
				Filters: []metadata.NotificationFilterRule{
					{Name: "prefix", Value: "images/"},
				},
			},
		},
	})

	d := NewDispatcher(store, 1, 10, 5, 3)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)

	// Non-matching key
	d.Dispatch("test-bucket", "docs/file.txt", "s3:ObjectCreated:Put", 100, "etag", "")
	time.Sleep(100 * time.Millisecond)
	if received.Load() != 0 {
		t.Errorf("expected 0 for non-matching prefix, got %d", received.Load())
	}

	// Matching key
	d.Dispatch("test-bucket", "images/photo.jpg", "s3:ObjectCreated:Put", 100, "etag", "")
	time.Sleep(100 * time.Millisecond)

	cancel()
	d.Stop()

	if received.Load() != 1 {
		t.Errorf("expected 1 for matching prefix, got %d", received.Load())
	}
}

// --- matchEvent tests ---

func TestMatchEvent_Exact(t *testing.T) {
	if !matchEvent([]string{"s3:ObjectCreated:Put"}, "s3:ObjectCreated:Put") {
		t.Error("exact match should succeed")
	}
	if matchEvent([]string{"s3:ObjectCreated:Put"}, "s3:ObjectRemoved:Delete") {
		t.Error("different events should not match")
	}
}

func TestMatchEvent_Wildcard(t *testing.T) {
	if !matchEvent([]string{"s3:ObjectCreated:*"}, "s3:ObjectCreated:Put") {
		t.Error("wildcard should match sub-event")
	}
	if !matchEvent([]string{"s3:ObjectCreated:*"}, "s3:ObjectCreated:Copy") {
		t.Error("wildcard should match sub-event")
	}
	if matchEvent([]string{"s3:ObjectCreated:*"}, "s3:ObjectRemoved:Delete") {
		t.Error("wrong category wildcard should not match")
	}
}

func TestMatchEvent_GlobalWildcard(t *testing.T) {
	if !matchEvent([]string{"*"}, "s3:ObjectCreated:Put") {
		t.Error("* should match everything")
	}
	if !matchEvent([]string{"s3:*"}, "s3:ObjectRemoved:Delete") {
		t.Error("s3:* should match all s3 events")
	}
}

func TestMatchEvent_NoPatterns(t *testing.T) {
	if matchEvent([]string{}, "s3:ObjectCreated:Put") {
		t.Error("empty patterns should not match")
	}
}

// --- matchFilters tests ---

func TestMatchFilters_NoFilters(t *testing.T) {
	if !matchFilters(nil, "any/key") {
		t.Error("no filters should match everything")
	}
}

func TestMatchFilters_Prefix(t *testing.T) {
	filters := []metadata.NotificationFilterRule{{Name: "prefix", Value: "logs/"}}
	if !matchFilters(filters, "logs/app.log") {
		t.Error("matching prefix should pass")
	}
	if matchFilters(filters, "data/file.csv") {
		t.Error("non-matching prefix should fail")
	}
}

func TestMatchFilters_Suffix(t *testing.T) {
	filters := []metadata.NotificationFilterRule{{Name: "suffix", Value: ".jpg"}}
	if !matchFilters(filters, "images/photo.jpg") {
		t.Error("matching suffix should pass")
	}
	if matchFilters(filters, "images/photo.png") {
		t.Error("non-matching suffix should fail")
	}
}

func TestMatchFilters_PrefixAndSuffix(t *testing.T) {
	filters := []metadata.NotificationFilterRule{
		{Name: "prefix", Value: "images/"},
		{Name: "suffix", Value: ".jpg"},
	}
	if !matchFilters(filters, "images/photo.jpg") {
		t.Error("both matching should pass")
	}
	if matchFilters(filters, "images/photo.png") {
		t.Error("suffix not matching should fail")
	}
	if matchFilters(filters, "docs/photo.jpg") {
		t.Error("prefix not matching should fail")
	}
}

func TestDispatcher_NoBucketConfig(t *testing.T) {
	store := newTestStore(t)
	// Don't create bucket or notification config
	d := NewDispatcher(store, 1, 10, 5, 3)
	// Should not panic
	d.Dispatch("nonexistent", "file.txt", "s3:ObjectCreated:Put", 100, "etag", "")
}

// Ensure temp dir doesn't leak
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
