package inventory

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Config holds inventory report configuration.
type Config struct {
	Enabled         bool   `json:"enabled" yaml:"enabled"`
	DestBucket      string `json:"dest_bucket" yaml:"dest_bucket"`
	Schedule        string `json:"schedule" yaml:"schedule"` // "daily" or "weekly"
	IntervalSecs    int    `json:"interval_secs" yaml:"interval_secs"`
	IncludeVersions bool   `json:"include_versions" yaml:"include_versions"`
}

// Reporter generates periodic inventory CSV reports of bucket contents.
type Reporter struct {
	store        *metadata.Store
	engine       storage.Engine
	cfg          Config
	intervalSecs int
}

// NewReporter creates a new inventory reporter.
func NewReporter(store *metadata.Store, engine storage.Engine, cfg Config) *Reporter {
	interval := cfg.IntervalSecs
	if interval <= 0 {
		if cfg.Schedule == "weekly" {
			interval = 7 * 24 * 3600
		} else {
			interval = 24 * 3600 // daily
		}
	}
	return &Reporter{
		store:        store,
		engine:       engine,
		cfg:          cfg,
		intervalSecs: interval,
	}
}

// Run starts the periodic inventory reporter.
func (r *Reporter) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(r.intervalSecs) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.generateReports()
		}
	}
}

func (r *Reporter) generateReports() {
	buckets, _ := r.store.ListBuckets()
	for _, b := range buckets {
		if err := r.generateBucketReport(b.Name); err != nil {
			slog.Error("inventory report failed", "bucket", b.Name, "error", err)
		}
	}
}

func (r *Reporter) generateBucketReport(bucket string) error {
	objects, _, err := r.engine.ListObjects(bucket, "", "", 100000)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	// Header
	w.Write([]string{"Bucket", "Key", "Size", "ETag", "LastModified", "ContentType", "VersionID", "StorageClass"})

	for _, obj := range objects {
		meta, err := r.store.GetObjectMeta(bucket, obj.Key)
		if err != nil {
			continue
		}
		modTime := time.Unix(0, meta.LastModified).UTC()
		w.Write([]string{
			bucket,
			obj.Key,
			fmt.Sprintf("%d", meta.Size),
			meta.ETag,
			modTime.Format(time.RFC3339),
			meta.ContentType,
			meta.VersionID,
			"STANDARD",
		})
	}
	w.Flush()

	// Store report in destination bucket
	destBucket := r.cfg.DestBucket
	if destBucket == "" {
		destBucket = bucket
	}
	r.engine.CreateBucketDir(destBucket)

	reportKey := fmt.Sprintf("inventory/%s/%s.csv", bucket, time.Now().UTC().Format("2006-01-02T15-04-05"))
	data := buf.Bytes()
	_, _, err = r.engine.PutObject(destBucket, reportKey, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("write inventory report: %w", err)
	}

	r.store.PutObjectMeta(metadata.ObjectMeta{
		Bucket:       destBucket,
		Key:          reportKey,
		Size:         int64(len(data)),
		ContentType:  "text/csv",
		LastModified: time.Now().UTC().UnixNano(),
	})

	slog.Info("inventory report generated", "bucket", bucket, "report", reportKey, "objects", len(objects))
	return nil
}
