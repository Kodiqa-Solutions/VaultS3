package replication

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// ChangeLog tracks object mutations for efficient delta sync between sites.
// It records each put/delete with the site's vector clock so remote sites
// can request only changes they haven't seen yet.
type ChangeLog struct {
	store  metadata.StoreAPI
	siteID string
	mu     sync.Mutex
}

// NewChangeLog creates a change log backed by the metadata store.
func NewChangeLog(store metadata.StoreAPI, siteID string) *ChangeLog {
	return &ChangeLog{
		store:  store,
		siteID: siteID,
	}
}

// Record appends a new change entry to the log.
func (cl *ChangeLog) Record(bucket, key, eventType, etag string, size int64, vc VectorClock) error {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	entry := ChangeEntry{
		Bucket:      bucket,
		Key:         key,
		EventType:   eventType,
		SiteID:      cl.siteID,
		VectorClock: vc,
		ETag:        etag,
		Size:        size,
		Timestamp:   NowNanos(),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal change entry: %w", err)
	}

	return cl.store.AppendChangeLog(data)
}

// RecordVersion appends the creation of a version or delete marker in a bucket
// with versioning. Versions are never rewritten, so they need no vector clock:
// a site that already holds the version ID has the change.
func (cl *ChangeLog) RecordVersion(meta metadata.ObjectMeta) error {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	// Not "delete": a site older than 5.0.12 deletes the object outright on
	// a "delete", where this is only a marker. It treats any other type as a
	// put and fetches the key, whose current version is the marker, gets 404
	// and skips the change.
	eventType := "put"
	if meta.DeleteMarker {
		eventType = "delete-marker"
	}
	entry := ChangeEntry{
		Bucket:       meta.Bucket,
		Key:          meta.Key,
		EventType:    eventType,
		SiteID:       cl.siteID,
		ETag:         meta.ETag,
		Size:         meta.Size,
		Timestamp:    NowNanos(),
		VersionID:    meta.VersionID,
		DeleteMarker: meta.DeleteMarker,
		LastModified: meta.LastModified,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal change entry: %w", err)
	}
	return cl.store.AppendChangeLog(data)
}

// ChangesSince returns all change entries with a sequence number greater than `sinceSeq`.
// Returns the entries and the latest sequence number seen.
func (cl *ChangeLog) ChangesSince(sinceSeq uint64, limit int) ([]ChangeEntry, uint64, error) {
	if limit <= 0 {
		limit = 1000
	}

	rawEntries, err := cl.store.ReadChangeLog(sinceSeq, limit)
	if err != nil {
		return nil, sinceSeq, fmt.Errorf("read change log: %w", err)
	}

	var entries []ChangeEntry
	var maxSeq uint64

	for _, raw := range rawEntries {
		if len(raw.Key) != 8 {
			continue
		}
		seq := binary.BigEndian.Uint64(raw.Key)
		if seq > maxSeq {
			maxSeq = seq
		}

		var entry ChangeEntry
		if err := json.Unmarshal(raw.Value, &entry); err != nil {
			continue // skip corrupt entries
		}
		entry.Seq = seq
		entries = append(entries, entry)
	}

	if maxSeq == 0 {
		maxSeq = sinceSeq
	}

	return entries, maxSeq, nil
}

// Trim removes change log entries older than the given sequence number.
func (cl *ChangeLog) Trim(beforeSeq uint64) error {
	return cl.store.TrimChangeLog(beforeSeq)
}
