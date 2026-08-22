package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// maxMigrateRetries is how many times a transient source failure is retried
// (with exponential backoff) before giving up on an object or listing.
const maxMigrateRetries = 3

// Object copies within a bucket run concurrently with a bounded worker pool, so
// a large bucket does not migrate one object at a time. defaultConcurrency is
// used when the request does not specify one; maxConcurrency caps it to avoid
// overwhelming the source or the local disk.
const (
	defaultConcurrency = 8
	maxConcurrency     = 32
)

// retryable reports whether an error is worth retrying. HTTP 5xx/429 and network
// or streaming errors are transient; 4xx (and anything explicitly non-retryable)
// is permanent.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var he interface{ Retryable() bool }
	if errors.As(err, &he) {
		return he.Retryable()
	}
	return true // network / I/O errors are transient
}

// withRetry runs fn, retrying transient failures with exponential backoff.
func withRetry(label string, fn func() error) error {
	var err error
	for attempt := 0; attempt <= maxMigrateRetries; attempt++ {
		if attempt > 0 {
			d := time.Duration(200*(1<<(attempt-1))) * time.Millisecond // 200ms, 400ms, 800ms
			if d > 5*time.Second {
				d = 5 * time.Second
			}
			time.Sleep(d)
			slog.Warn("migrate: retrying after transient error", "op", label, "attempt", attempt, "error", err)
		}
		if err = fn(); err == nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
	}
	return err
}

// Job tracks the progress of one migration.
type Job struct {
	ID         string   `json:"id"`
	Endpoint   string   `json:"endpoint"`
	Buckets    []string `json:"buckets"`
	Status     string   `json:"status"` // "running", "completed", "failed", "cancelled"
	Total      int      `json:"total"`
	Copied     int      `json:"copied"`
	Skipped    int      `json:"skipped"` // already present at destination (resumed migration)
	Failed     int      `json:"failed"`
	Policies   int      `json:"policies"` // bucket policies carried over from the source
	Error      string   `json:"error,omitempty"`
	StartedAt  int64    `json:"started_at"`
	FinishedAt int64    `json:"finished_at,omitempty"`
}

// Manager runs migrations from S3-compatible sources into the local store/engine.
type Manager struct {
	store metadata.StoreAPI
	// allowPrivateSources lifts the SSRF guard for operators migrating from a
	// source inside their own network.
	allowPrivateSources bool
	engine              storage.Engine
	mu                  sync.RWMutex
	jobs                map[string]*Job
	cancels             map[string]context.CancelFunc // running job -> its cancel func
	seq                 int
}

// NewManager creates a migration manager.
func NewManager(store metadata.StoreAPI, engine storage.Engine) *Manager {
	return &Manager{
		store:   store,
		engine:  engine,
		jobs:    make(map[string]*Job),
		cancels: make(map[string]context.CancelFunc),
	}
}

// StartConfig describes a migration request.
type StartConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Region    string
	Buckets   []string // empty = all source buckets
	// Concurrency is how many objects to copy in parallel per bucket. 0 uses
	// defaultConcurrency; values above maxConcurrency are clamped.
	Concurrency int
}

// AllowPrivateSources lets this manager migrate from loopback, private and
// link-local addresses. Off by default: a caller-supplied endpoint is otherwise
// a server-side request primitive pointed at the internal network and the cloud
// metadata service (security assessment finding 6). Operators migrating from a
// source on their own network turn it on deliberately.
func (m *Manager) AllowPrivateSources(allow bool) { m.allowPrivateSources = allow }

func (m *Manager) newSource(cfg StartConfig, timeoutSecs int) *Source {
	if m.allowPrivateSources {
		return newSourceAllowingPrivate(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.Region, timeoutSecs)
	}
	return NewSource(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.Region, timeoutSecs)
}

// TestConnection validates credentials by listing the source buckets.
func (m *Manager) TestConnection(cfg StartConfig) ([]string, error) {
	if err := ValidateEndpoint(cfg.Endpoint); err != nil {
		return nil, err
	}
	return m.newSource(cfg, 30).ListBuckets()
}

