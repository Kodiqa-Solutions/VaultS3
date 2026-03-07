package lifecycle

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

type Worker struct {
	store              *metadata.Store
	engine             storage.Engine
	interval           time.Duration
	auditRetentionDays int
}

func NewWorker(store *metadata.Store, engine storage.Engine, intervalSecs, auditRetentionDays int) *Worker {
	return &Worker{
		store:              store,
		engine:             engine,
		interval:           time.Duration(intervalSecs) * time.Second,
		auditRetentionDays: auditRetentionDays,
	}
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Run once at startup
	w.scan()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.scan()
		}
	}
}

// matchRule checks if an object matches a lifecycle rule's filters.
func matchRule(rule *metadata.LifecycleRule, meta *metadata.ObjectMeta) bool {
	if rule.Prefix != "" && !strings.HasPrefix(meta.Key, rule.Prefix) {
		return false
	}
	if len(rule.TagFilter) > 0 {
		for k, v := range rule.TagFilter {
			if meta.Tags[k] != v {
				return false
			}
		}
	}
	if rule.ObjectSizeGreaterThan > 0 && meta.Size <= rule.ObjectSizeGreaterThan {
		return false
	}
	if rule.ObjectSizeLessThan > 0 && meta.Size >= rule.ObjectSizeLessThan {
		return false
	}
	return true
}

