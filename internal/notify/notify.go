package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// S3Event matches the AWS S3 event notification JSON format.
type S3Event struct {
	Records []S3EventRecord `json:"Records"`
}

type S3EventRecord struct {
	EventVersion string   `json:"eventVersion"`
	EventSource  string   `json:"eventSource"`
	EventTime    string   `json:"eventTime"`
	EventName    string   `json:"eventName"`
	S3           S3Detail `json:"s3"`
}

type S3Detail struct {
	Bucket S3Bucket `json:"bucket"`
	Object S3Object `json:"object"`
}

type S3Bucket struct {
	Name string `json:"name"`
}

type S3Object struct {
	Key       string `json:"key"`
	Size      int64  `json:"size"`
	ETag      string `json:"eTag,omitempty"`
	VersionID string `json:"versionId,omitempty"`
}

// Backend is the interface for notification delivery backends.
type Backend interface {
	Name() string
	Publish(ctx context.Context, payload []byte) error
	Close() error
}

type deliveryJob struct {
	endpoint   string
	payload    []byte
	retryCount int
	maxRetries int
}

// Dispatcher handles async webhook delivery with retry.
type Dispatcher struct {
	store      *metadata.Store
	client     *http.Client
	workerCh   chan deliveryJob
	wg         sync.WaitGroup
	maxWorkers int
	maxRetries int
	backoff    []time.Duration
	backends   []Backend
	mu         sync.Mutex
}

func NewDispatcher(store *metadata.Store, maxWorkers, queueSize, timeoutSecs, maxRetries int) *Dispatcher {
	return &Dispatcher{
		store:      store,
		client:     &http.Client{Timeout: time.Duration(timeoutSecs) * time.Second},
		workerCh:   make(chan deliveryJob, queueSize),
		maxWorkers: maxWorkers,
		maxRetries: maxRetries,
		backoff:    []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second},
	}
}

func (d *Dispatcher) Start(ctx context.Context) {
	for i := 0; i < d.maxWorkers; i++ {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-d.workerCh:
					if !ok {
						return
					}
					d.deliverWebhook(job)
				}
			}
		}()
	}
}

// AddBackend registers a notification backend.
func (d *Dispatcher) AddBackend(b Backend) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.backends = append(d.backends, b)
	slog.Info("notification backend registered", "backend", b.Name())
}

func (d *Dispatcher) Stop() {
	close(d.workerCh)
	d.wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.backends {
		b.Close()
	}
}

// Dispatch checks notification configs for the bucket and fires matching webhooks.
func (d *Dispatcher) Dispatch(bucket, key, eventType string, size int64, etag, versionID string) {
	cfg, err := d.store.GetNotificationConfig(bucket)
	if err != nil {
		return // no config for this bucket
	}

	event := S3Event{
		Records: []S3EventRecord{{
			EventVersion: "2.1",
			EventSource:  "vaults3",
			EventTime:    time.Now().UTC().Format(time.RFC3339),
			EventName:    eventType,
			S3: S3Detail{
				Bucket: S3Bucket{Name: bucket},
				Object: S3Object{
					Key:       key,
					Size:      size,
					ETag:      etag,
					VersionID: versionID,
				},
			},
		}},
	}

	payload, err := json.Marshal(event)
	if err != nil {
		slog.Error("notify error marshaling event", "error", err)
		return
	}

	// Publish to all registered backends
	d.mu.Lock()
	backends := make([]Backend, len(d.backends))
	copy(backends, d.backends)
	d.mu.Unlock()
	for _, b := range backends {
		if err := b.Publish(context.Background(), payload); err != nil {
			slog.Error("notify backend publish error", "backend", b.Name(), "error", err)
		}
	}

	for _, wh := range cfg.Webhooks {
		if !matchEvent(wh.Events, eventType) {
			continue
		}
		if !matchFilters(wh.Filters, key) {
			continue
		}

		job := deliveryJob{
			endpoint:   wh.Endpoint,
			payload:    payload,
			retryCount: 0,
			maxRetries: d.maxRetries,
		}

		// Non-blocking send — drop if queue is full
		select {
		case d.workerCh <- job:
		default:
			slog.Warn("notify queue full, dropping event", "event", eventType, "bucket", bucket, "key", key)
		}
	}
}

func (d *Dispatcher) deliverWebhook(job deliveryJob) {
	resp, err := d.client.Post(job.endpoint, "application/json", bytes.NewReader(job.payload))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return // success
		}
		err = &httpError{statusCode: resp.StatusCode}
	}

	// Retry
	if job.retryCount < job.maxRetries-1 {
		backoffIdx := job.retryCount
		if backoffIdx >= len(d.backoff) {
			backoffIdx = len(d.backoff) - 1
		}
		time.Sleep(d.backoff[backoffIdx])

		job.retryCount++
		select {
		case d.workerCh <- job:
		default:
			slog.Warn("notify queue full on retry, dropping webhook", "endpoint", job.endpoint)
		}
	} else {
		slog.Error("notify webhook failed after retries", "retries", job.maxRetries, "endpoint", job.endpoint, "error", err)
	}
}

type httpError struct {
	statusCode int
}

func (e *httpError) Error() string {
	return "webhook returned non-success status"
}

// matchEvent checks if the actual event type matches any of the configured event patterns.
func matchEvent(patterns []string, actual string) bool {
	for _, p := range patterns {
		if p == actual {
			return true
		}
		// Wildcard matching: "s3:ObjectCreated:*" matches "s3:ObjectCreated:Put"
		if strings.HasSuffix(p, ":*") {
			prefix := p[:len(p)-1] // "s3:ObjectCreated:"
			if strings.HasPrefix(actual, prefix) {
				return true
			}
		}
		// Global wildcard
		if p == "*" || p == "s3:*" {
			return true
		}
	}
	return false
}

// matchFilters checks if the key matches all filter rules.
func matchFilters(filters []metadata.NotificationFilterRule, key string) bool {
	for _, f := range filters {
		switch f.Name {
		case "prefix":
			if !strings.HasPrefix(key, f.Value) {
				return false
			}
		case "suffix":
			if !strings.HasSuffix(key, f.Value) {
				return false
			}
		}
	}
	return true
}
