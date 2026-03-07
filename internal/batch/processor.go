package batch

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// JobType defines the type of batch operation.
type JobType string

const (
	JobBulkDelete JobType = "bulk-delete"
	JobBulkCopy   JobType = "bulk-copy"
)

// Job represents a batch operation job.
type Job struct {
	ID        string    `json:"id"`
	Type      JobType   `json:"type"`
	Bucket    string    `json:"bucket"`
	Prefix    string    `json:"prefix,omitempty"`
	DstBucket string    `json:"dst_bucket,omitempty"`
	Status    string    `json:"status"` // "queued", "running", "completed", "failed"
	Progress  int       `json:"progress"`
	Total     int       `json:"total"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Processor handles batch operations.
type Processor struct {
	store  *metadata.Store
	engine storage.Engine
	mu     sync.RWMutex
	jobs   map[string]*Job
}

// NewProcessor creates a new batch processor.
func NewProcessor(store *metadata.Store, engine storage.Engine) *Processor {
	return &Processor{
		store:  store,
		engine: engine,
		jobs:   make(map[string]*Job),
	}
}

// Submit submits a new batch job. Returns the job ID.
func (p *Processor) Submit(job *Job) string {
	job.Status = "queued"
	job.CreatedAt = time.Now().UTC()
	p.mu.Lock()
	p.jobs[job.ID] = job
	p.mu.Unlock()

	go p.execute(job)
	return job.ID
}

// GetJob returns a job by ID.
func (p *Processor) GetJob(id string) *Job {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.jobs[id]
}

// ListJobs returns all jobs.
func (p *Processor) ListJobs() []*Job {
	p.mu.RLock()
	defer p.mu.RUnlock()
	jobs := make([]*Job, 0, len(p.jobs))
	for _, j := range p.jobs {
		jobs = append(jobs, j)
	}
	return jobs
}

func (p *Processor) execute(job *Job) {
	p.mu.Lock()
	job.Status = "running"
	p.mu.Unlock()

	var err error
	switch job.Type {
	case JobBulkDelete:
		err = p.executeBulkDelete(job)
	case JobBulkCopy:
		err = p.executeBulkCopy(job)
	default:
		err = fmt.Errorf("unknown job type: %s", job.Type)
	}

	p.mu.Lock()
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
	} else {
		job.Status = "completed"
	}
	p.mu.Unlock()
}

func (p *Processor) executeBulkDelete(job *Job) error {
	objects, _, err := p.engine.ListObjects(job.Bucket, job.Prefix, "", 10000)
	if err != nil {
		return err
	}

	p.mu.Lock()
	job.Total = len(objects)
	p.mu.Unlock()

	for i, obj := range objects {
		if err := p.engine.DeleteObject(job.Bucket, obj.Key); err != nil {
			slog.Error("batch delete error", "key", obj.Key, "error", err)
			continue
		}
		p.store.DeleteObjectMeta(job.Bucket, obj.Key)

		p.mu.Lock()
		job.Progress = i + 1
		p.mu.Unlock()
	}
	return nil
}

func (p *Processor) executeBulkCopy(job *Job) error {
	objects, _, err := p.engine.ListObjects(job.Bucket, job.Prefix, "", 10000)
	if err != nil {
		return err
	}

	p.mu.Lock()
	job.Total = len(objects)
	p.mu.Unlock()

	for i, obj := range objects {
		reader, size, err := p.engine.GetObject(job.Bucket, obj.Key)
		if err != nil {
			slog.Error("batch copy read error", "key", obj.Key, "error", err)
			continue
		}
		_, _, err = p.engine.PutObject(job.DstBucket, obj.Key, reader, size)
		reader.Close()
		if err != nil {
			slog.Error("batch copy write error", "key", obj.Key, "error", err)
			continue
		}

		p.mu.Lock()
		job.Progress = i + 1
		p.mu.Unlock()
	}
	return nil
}
