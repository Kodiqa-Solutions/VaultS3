package lambda

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// LambdaEvent is the payload sent to the function URL.
type LambdaEvent struct {
	Event  S3Event `json:"event"`
	Object string  `json:"object,omitempty"` // base64-encoded body if IncludeBody
}

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

type triggerJob struct {
	trigger   metadata.LambdaTrigger
	bucket    string
	key       string
	eventType string
	size      int64
	etag      string
	versionID string
}

// TriggerManager dispatches S3 events to lambda function URLs.
type TriggerManager struct {
	store           *metadata.Store
	engine          storage.Engine
	client          *http.Client
	workerCh        chan triggerJob
	wg              sync.WaitGroup
	maxResponseSize int64
	maxWorkers      int
}

// NewTriggerManager creates a new trigger manager.
func NewTriggerManager(store *metadata.Store, engine storage.Engine, cfg config.LambdaConfig) *TriggerManager {
	return &TriggerManager{
		store:           store,
		engine:          engine,
		client:          &http.Client{Timeout: time.Duration(cfg.TimeoutSecs) * time.Second},
		workerCh:        make(chan triggerJob, cfg.QueueSize),
		maxResponseSize: cfg.MaxResponseSize,
		maxWorkers:      cfg.MaxWorkers,
	}
}

// Start launches worker goroutines.
func (m *TriggerManager) Start(ctx context.Context) {
	for i := 0; i < m.maxWorkers; i++ {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-m.workerCh:
					if !ok {
						return
					}
					m.executeTrigger(job)
				}
			}
		}()
	}
}

// Stop closes the work channel and waits for workers to drain.
func (m *TriggerManager) Stop() {
	close(m.workerCh)
	m.wg.Wait()
}

// QueueDepth returns the current number of pending jobs.
func (m *TriggerManager) QueueDepth() int {
	return len(m.workerCh)
}

// Dispatch checks lambda configs for the bucket and enqueues matching triggers.
func (m *TriggerManager) Dispatch(bucket, key, eventType string, size int64, etag, versionID string) {
	cfg, err := m.store.GetLambdaConfig(bucket)
	if err != nil {
		return // no lambda config for this bucket
	}

	for _, trigger := range cfg.Triggers {
		if !matchEvent(trigger.Events, eventType) {
			continue
		}
		if !matchFilter(trigger.Filters, key) {
			continue
		}

		job := triggerJob{
			trigger:   trigger,
			bucket:    bucket,
			key:       key,
			eventType: eventType,
			size:      size,
			etag:      etag,
			versionID: versionID,
		}

		// Non-blocking send — drop if queue is full
		select {
		case m.workerCh <- job:
		default:
			slog.Warn("lambda queue full, dropping trigger", "trigger_id", trigger.ID, "bucket", bucket, "key", key)
		}
	}
}

func (m *TriggerManager) executeTrigger(job triggerJob) {
	event := S3Event{
		Records: []S3EventRecord{{
			EventVersion: "2.1",
			EventSource:  "vaults3",
			EventTime:    time.Now().UTC().Format(time.RFC3339),
			EventName:    job.eventType,
			S3: S3Detail{
				Bucket: S3Bucket{Name: job.bucket},
				Object: S3Object{
					Key:       job.key,
					Size:      job.size,
					ETag:      job.etag,
					VersionID: job.versionID,
				},
			},
		}},
	}

	lambdaEvent := LambdaEvent{Event: event}

	// Include object body if configured
	if job.trigger.IncludeBody && job.eventType != "s3:ObjectRemoved:Delete" {
		maxBody := job.trigger.MaxBodySize
		if maxBody <= 0 {
			maxBody = 1 << 20 // 1MB default
		}
		if job.size <= maxBody {
			reader, _, err := m.engine.GetObject(job.bucket, job.key)
			if err == nil {
				data, err := io.ReadAll(io.LimitReader(reader, maxBody+1))
				reader.Close()
				if err == nil && int64(len(data)) <= maxBody {
					lambdaEvent.Object = base64.StdEncoding.EncodeToString(data)
				}
			}
		}
	}

	payload, err := json.Marshal(lambdaEvent)
	if err != nil {
		slog.Error("lambda error marshaling event", "trigger_id", job.trigger.ID, "error", err)
		return
	}

	resp, err := m.client.Post(job.trigger.FunctionURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		slog.Error("lambda function call failed", "url", job.trigger.FunctionURL, "trigger_id", job.trigger.ID, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		slog.Warn("lambda trigger returned error status", "trigger_id", job.trigger.ID, "status", resp.StatusCode, "url", job.trigger.FunctionURL)
		return
	}

	// Store response as new object if output bucket is configured
	if job.trigger.OutputBucket != "" && job.trigger.OutputKeyTemplate != "" {
		responseBody, err := io.ReadAll(io.LimitReader(resp.Body, m.maxResponseSize))
		if err != nil {
			slog.Error("lambda error reading response", "trigger_id", job.trigger.ID, "error", err)
			return
		}

		outputKey := expandTemplate(job.trigger.OutputKeyTemplate, job.bucket, job.key)

		// Validate output key doesn't contain path traversal
		for _, segment := range strings.Split(outputKey, "/") {
			if segment == ".." {
				slog.Error("lambda output key contains path traversal", "trigger_id", job.trigger.ID, "key", outputKey)
				return
			}
		}

		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		_, _, err = m.engine.PutObject(job.trigger.OutputBucket, outputKey, bytes.NewReader(responseBody), int64(len(responseBody)))
		if err != nil {
			slog.Error("lambda error storing output", "trigger_id", job.trigger.ID, "bucket", job.trigger.OutputBucket, "key", outputKey, "error", err)
			return
		}

		// Update metadata
		m.store.PutObjectMeta(metadata.ObjectMeta{
			Bucket:       job.trigger.OutputBucket,
			Key:          outputKey,
			ContentType:  contentType,
			Size:         int64(len(responseBody)),
			LastModified: time.Now().Unix(),
		})

		slog.Info("lambda trigger stored output", "trigger_id", job.trigger.ID, "bucket", job.trigger.OutputBucket, "key", outputKey, "bytes", len(responseBody))
	}
}

// expandTemplate expands {bucket}, {key}, {ext} placeholders in the output key template.
func expandTemplate(tmpl, bucket, key string) string {
	ext := path.Ext(key)
	base := strings.TrimSuffix(key, ext)

	result := tmpl
	result = strings.ReplaceAll(result, "{bucket}", bucket)
	result = strings.ReplaceAll(result, "{key}", key)
	result = strings.ReplaceAll(result, "{base}", base)
	result = strings.ReplaceAll(result, "{ext}", ext)
	return result
}

// matchEvent checks if the actual event type matches any of the configured event patterns.
func matchEvent(patterns []string, actual string) bool {
	for _, p := range patterns {
		if p == actual {
			return true
		}
		if strings.HasSuffix(p, ":*") {
			prefix := p[:len(p)-1]
			if strings.HasPrefix(actual, prefix) {
				return true
			}
		}
		if p == "*" || p == "s3:*" {
			return true
		}
	}
	return false
}

// matchFilter checks if the key matches prefix/suffix filters.
func matchFilter(f metadata.LambdaTriggerFilter, key string) bool {
	if f.Prefix != "" && !strings.HasPrefix(key, f.Prefix) {
		return false
	}
	if f.Suffix != "" && !strings.HasSuffix(key, f.Suffix) {
		return false
	}
	return true
}
