package erasure

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Healer periodically scans for degraded erasure-coded objects and repairs them.
type Healer struct {
	store        *metadata.Store
	engine       *Engine
	intervalSecs int
}

// NewHealer creates a new erasure coding healer.
func NewHealer(store *metadata.Store, engine *Engine, intervalSecs int) *Healer {
	if intervalSecs <= 0 {
		intervalSecs = 3600 // default: scan every hour
	}
	return &Healer{
		store:        store,
		engine:       engine,
		intervalSecs: intervalSecs,
	}
}

// Run starts the healer loop. Blocks until ctx is cancelled.
func (h *Healer) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(h.intervalSecs) * time.Second)
	defer ticker.Stop()

	slog.Info("erasure healer started", "interval_secs", h.intervalSecs)

	for {
		select {
		case <-ctx.Done():
			slog.Info("erasure healer stopped")
			return
		case <-ticker.C:
			h.scan()
		}
	}
}

func (h *Healer) scan() {
	buckets, _ := h.store.ListBuckets()
	repaired := 0
	scanned := 0

	for _, bucket := range buckets {
		// List .ec/ prefix to find erasure-coded objects
		objects, _, err := h.engine.inner.ListObjects(bucket.Name, ".ec/", "", 10000)
		if err != nil {
			continue
		}

		// Group by object key (extract from .ec/{key}/meta.json)
		seen := make(map[string]bool)
		for _, obj := range objects {
			key := extractObjectKey(obj.Key)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			scanned++

			if h.healObject(bucket.Name, key) {
				repaired++
			}
		}
	}

	if scanned > 0 {
		slog.Info("erasure healer scan complete", "scanned", scanned, "repaired", repaired)
	}
}

// healObject checks a single EC object and repairs missing shards.
// Returns true if repair was performed.
func (h *Healer) healObject(bucket, key string) bool {
	mKey := metaKey(key)
	metaReader, _, err := h.engine.backendFor(0).GetObject(bucket, mKey)
	if err != nil {
		return false
	}
	metaBytes, _ := io.ReadAll(metaReader)
	metaReader.Close()

	meta, err := UnmarshalShardMeta(metaBytes)
	if err != nil {
		return false
	}

	totalShards := meta.DataShards + meta.ParityShards

	// Check which shards are missing
	shards := make([][]byte, totalShards)
	missing := make([]int, 0)

	for i := 0; i < totalShards; i++ {
		backend := h.engine.backendFor(i)
		sKey := shardKey(key, i)

		reader, _, err := backend.GetObject(bucket, sKey)
		if err != nil {
			shards[i] = nil
			missing = append(missing, i)
			continue
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			shards[i] = nil
			missing = append(missing, i)
			continue
		}
		shards[i] = data
	}

	if len(missing) == 0 {
		return false // all shards intact
	}

	if len(missing) > meta.ParityShards {
		slog.Error("erasure healer: unrecoverable object",
			"bucket", bucket, "key", key,
			"missing", len(missing), "max_recoverable", meta.ParityShards,
		)
		return false
	}

	// Reconstruct missing shards
	encoder, err := NewEncoder(meta.DataShards, meta.ParityShards)
	if err != nil {
		slog.Error("erasure healer: create encoder failed", "error", err)
		return false
	}

	if err := encoder.Reconstruct(shards); err != nil {
		slog.Error("erasure healer: reconstruct failed",
			"bucket", bucket, "key", key, "error", err,
		)
		return false
	}

	// Write repaired shards back
	for _, idx := range missing {
		backend := h.engine.backendFor(idx)
		sKey := shardKey(key, idx)
		if _, _, err := backend.PutObject(bucket, sKey, bytes.NewReader(shards[idx]), int64(len(shards[idx]))); err != nil {
			slog.Error("erasure healer: write repaired shard failed",
				"bucket", bucket, "key", key, "shard", idx, "error", err,
			)
			continue
		}
	}

	slog.Info("erasure healer: repaired object",
		"bucket", bucket, "key", key,
		"shards_repaired", len(missing),
	)
	return true
}

// extractObjectKey extracts the original object key from an .ec/ path.
// Input: ".ec/some/path/to/file.txt/meta.json" → "some/path/to/file.txt"
// Input: ".ec/some/path/to/file.txt/shard-00" → "some/path/to/file.txt"
func extractObjectKey(ecPath string) string {
	if len(ecPath) < 5 || ecPath[:4] != ".ec/" {
		return ""
	}
	rest := ecPath[4:] // "some/path/to/file.txt/meta.json"

	// Find the last "/" before meta.json or shard-XX
	for i := len(rest) - 1; i >= 0; i-- {
		if rest[i] == '/' {
			suffix := rest[i+1:]
			if suffix == "meta.json" || (len(suffix) >= 6 && suffix[:6] == "shard-") {
				return rest[:i]
			}
		}
	}
	return ""
}

// HealStatus returns stats about erasure-coded objects.
type HealStatus struct {
	TotalObjects    int `json:"total_objects"`
	HealthyObjects  int `json:"healthy_objects"`
	DegradedObjects int `json:"degraded_objects"`
}

// Status scans and returns the current health status.
func (h *Healer) Status() HealStatus {
	status := HealStatus{}
	buckets, _ := h.store.ListBuckets()

	for _, bucket := range buckets {
		objects, _, err := h.engine.inner.ListObjects(bucket.Name, ".ec/", "", 10000)
		if err != nil {
			continue
		}

		seen := make(map[string]bool)
		for _, obj := range objects {
			key := extractObjectKey(obj.Key)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			status.TotalObjects++

			if h.isDegraded(bucket.Name, key) {
				status.DegradedObjects++
			} else {
				status.HealthyObjects++
			}
		}
	}

	return status
}

func (h *Healer) isDegraded(bucket, key string) bool {
	mKey := metaKey(key)
	metaReader, _, err := h.engine.backendFor(0).GetObject(bucket, mKey)
	if err != nil {
		return true
	}
	metaBytes, _ := io.ReadAll(metaReader)
	metaReader.Close()

	meta, err := UnmarshalShardMeta(metaBytes)
	if err != nil {
		return true
	}

	totalShards := meta.DataShards + meta.ParityShards
	for i := 0; i < totalShards; i++ {
		backend := h.engine.backendFor(i)
		if !backend.ObjectExists(bucket, shardKey(key, i)) {
			return true
		}
	}
	return false
}

// compile-time check: Engine implements storage.Engine
var _ storage.Engine = (*Engine)(nil)