// Start validates the source then launches an async migration; returns the job ID.
func (m *Manager) Start(cfg StartConfig) (string, error) {
	if err := ValidateEndpoint(cfg.Endpoint); err != nil {
		return "", err
	}
	src := m.newSource(cfg, 300)

	buckets := cfg.Buckets
	if len(buckets) == 0 {
		all, err := src.ListBuckets()
		if err != nil {
			return "", fmt.Errorf("list source buckets: %w", err)
		}
		buckets = all
	}
	if len(buckets) == 0 {
		return "", fmt.Errorf("no buckets to migrate")
	}

	m.mu.Lock()
	// Reject an obvious duplicate: the same source already migrating the same
	// buckets. Prevents accidental double-clicks from spawning parallel copies.
	for _, j := range m.jobs {
		if j.Status == "running" && j.Endpoint == cfg.Endpoint && sameBucketSet(j.Buckets, buckets) {
			m.mu.Unlock()
			return "", fmt.Errorf("a migration from this source for these buckets is already running")
		}
	}
	m.seq++
	id := fmt.Sprintf("migrate-%d", m.seq)
	job := &Job{
		ID:        id,
		Endpoint:  cfg.Endpoint,
		Buckets:   buckets,
		Status:    "running",
		StartedAt: time.Now().Unix(),
	}
	m.jobs[id] = job
	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[id] = cancel
	m.mu.Unlock()

	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	if concurrency > maxConcurrency {
		concurrency = maxConcurrency
	}

	go m.run(ctx, src, job, concurrency)
	return id, nil
}

// sameBucketSet reports whether two bucket lists contain the same set of names.
func sameBucketSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, x := range a {
		seen[x] = true
	}
	for _, y := range b {
		if !seen[y] {
			return false
		}
	}
	return true
}

// Cancel stops a running migration. It returns false if the job is unknown or
// already finished. Cancellation takes effect between objects (an in-flight
// object copy completes first), so no partial objects are left behind.
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil || job.Status != "running" {
		return false
	}
	if cancel := m.cancels[id]; cancel != nil {
		cancel()
	}
	return true
}

func (m *Manager) run(ctx context.Context, src *Source, job *Job, concurrency int) {
	defer func() {
		m.mu.Lock()
		delete(m.cancels, job.ID)
		m.mu.Unlock()
	}()
	for _, bucket := range job.Buckets {
		if ctx.Err() != nil {
			m.markCancelled(job)
			return
		}
		if !m.store.BucketExists(bucket) {
			if err := m.store.CreateBucket(bucket); err != nil {
				m.setError(job, fmt.Sprintf("create bucket %s: %v", bucket, err))
				return
			}
		}
		m.engine.CreateBucketDir(bucket)

		// Carry over the bucket's access policy and tags before the objects. This
		// is the IAM/policies half of a migration; user/access-key migration is
		// source-specific (MinIO admin API) and intentionally out of scope.
		m.migrateBucketMeta(src, bucket, job)

		token := ""
		for {
			var objs []ObjectInfo
			var next string
			err := withRetry("list "+bucket, func() error {
				var e error
				objs, next, e = src.ListObjects(bucket, token)
				return e
			})
			if err != nil {
				m.setError(job, fmt.Sprintf("list %s: %v", bucket, err))
				return
			}
			m.bump(job, func(j *Job) { j.Total += len(objs) })

			// Copy the page's objects with a bounded worker pool so a large
			// bucket migrates in parallel instead of one object at a time.
			sem := make(chan struct{}, concurrency)
			var wg sync.WaitGroup
			for _, o := range objs {
				if ctx.Err() != nil {
					break
				}
				o := o
				sem <- struct{}{}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					if ctx.Err() != nil {
						return
					}
					// Resume: skip an object already at the destination (a prior
					// run copied it), so a restarted/crashed migration continues
					// instead of re-transferring the whole bucket.
					if m.alreadyMigrated(bucket, o) {
						m.bump(job, func(j *Job) { j.Skipped++ })
						return
					}
					if err := withRetry("copy "+bucket+"/"+o.Key, func() error { return m.copyOne(src, bucket, o) }); err != nil {
						slog.Warn("migrate: copy failed after retries", "bucket", bucket, "key", o.Key, "error", err)
						m.bump(job, func(j *Job) { j.Failed++ })
						return
					}
					m.bump(job, func(j *Job) { j.Copied++ })
				}()
			}
			wg.Wait()
			if ctx.Err() != nil {
				m.markCancelled(job)
				return
			}
			if next == "" {
				break
			}
			token = next
		}
	}
	m.bump(job, func(j *Job) {
		if j.Status == "running" {
			j.Status = "completed"
		}
		j.FinishedAt = time.Now().Unix()
	})
	slog.Info("migrate: completed", "id", job.ID, "copied", job.Copied, "skipped", job.Skipped, "failed", job.Failed)
}

// alreadyMigrated reports whether the object is already present at the
// destination with the same size, i.e. a previous run copied it. This makes a
// migration resumable: after a restart or crash it skips what it already
// transferred instead of re-copying the whole bucket. GetObjectMeta only
// succeeds once both the object data and its metadata are committed, so an
// object left half-written by a crash is not mistaken for a completed copy and
// gets re-copied. Size (not ETag) is the match key on purpose: ETag schemes
// differ across S3 implementations and storage modes (multipart, encryption,
// packing), so comparing ETags would force needless re-copies.
func (m *Manager) alreadyMigrated(bucket string, o ObjectInfo) bool {
	meta, err := m.store.GetObjectMeta(bucket, o.Key)
	if err != nil {
		return false
	}
	return meta.Size == o.Size
}

