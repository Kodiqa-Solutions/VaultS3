package scanner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// ScanJob represents an object to be scanned.
type ScanJob struct {
	Bucket string
	Key    string
	Size   int64
}

// ScanResult records the result of a scan.
type ScanResult struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	Status    string `json:"status"` // "clean", "infected", "error"
	Detail    string `json:"detail,omitempty"`
	ScannedAt int64  `json:"scanned_at"`
}

// Scanner posts uploaded objects to a configurable webhook for virus scanning.
type Scanner struct {
	webhookURL       string
	quarantineBucket string
	failClosed       bool
	maxScanSize      int64
	timeout          time.Duration

	store  *metadata.Store
	engine storage.Engine
	client *http.Client

	jobs    chan ScanJob
	results []ScanResult
	mu      sync.RWMutex

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewScanner creates a new virus scanner.
func NewScanner(store *metadata.Store, engine storage.Engine, webhookURL string, workers int, timeoutSecs int, quarantineBucket string, failClosed bool, maxScanSize int64, queueSize int) *Scanner {
	if workers <= 0 {
		workers = 2
	}
	if queueSize <= 0 {
		queueSize = 256
	}
	return &Scanner{
		webhookURL:       webhookURL,
		quarantineBucket: quarantineBucket,
		failClosed:       failClosed,
		maxScanSize:      maxScanSize,
		timeout:          time.Duration(timeoutSecs) * time.Second,
		store:            store,
		engine:           engine,
		client:           &http.Client{Timeout: time.Duration(timeoutSecs) * time.Second},
		jobs:             make(chan ScanJob, queueSize),
	}
}

// Start launches scanner workers.
func (s *Scanner) Start(ctx context.Context, workers int) {
	ctx, s.cancel = context.WithCancel(ctx)
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go s.worker(ctx)
	}
	slog.Info("scanner started", "workers", workers, "webhook", s.webhookURL)
}

// Stop shuts down the scanner gracefully.
func (s *Scanner) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// Scan enqueues an object for scanning.
func (s *Scanner) Scan(bucket, key string, size int64) {
	if s.maxScanSize > 0 && size > s.maxScanSize {
		return // skip files larger than max scan size
	}
	select {
	case s.jobs <- ScanJob{Bucket: bucket, Key: key, Size: size}:
	default:
		slog.Warn("scanner queue full, dropping scan", "bucket", bucket, "key", key)
	}
}

// QueueDepth returns the current number of pending scan jobs.
func (s *Scanner) QueueDepth() int {
	return len(s.jobs)
}

// RecentResults returns recent scan results.
func (s *Scanner) RecentResults(limit int) []ScanResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.results) {
		limit = len(s.results)
	}
	// Return most recent first
	start := len(s.results) - limit
	if start < 0 {
		start = 0
	}
	results := make([]ScanResult, limit)
	for i, j := 0, len(s.results)-1; i < limit && j >= start; i, j = i+1, j-1 {
		results[i] = s.results[j]
	}
	return results
}

// QuarantineList returns objects in the quarantine bucket.
func (s *Scanner) QuarantineList(store *metadata.Store, engine storage.Engine) []map[string]interface{} {
	objects, _, _ := engine.ListObjects(s.quarantineBucket, "", "", 1000)
	var results []map[string]interface{}
	for _, obj := range objects {
		results = append(results, map[string]interface{}{
			"key":           obj.Key,
			"size":          obj.Size,
			"last_modified": obj.LastModified,
		})
	}
	return results
}

func (s *Scanner) worker(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.jobs:
			s.processJob(job)
		}
	}
}

func (s *Scanner) processJob(job ScanJob) {
	result := ScanResult{
		Bucket:    job.Bucket,
		Key:       job.Key,
		ScannedAt: time.Now().Unix(),
	}

	// Read the object
	reader, size, err := s.engine.GetObject(job.Bucket, job.Key)
	if err != nil {
		result.Status = "error"
		result.Detail = fmt.Sprintf("failed to read object: %v", err)
		s.addResult(result)
		return
	}
	defer reader.Close()

	// POST to webhook as multipart/form-data
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", job.Key)
	if err != nil {
		result.Status = "error"
		result.Detail = fmt.Sprintf("failed to create form: %v", err)
		s.addResult(result)
		return
	}

	if _, err := io.Copy(part, reader); err != nil {
		result.Status = "error"
		result.Detail = fmt.Sprintf("failed to read object data: %v", err)
		s.addResult(result)
		return
	}

	// Add metadata fields
	writer.WriteField("bucket", job.Bucket)
	writer.WriteField("key", job.Key)
	writer.WriteField("size", fmt.Sprintf("%d", size))
	writer.Close()

	req, err := http.NewRequest("POST", s.webhookURL, &body)
	if err != nil {
		result.Status = "error"
		result.Detail = fmt.Sprintf("failed to create request: %v", err)
		s.addResult(result)
		return
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		// Webhook unreachable
		result.Status = "error"
		result.Detail = fmt.Sprintf("webhook unreachable: %v", err)
		s.addResult(result)

		if s.failClosed {
			// Move to quarantine on failure
			s.quarantine(job, "webhook unreachable (fail-closed)")
		}
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == 200:
		// Clean
		result.Status = "clean"
		result.Detail = string(respBody)
	case resp.StatusCode == 406 || resp.StatusCode == 403:
		// Infected
		result.Status = "infected"
		result.Detail = string(respBody)
		s.quarantine(job, string(respBody))
	default:
		result.Status = "error"
		result.Detail = fmt.Sprintf("webhook returned %d: %s", resp.StatusCode, string(respBody))
		if s.failClosed {
			s.quarantine(job, result.Detail)
		}
	}

	s.addResult(result)
}

func (s *Scanner) quarantine(job ScanJob, reason string) {
	// Ensure quarantine bucket exists
	s.engine.CreateBucketDir(s.quarantineBucket)
	s.store.CreateBucket(s.quarantineBucket)

	// Read the object
	reader, size, err := s.engine.GetObject(job.Bucket, job.Key)
	if err != nil {
		slog.Error("scanner quarantine: failed to read", "bucket", job.Bucket, "key", job.Key, "error", err)
		return
	}
	defer reader.Close()

	// Write to quarantine bucket with original bucket/key as the key
	quarantineKey := fmt.Sprintf("%s/%s", job.Bucket, job.Key)
	if _, _, err := s.engine.PutObject(s.quarantineBucket, quarantineKey, reader, size); err != nil {
		slog.Error("scanner quarantine: failed to write", "key", quarantineKey, "error", err)
		return
	}

	// Delete from original bucket
	if err := s.engine.DeleteObject(job.Bucket, job.Key); err != nil {
		slog.Error("scanner quarantine: failed to delete original", "bucket", job.Bucket, "key", job.Key, "error", err)
		return
	}
	s.store.DeleteObjectMeta(job.Bucket, job.Key)

	slog.Warn("scanner quarantined object", "bucket", job.Bucket, "key", job.Key, "reason", reason)
}

func (s *Scanner) addResult(r ScanResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, r)
	// Keep last 1000 results
	if len(s.results) > 1000 {
		s.results = s.results[len(s.results)-1000:]
	}
}
