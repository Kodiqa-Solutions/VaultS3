package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

type Worker struct {
	store              metadata.StoreAPI
	engine             storage.Engine
	interval           time.Duration
	auditRetentionDays int
	// reap, if set (cluster mode), removes an expired object's data file from the
	// OTHER nodes. Expiry deletes the metadata through Raft, so the first node to
	// sweep hides the object from every other node's next sweep and their copies of
	// the data are stranded forever. Same leak as the multi-object delete had
	// (issue #47). Best-effort; correctness comes from metadata being authoritative.
	reap func(bucket, key, versionID string)
}

// SetReaper wires the cluster hook that drops an expired object's data on the
// other nodes. No-op single-node.
func (w *Worker) SetReaper(fn func(bucket, key, versionID string)) { w.reap = fn }

func (w *Worker) reapElsewhere(bucket, key, versionID string) {
	if w.reap != nil {
		w.reap(bucket, key, versionID)
	}
}

func NewWorker(store metadata.StoreAPI, engine storage.Engine, intervalSecs, auditRetentionDays int) *Worker {
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

	// Versioning is resolved before any walk. ScanObjects and
	// ScanObjectVersions hold a store read transaction for the whole walk, and
	// a nested store call from inside the callback (this used to ask for the
	// bucket's versioning, delete metadata through Raft and reach other nodes
	// over the network, all mid-walk) deadlocks the store once a writer queues
	// behind it. So the walks only collect, and everything acts after them.
	versioning := make(map[string]string, len(configs))
	for name := range configs {
		v, err := w.store.GetBucketVersioning(name)
		if err != nil {
			slog.Error("lifecycle error reading bucket versioning, skipping bucket", "bucket", name, "error", err)
			continue
		}
		versioning[name] = v
	}

	// 1. Current object expiration (with size/tag filters, multiple rules)
	var candidates []metadata.ObjectMeta
	w.store.ScanObjects(func(meta metadata.ObjectMeta) bool {
		cfg, ok := configs[meta.Bucket]
		if !ok {
			return true
		}
		vs, ok := versioning[meta.Bucket]
		if !ok {
			return true
		}
		if vs == "Enabled" && meta.VersionID != "" {
			return true
		}
		for i := range cfg.Rules {
			rule := &cfg.Rules[i]
			if rule.Status != "Enabled" || !matchRule(rule, &meta) {
				continue
			}
			if rule.ExpirationDays > 0 && !meta.DeleteMarker &&
				meta.LastModified+int64(rule.ExpirationDays)*86400 <= now {
				candidates = append(candidates, meta)
				break
			}
		}
		return true
	})
	for _, snap := range candidates {
		if w.expireCurrent(snap, versioning[snap.Bucket], now) {
			expired++
		}
	}

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

				// A version is deleted at most once per pass, even when it is
				// both too old and beyond the version count.
				gone := make(map[string]bool)

				// Noncurrent version expiration by age
				if hasNoncurrentExpiry {
					cutoff := now - int64(rule.NoncurrentVersionExpirationDays)*86400
					for _, v := range noncurrent {
						if v.LastModified < cutoff && !gone[v.VersionID] && w.expireVersion(v, now) {
							gone[v.VersionID] = true
							noncurrentExpired++
						}
					}
				}

				// Max noncurrent versions
				if hasMaxVersions && len(noncurrent) > rule.MaxNoncurrentVersions {
					excess := noncurrent[rule.MaxNoncurrentVersions:]
					for _, v := range excess {
						if !gone[v.VersionID] && w.expireVersion(v, now) {
							gone[v.VersionID] = true
							noncurrentExpired++
						}
					}
				}

				// Expired delete marker cleanup: remove delete markers with no noncurrent versions
				if hasDeleteMarkerCleanup && len(versions) == 1 && versions[0].DeleteMarker {
					if err := w.store.DeleteObjectVersion(bucketName, key, versions[0].VersionID); err != nil {
						slog.Error("lifecycle error removing expired delete marker", "bucket", bucketName, "key", key, "error", err)
						continue
					}
					if err := w.store.DeleteObjectMeta(bucketName, key); err != nil {
						slog.Error("lifecycle error removing expired delete marker", "bucket", bucketName, "key", key, "error", err)
						continue
					}
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
					if err := w.store.DeleteMultipartUpload(upload.UploadID); err != nil {
						slog.Error("lifecycle error aborting multipart upload", "upload", upload.UploadID, "error", err)
						continue
					}
					// Also remove the uploaded parts from disk, otherwise the
					// space they occupy is never reclaimed (deleting the metadata
					// alone leaves the part files behind). Mirrors the layout the
					// S3 AbortMultipartUpload handler uses.
					if safeUploadID(upload.UploadID) {
						os.RemoveAll(filepath.Join(w.engine.DataDir(), ".multipart", upload.UploadID))
					}
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

// locked reports whether object lock forbids destroying this version: a legal
// hold, or retention (either mode) that has not run out. Lifecycle has no way to
// carry a governance bypass, so GOVERNANCE blocks it as well.
func locked(m *metadata.ObjectMeta, now int64) bool {
	return m.LegalHold || (m.RetentionMode != "" && m.RetentionUntil > now)
}

// sameObject reports whether cur is still the object the scan saw. A PUT that
// replaced the key after the scan read it gives it a new ETag or modification
// time (or version), and expiring by key would then destroy the new data.
func sameObject(snap, cur *metadata.ObjectMeta) bool {
	return cur.ETag == snap.ETag && cur.LastModified == snap.LastModified &&
		cur.VersionID == snap.VersionID && cur.DeleteMarker == snap.DeleteMarker
}

// expireCurrent expires the current object the scan found, if it is still the
// same object and not locked. Unversioned buckets delete it. In a bucket with
// versioning Enabled or Suspended the object is hidden behind a delete marker,
// as S3 does: Enabled keeps the object as the "null" version, Suspended replaces
// the null version with a null marker and its data goes.
func (w *Worker) expireCurrent(snap metadata.ObjectMeta, versioning string, now int64) bool {
	cur, err := w.store.GetObjectMeta(snap.Bucket, snap.Key)
	if err != nil || cur == nil {
		return false // already gone
	}
	if !sameObject(&snap, cur) {
		return false // overwritten since the scan, so not this object any more
	}
	if locked(cur, now) {
		return false
	}

	switch versioning {
	case "Enabled", "Suspended":
		markerID := newVersionID()
		var demote *metadata.ObjectMeta
		if versioning == "Suspended" && (cur.VersionID == "" || cur.VersionID == "null") {
			// The null version is replaced, data included.
			markerID = "null"
			if err := w.engine.DeleteObjectVersion(cur.Bucket, cur.Key, "null"); err != nil && !os.IsNotExist(err) {
				slog.Error("lifecycle error deleting object", "bucket", cur.Bucket, "key", cur.Key, "error", err)
				return false
			}
			if cur.VersionID == "" {
				if err := w.engine.DeleteObject(cur.Bucket, cur.Key); err != nil && !os.IsNotExist(err) {
					slog.Error("lifecycle error deleting object", "bucket", cur.Bucket, "key", cur.Key, "error", err)
					return false
				}
			}
			if err := w.store.DeleteObjectVersion(cur.Bucket, cur.Key, "null"); err != nil {
				slog.Error("lifecycle error deleting object", "bucket", cur.Bucket, "key", cur.Key, "error", err)
				return false
			}
		} else {
			// The object stays as a noncurrent version. One written before
			// versioning was turned on has no version id: S3 calls it "null",
			// and its bytes stay at the ordinary object path.
			d := *cur
			if d.VersionID == "" {
				d.VersionID = "null"
			}
			d.IsLatest = false
			demote = &d
		}
		if demote != nil {
			if err := w.store.PutObjectVersion(*demote); err != nil {
				slog.Error("lifecycle error writing delete marker", "bucket", cur.Bucket, "key", cur.Key, "error", err)
				return false
			}
		}
		dm := metadata.ObjectMeta{
			Bucket:       cur.Bucket,
			Key:          cur.Key,
			VersionID:    markerID,
			IsLatest:     true,
			DeleteMarker: true,
			LastModified: time.Now().UTC().Unix(),
		}
		if err := w.store.PutObjectVersion(dm); err != nil {
			slog.Error("lifecycle error writing delete marker", "bucket", cur.Bucket, "key", cur.Key, "error", err)
			return false
		}
		if err := w.store.PutObjectMeta(dm); err != nil {
			slog.Error("lifecycle error writing delete marker", "bucket", cur.Bucket, "key", cur.Key, "error", err)
			return false
		}
		if markerID == "null" {
			w.reapElsewhere(cur.Bucket, cur.Key, "null")
		}
		return true
	}

	if err := w.engine.DeleteObject(cur.Bucket, cur.Key); err != nil {
		slog.Error("lifecycle error deleting object", "bucket", cur.Bucket, "key", cur.Key, "error", err)
		return false
	}
	if err := w.store.DeleteObjectMeta(cur.Bucket, cur.Key); err != nil {
		slog.Error("lifecycle error deleting object metadata", "bucket", cur.Bucket, "key", cur.Key, "error", err)
		return false
	}
	w.reapElsewhere(cur.Bucket, cur.Key, "")
	return true
}

// expireVersion permanently removes one noncurrent version, unless object lock
// protects it or it has changed since the scan. Engine and store failures are
// logged and the version is not counted, so a failed delete is never reported
// as done.
func (w *Worker) expireVersion(v metadata.ObjectMeta, now int64) bool {
	cur, err := w.store.GetObjectVersion(v.Bucket, v.Key, v.VersionID)
	if err != nil || cur == nil {
		return false
	}
	if locked(cur, now) {
		return false
	}
	// The latest pointer is the authority on which version is current. A PUT
	// since the scan can have made this one current again (a "null" rewrite),
	// and the current version is never expired as noncurrent.
	latest, lerr := w.store.GetObjectMeta(v.Bucket, v.Key)
	if lerr == nil && latest != nil && latest.VersionID == v.VersionID {
		return false
	}
	if !cur.DeleteMarker {
		if err := w.engine.DeleteObjectVersion(v.Bucket, v.Key, v.VersionID); err != nil && !os.IsNotExist(err) {
			slog.Error("lifecycle error deleting noncurrent version", "bucket", v.Bucket, "key", v.Key, "version", v.VersionID, "error", err)
			return false
		}
		// A null version written before versioning was turned on keeps its
		// bytes at the ordinary object path, which DeleteObjectVersion does not
		// touch. That path holds no other version, because once versioning is
		// on new writes go under the versions directory.
		if v.VersionID == "null" {
			if lerr != nil || latest == nil || latest.VersionID != "" {
				if err := w.engine.DeleteObject(v.Bucket, v.Key); err != nil && !os.IsNotExist(err) {
					slog.Error("lifecycle error deleting noncurrent version", "bucket", v.Bucket, "key", v.Key, "version", v.VersionID, "error", err)
					return false
				}
			}
		}
	}
	if err := w.store.DeleteObjectVersion(v.Bucket, v.Key, v.VersionID); err != nil {
		slog.Error("lifecycle error deleting noncurrent version", "bucket", v.Bucket, "key", v.Key, "version", v.VersionID, "error", err)
		return false
	}
	w.reapElsewhere(v.Bucket, v.Key, v.VersionID)
	return true
}

// newVersionID matches the S3 handler's version ids: a nanosecond timestamp then
// random bytes, so ids sort by creation time.
func newVersionID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%016x%s", time.Now().UnixNano(), hex.EncodeToString(b))
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

// safeUploadID guards the parts-directory removal against path traversal. Upload
// IDs are server-generated, but this is defense in depth before an os.RemoveAll.
func safeUploadID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}