// migrateBucketMeta copies the source bucket's policy and tags to the local
// store. Best-effort: a source that doesn't expose these endpoints, or a bucket
// with none, is not a migration failure — object data is what matters.
func (m *Manager) migrateBucketMeta(src *Source, bucket string, job *Job) {
	if policy, err := src.GetBucketPolicy(bucket); err != nil {
		slog.Warn("migrate: bucket policy fetch failed", "bucket", bucket, "error", err)
	} else if len(policy) > 0 {
		if err := m.store.PutBucketPolicy(bucket, policy); err != nil {
			slog.Warn("migrate: apply bucket policy failed", "bucket", bucket, "error", err)
		} else {
			m.bump(job, func(j *Job) { j.Policies++ })
			slog.Info("migrate: bucket policy migrated", "bucket", bucket)
		}
	}
	if tags, err := src.GetBucketTagging(bucket); err != nil {
		slog.Warn("migrate: bucket tagging fetch failed", "bucket", bucket, "error", err)
	} else if len(tags) > 0 {
		if err := m.store.PutBucketTags(bucket, tags); err != nil {
			slog.Warn("migrate: apply bucket tags failed", "bucket", bucket, "error", err)
		} else {
			slog.Info("migrate: bucket tags migrated", "bucket", bucket, "count", len(tags))
		}
	}
}

func (m *Manager) copyOne(src *Source, bucket string, o ObjectInfo) error {
	obj, err := src.GetObject(bucket, o.Key)
	if err != nil {
		return err
	}
	defer obj.Body.Close()

	size := obj.Size
	if size < 0 { // content length unknown — fall back to the listed size
		size = o.Size
	}
	// Stream straight from the source response into the local engine — no
	// buffering of the whole object in memory.
	written, etag, err := m.engine.PutObject(bucket, o.Key, obj.Body, size)
	if err != nil {
		return err
	}
	ct := obj.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	// Preserve the source's original metadata so the migration is a faithful copy,
	// not a same-day re-upload (issue #13). Prefer the GET Last-Modified, fall back
	// to the listed time, and only stamp "now" if the source gave us neither.
	lastModified := obj.LastModified
	if lastModified == 0 {
		lastModified = o.LastModified
	}
	if lastModified == 0 {
		lastModified = time.Now().Unix()
	}
	if err := m.store.PutObjectMeta(metadata.ObjectMeta{
		Bucket:             bucket,
		Key:                o.Key,
		ContentType:        ct,
		ETag:               etag,
		Size:               written,
		LastModified:       lastModified,
		UserMetadata:       obj.UserMetadata,
		ContentEncoding:    obj.ContentEncoding,
		ContentDisposition: obj.ContentDisposition,
		CacheControl:       obj.CacheControl,
		ContentLanguage:    obj.ContentLanguage,
	}); err != nil {
		return err
	}
	// Also stamp the on-disk file's mtime so the filesystem-walk listing (used for
	// non-versioned buckets) shows the preserved date, not the write time. Best
	// effort: object stores that don't keep one-file-per-object (e.g. packing) just
	// keep the authoritative date in metadata, which the S3 API already reports.
	mt := time.Unix(lastModified, 0)
	_ = os.Chtimes(m.engine.ObjectPath(bucket, o.Key), mt, mt)
	return nil
}

func (m *Manager) bump(job *Job, fn func(*Job)) {
	m.mu.Lock()
	fn(job)
	m.mu.Unlock()
}

func (m *Manager) markCancelled(job *Job) {
	m.bump(job, func(j *Job) {
		if j.Status == "running" {
			j.Status = "cancelled"
		}
		j.FinishedAt = time.Now().Unix()
	})
	slog.Info("migrate: cancelled", "id", job.ID, "copied", job.Copied, "failed", job.Failed)
}

func (m *Manager) setError(job *Job, msg string) {
	m.bump(job, func(j *Job) {
		j.Status = "failed"
		j.Error = msg
		j.FinishedAt = time.Now().Unix()
	})
	slog.Error("migrate: failed", "id", job.ID, "error", msg)
}

// GetJob returns a snapshot copy of a job (safe to read while it runs).
func (m *Manager) GetJob(id string) *Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j := m.jobs[id]
	if j == nil {
		return nil
	}
	cp := *j
	return &cp
}

// ListJobs returns snapshot copies of all jobs.
func (m *Manager) ListJobs() []*Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		cp := *j
		out = append(out, &cp)
	}
	return out
}
