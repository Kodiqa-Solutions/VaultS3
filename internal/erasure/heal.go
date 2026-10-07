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
	store        metadata.StoreAPI
	engine       *Engine
	intervalSecs int
}

// NewHealer creates a new erasure coding healer.
func NewHealer(store metadata.StoreAPI, engine *Engine, intervalSecs int) *Healer {
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
	res := h.healScope("", "")
	if res.Scanned > 0 {
		slog.Info("erasure healer scan complete", "scanned", res.Scanned, "repaired", res.Repaired)
	}
}

// HealResult reports the outcome of an on-demand heal pass.
type HealResult struct {
	Scanned  int `json:"scanned"`
	Repaired int `json:"repaired"`
}

// Heal runs an on-demand heal pass. An empty bucket scans all buckets; a
// non-empty prefix restricts the scan to object keys under that prefix.
func (h *Healer) Heal(bucket, prefix string) HealResult {
	res := h.healScope(bucket, prefix)
	slog.Info("erasure manual heal complete",
		"bucket", bucket, "prefix", prefix,
		"scanned", res.Scanned, "repaired", res.Repaired,
	)
	return res
}

// healScope scans erasure-coded objects (optionally narrowed to a bucket and/or
// key prefix) and repairs any degraded ones. It performs no logging itself.
func (h *Healer) healScope(bucket, prefix string) HealResult {
	res := HealResult{}

	var bucketNames []string
	if bucket != "" {
		bucketNames = []string{bucket}
	} else {
		buckets, _ := h.store.ListBuckets()
		for _, b := range buckets {
			bucketNames = append(bucketNames, b.Name)
		}
	}

	for _, name := range bucketNames {
		h.forEachCodedObject(name, prefix, func(key string) {
			res.Scanned++
			if h.healObject(name, key) {
				res.Repaired++
			}
		})
	}

	return res
}

// healPageSize is how many object keys one metadata page holds. A var so a
// test can force several pages without writing thousands of objects.
var healPageSize = 1000

// forEachCodedObject calls fn for every erasure-coded object in bucket under
// prefix. It pages through the metadata store, which keeps keys sorted, and
// checks each key for a meta.json. The previous scan listed the .ec/ tree with
// a cap of 10,000 files, and every object has five or more files there, so on
// a bucket with more than about 2,000 coded objects the rest were never
// checked or repaired. A coded object with no metadata record cannot be read
// by anyone, so it has nothing worth healing.
//
// Each page is fetched and released before fn runs, so fn never executes
// inside a store transaction.
func (h *Healer) forEachCodedObject(bucket, prefix string, fn func(key string)) {
	after := ""
	for {
		objs, truncated, err := h.store.ListLatestObjects(bucket, prefix, after, healPageSize)
		if err != nil {
			slog.Error("erasure healer: list objects failed", "bucket", bucket, "error", err)
			return
		}
		for _, o := range objs {
			if h.engine.backendFor(0).ObjectExists(bucket, metaKey(o.Key)) {
				fn(o.Key)
			}
		}
		if !truncated || len(objs) == 0 {
			return
		}
		after = objs[len(objs)-1].Key
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

	// Check which shards are missing. A shard that is short or fails its
	// checksum counts as missing, so it is rebuilt and rewritten here too.
	shards := make([][]byte, totalShards)
	missing := make([]int, 0)

	for i := 0; i < totalShards; i++ {
		data, ok := h.engine.readWholeShard(bucket, key, meta, i)
		if !ok {
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
		sKey := meta.shardPath(key, idx)
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
		h.forEachCodedObject(bucket.Name, "", func(key string) {
			status.TotalObjects++
			if h.isDegraded(bucket.Name, key) {
				status.DegradedObjects++
			} else {
				status.HealthyObjects++
			}
		})
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
		if !backend.ObjectExists(bucket, meta.shardPath(key, i)) {
			return true
		}
	}
	return false
}

// compile-time check: Engine implements storage.Engine
var _ storage.Engine = (*Engine)(nil)
