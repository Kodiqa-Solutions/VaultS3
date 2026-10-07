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
	"github.com/Kodiqa-Solutions/VaultS3/internal/netguard"
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
	store      metadata.StoreAPI
	client     *http.Client
	workerCh   chan deliveryJob
	backendCh  chan []byte
	wg         sync.WaitGroup
	maxWorkers int
	maxRetries int
	backoff    []time.Duration
	backends   []Backend
	timeout    time.Duration

	// mu guards backends and stopped. Every send on workerCh and backendCh
	// happens under it, so Stop can close them without a late retry or a
	// late event sending on a closed channel.
	mu      sync.Mutex
	stopped bool
}

func NewDispatcher(store metadata.StoreAPI, maxWorkers, queueSize, timeoutSecs, maxRetries int) *Dispatcher {
	timeout := time.Duration(timeoutSecs) * time.Second
	return &Dispatcher{
		store: store,
		// Webhook endpoints are set by bucket owners, not only the operator, so
		// the dial is checked against private, loopback and metadata addresses
		// after DNS resolution. The URL check made when a configuration is
		// saved sees only a literal address.
		client: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{DialContext: netguard.DialContext(10*time.Second, false)},
			// A redirect could take the request anywhere after the dial check
			// approved the first host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		workerCh:   make(chan deliveryJob, queueSize),
		backendCh:  make(chan []byte, queueSize),
		maxWorkers: maxWorkers,
		maxRetries: maxRetries,
		backoff:    []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second},
		timeout:    timeout,
	}
}

// AllowPrivateWebhooks lets webhooks reach private and loopback addresses. For
// tests and for an operator whose receivers live on their own network.
func (d *Dispatcher) AllowPrivateWebhooks() {
	d.client.Transport = &http.Transport{DialContext: netguard.DialContext(10*time.Second, true)}
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
	// Backends publish here, off the request path. They used to publish inside
	// the S3 request with no deadline, so one slow or unreachable Kafka, NATS or
	// PostgreSQL held up every PUT and DELETE on every bucket.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case payload, ok := <-d.backendCh:
				if !ok {
					return
				}
				d.publishToBackends(payload)
			}
		}
	}()
}

func (d *Dispatcher) publishToBackends(payload []byte) {
	d.mu.Lock()
	backends := make([]Backend, len(d.backends))
	copy(backends, d.backends)
	d.mu.Unlock()
	timeout := d.timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	for _, b := range backends {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if err := b.Publish(ctx, payload); err != nil {
			slog.Error("notify backend publish error", "backend", b.Name(), "error", err)
		}
		cancel()
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
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.stopped = true
	close(d.workerCh)
	close(d.backendCh)
	d.mu.Unlock()
	d.wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.backends {
		b.Close()
	}
}

// enqueue sends a webhook job unless the dispatcher has stopped or the queue
// is full. It never blocks.
func (d *Dispatcher) enqueue(job deliveryJob) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return false
	}
	select {
	case d.workerCh <- job:
		return true
	default:
		return false
	}
}

// Dispatch sends an event to every registered backend and fires the bucket's
// matching webhooks.
//
// It returned early for a bucket with no webhook configuration, before the
// backends, so Kafka, NATS, Redis, AMQP, PostgreSQL and Elasticsearch received
// events only from buckets that also had a webhook, where the documentation
// says every event goes to every enabled backend.
func (d *Dispatcher) Dispatch(bucket, key, eventType string, size int64, etag, versionID string) {
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

	d.mu.Lock()
	haveBackends := len(d.backends) > 0
	if haveBackends && !d.stopped {
		select {
		case d.backendCh <- payload:
		default:
			slog.Warn("notify backend queue full, dropping event", "event", eventType, "bucket", bucket, "key", key)
		}
	}
	d.mu.Unlock()

	cfg, err := d.store.GetNotificationConfig(bucket)
	if err != nil {
		return // no webhooks for this bucket
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
		if !d.enqueue(job) {
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

	// Retry later without holding this worker. Sleeping here, up to 30 seconds
	// per attempt, let one dead endpoint occupy every worker in turn, and the
	// events of every other bucket queued behind it or were dropped.
	if job.retryCount < job.maxRetries-1 {
		backoffIdx := job.retryCount
		if backoffIdx >= len(d.backoff) {
			backoffIdx = len(d.backoff) - 1
		}
		job.retryCount++
		time.AfterFunc(d.backoff[backoffIdx], func() {
			if !d.enqueue(job) {
				slog.Warn("notify queue full or stopped on retry, dropping webhook", "endpoint", job.endpoint)
			}
		})
	} else {
		if strings.Contains(err.Error(), "blocked destination") {
			slog.Error("notify webhook refused: it resolves to a private, loopback or metadata address; "+
				"set notifications.allow_private_webhooks to allow receivers on your own network",
				"endpoint", job.endpoint, "error", err)
			return
		}
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
