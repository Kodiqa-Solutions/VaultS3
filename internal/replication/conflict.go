package replication

import (
	"time"
)

// ChangeEntry represents a single object mutation tracked for replication.
type ChangeEntry struct {
	Bucket      string      `json:"bucket"`
	Key         string      `json:"key"`
	EventType   string      `json:"event_type"` // "put" or "delete"
	SiteID      string      `json:"site_id"`
	VectorClock VectorClock `json:"vector_clock"`
	ETag        string      `json:"etag,omitempty"`
	Size        int64       `json:"size,omitempty"`
	Timestamp   int64       `json:"timestamp"` // unix nanos
	// Seq is the entry's position in the sending site's change log. It is set
	// when the log is read for a sync, so the puller can resume right after
	// the last change it applied. A peer older than this field sends 0.
	Seq uint64 `json:"seq,omitempty"`
}

// ConflictStrategy names the available conflict resolution strategies.
type ConflictStrategy string

const (
	StrategyLastWriterWins ConflictStrategy = "last-writer-wins"
	StrategyLargestObject  ConflictStrategy = "largest-object"
	StrategySitePreference ConflictStrategy = "site-preference"
)

// ConflictResolver picks a winner when two concurrent writes conflict.
type ConflictResolver interface {
	Resolve(local, remote ChangeEntry) ChangeEntry
}

// LastWriterWins resolves conflicts by choosing the entry with the latest timestamp.
type LastWriterWins struct{}

func (LastWriterWins) Resolve(local, remote ChangeEntry) ChangeEntry {
	if remote.Timestamp > local.Timestamp {
		return remote
	}
	if remote.Timestamp == local.Timestamp {
		// Deterministic tie-break: higher site ID wins
		if remote.SiteID > local.SiteID {
			return remote
		}
	}
	return local
}

// LargestObject resolves conflicts by choosing the larger object.
// Deletes always lose to puts.
type LargestObject struct{}

func (LargestObject) Resolve(local, remote ChangeEntry) ChangeEntry {
	// Put wins over delete
	if local.EventType == "delete" && remote.EventType == "put" {
		return remote
	}
	if local.EventType == "put" && remote.EventType == "delete" {
		return local
	}
	// Both puts: largest wins
	if remote.Size > local.Size {
		return remote
	}
	if remote.Size == local.Size {
		// Deterministic tie-break: higher ETag wins
		if remote.ETag > local.ETag {
			return remote
		}
	}
	return local
}

// SitePreference resolves conflicts by always preferring a specific site.
type SitePreference struct {
	PreferredSiteID string
}

func (sp SitePreference) Resolve(local, remote ChangeEntry) ChangeEntry {
	if remote.SiteID == sp.PreferredSiteID {
		return remote
	}
	return local
}

// NewConflictResolver creates a resolver from the strategy name and optional config.
func NewConflictResolver(strategy ConflictStrategy, preferredSite string) ConflictResolver {
	switch strategy {
	case StrategyLargestObject:
		return LargestObject{}
	case StrategySitePreference:
		return SitePreference{PreferredSiteID: preferredSite}
	default:
		return LastWriterWins{}
	}
}

// NowNanos returns the current time in unix nanoseconds.
func NowNanos() int64 {
	return time.Now().UnixNano()
}