func (w *Worker) scan() {
	now := time.Now().UTC().Unix()

	buckets, err := w.store.ListBuckets()
	if err != nil {
		slog.Error("lifecycle error listing buckets", "error", err)
		return
	}

	// Load lifecycle configs (supports both old single-rule and new multi-rule)
	configs := make(map[string]*metadata.LifecycleConfig)
	for _, b := range buckets {
		cfg, err := w.store.GetLifecycleConfig(b.Name)
		if err != nil || cfg == nil || len(cfg.Rules) == 0 {
			continue
		}
		configs[b.Name] = cfg
	}

	if len(configs) == 0 {
		w.pruneAndClean()
		return
	}

	var expired, noncurrentExpired, multipartAborted, deleteMarkersRemoved int

	// 1. Current object expiration (with size/tag filters, multiple rules)
	w.store.ScanObjects(func(meta metadata.ObjectMeta) bool {
		cfg, ok := configs[meta.Bucket]
		if !ok {
			return true
		}

		for i := range cfg.Rules {
			rule := &cfg.Rules[i]
			if rule.Status != "Enabled" {
				continue
			}
			if !matchRule(rule, &meta) {
				continue
			}

			// Current object expiration
			if rule.ExpirationDays > 0 && !meta.DeleteMarker {
				expiryTime := meta.LastModified + int64(rule.ExpirationDays)*86400
				if expiryTime <= now {
					if meta.LegalHold || (meta.RetentionMode != "" && meta.RetentionUntil > now) {
						continue
					}
					versioning, _ := w.store.GetBucketVersioning(meta.Bucket)
					if versioning == "Enabled" && meta.VersionID != "" {
						continue
					}
					if err := w.engine.DeleteObject(meta.Bucket, meta.Key); err != nil {
						slog.Error("lifecycle error deleting object", "bucket", meta.Bucket, "key", meta.Key, "error", err)
						continue
					}
					w.store.DeleteObjectMeta(meta.Bucket, meta.Key)
					expired++
					break
				}
			}
		}
		return true
	})

	// 2. Noncurrent version expiration + max noncurrent versions + expired delete marker cleanup
	for bucketName, cfg := range configs {
		for i := range cfg.Rules {
			rule := &cfg.Rules[i]
			if rule.Status != "Enabled" {
				continue
			}

			hasNoncurrentExpiry := rule.NoncurrentVersionExpirationDays > 0
			hasMaxVersions := rule.MaxNoncurrentVersions > 0
			hasDeleteMarkerCleanup := rule.ExpiredObjectDeleteMarker

			if !hasNoncurrentExpiry && !hasMaxVersions && !hasDeleteMarkerCleanup {
				continue
			}

			// Group versions by key
			keyVersions := make(map[string][]metadata.ObjectMeta)
			w.store.ScanObjectVersions(func(meta metadata.ObjectMeta) bool {
				if meta.Bucket != bucketName {
					return true
				}
				if rule.Prefix != "" && !strings.HasPrefix(meta.Key, rule.Prefix) {
					return true
				}
				keyVersions[meta.Key] = append(keyVersions[meta.Key], meta)
				return true
			})

			for key, versions := range keyVersions {
				// Sort by LastModified descending
				sort.Slice(versions, func(a, b int) bool {
					return versions[a].LastModified > versions[b].LastModified
				})

				// Find noncurrent versions (not the latest)
				var noncurrent []metadata.ObjectMeta
				for j, v := range versions {
					if j == 0 {
						continue // latest
					}
					noncurrent = append(noncurrent, v)
				}

				// Noncurrent version expiration by age
				if hasNoncurrentExpiry {
					cutoff := now - int64(rule.NoncurrentVersionExpirationDays)*86400
					for _, v := range noncurrent {
						if v.LastModified < cutoff {
							w.engine.DeleteObjectVersion(v.Bucket, v.Key, v.VersionID)
							w.store.DeleteObjectVersion(v.Bucket, v.Key, v.VersionID)
							noncurrentExpired++
						}
					}
				}

				// Max noncurrent versions
				if hasMaxVersions && len(noncurrent) > rule.MaxNoncurrentVersions {
					excess := noncurrent[rule.MaxNoncurrentVersions:]
					for _, v := range excess {
						w.engine.DeleteObjectVersion(v.Bucket, v.Key, v.VersionID)
						w.store.DeleteObjectVersion(v.Bucket, v.Key, v.VersionID)
						noncurrentExpired++
					}
				}

				// Expired delete marker cleanup: remove delete markers with no noncurrent versions
				if hasDeleteMarkerCleanup && len(versions) == 1 && versions[0].DeleteMarker {
					w.store.DeleteObjectVersion(bucketName, key, versions[0].VersionID)
					w.store.DeleteObjectMeta(bucketName, key)
					deleteMarkersRemoved++
				}
			}
		}
	}

	// 3. Abort incomplete multipart uploads
	for bucketName, cfg := range configs {
		for i := range cfg.Rules {
			rule := &cfg.Rules[i]
			if rule.Status != "Enabled" || rule.AbortIncompleteMultipartDays <= 0 {
				continue
			}
			uploads, err := w.store.ListMultipartUploads(bucketName)
			if err != nil {
				continue
			}
			cutoff := now - int64(rule.AbortIncompleteMultipartDays)*86400
			for _, upload := range uploads {
				if upload.CreatedAt < cutoff {
					if rule.Prefix != "" && !strings.HasPrefix(upload.Key, rule.Prefix) {
						continue
					}
					w.store.DeleteMultipartUpload(upload.UploadID)
					multipartAborted++
				}
			}
		}
	}

	if expired > 0 {
		slog.Info("lifecycle deleted expired objects", "count", expired)
	}
	if noncurrentExpired > 0 {
		slog.Info("lifecycle deleted noncurrent versions", "count", noncurrentExpired)
	}
	if multipartAborted > 0 {
		slog.Info("lifecycle aborted incomplete multipart uploads", "count", multipartAborted)
	}
	if deleteMarkersRemoved > 0 {
		slog.Info("lifecycle removed expired delete markers", "count", deleteMarkersRemoved)
	}

	w.pruneAndClean()
}

func (w *Worker) pruneAndClean() {
	// Prune old audit entries
	if w.auditRetentionDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -w.auditRetentionDays)
		pruned, err := w.store.PruneAuditEntries(cutoff)
		if err != nil {
			slog.Error("lifecycle error pruning audit entries", "error", err)
		} else if pruned > 0 {
			slog.Info("lifecycle pruned audit entries", "count", pruned)
		}
	}

	// Clean up expired STS keys
	deleted, err := w.store.DeleteExpiredAccessKeys()
	if err != nil {
		slog.Error("lifecycle error cleaning expired keys", "error", err)
	} else if deleted > 0 {
		slog.Info("lifecycle removed expired STS keys", "count", deleted)
	}
}
