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
	store  *metadata.Store
	siteID string
	mu     sync.Mutex
}

// NewChangeLog creates a change log backed by the metadata store.
func NewChangeLog(store *metadata.Store, siteID string) *ChangeLog {
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
