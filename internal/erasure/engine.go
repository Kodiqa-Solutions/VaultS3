package erasure

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Engine wraps an inner storage.Engine with Reed-Solomon erasure coding.
// Objects larger than BlockSize are split into data+parity shards.
// Small objects (< BlockSize) are stored directly without EC for efficiency.
type Engine struct {
	inner   storage.Engine
	encoder *Encoder
	cfg     Config
	// backends are the storage engines for distributing shards.
	// backends[0] is always the inner engine. Additional backends
	// come from extra data directories (for single-node multi-disk EC).
	backends []storage.Engine
}

// NewEngine creates an erasure coding engine wrapping the inner engine.
func NewEngine(inner storage.Engine, cfg Config) (*Engine, error) {
	applyDefaults(&cfg)

	encoder, err := NewEncoder(cfg.DataShards, cfg.ParityShards)
	if err != nil {
		return nil, err
	}

	backends := []storage.Engine{inner}
	// Create additional filesystem backends for extra data dirs
	for _, dir := range cfg.DataDirs {
		fs, err := storage.NewFileSystem(dir)
		if err != nil {
			return nil, fmt.Errorf("init extra data dir %s: %w", dir, err)
		}
		backends = append(backends, fs)
	}

	return &Engine{
		inner:    inner,
		encoder:  encoder,
		cfg:      cfg,
		backends: backends,
	}, nil
}

// --- Bucket operations (delegate directly) ---

func (e *Engine) CreateBucketDir(bucket string) error {
	for _, b := range e.backends {
		if err := b.CreateBucketDir(bucket); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) DeleteBucketDir(bucket string) error {
	for _, b := range e.backends {
		if err := b.DeleteBucketDir(bucket); err != nil {
			slog.Warn("erasure: delete bucket dir failed on backend", "error", err)
		}
	}
	return nil
}

// --- Object operations ---

func (e *Engine) PutObject(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	// Read all data into memory
	data, err := io.ReadAll(reader)
	if err != nil {
		return 0, "", fmt.Errorf("read object data: %w", err)
	}
	actualSize := int64(len(data))

	// Small objects: store directly without EC
	if actualSize < e.cfg.BlockSize {
		return e.inner.PutObject(bucket, key, bytes.NewReader(data), actualSize)
	}

	// Erasure code the object
	shards, err := e.encoder.Encode(data)
	if err != nil {
		return 0, "", fmt.Errorf("erasure encode: %w", err)
	}

	// Compute ETag on original data
	hash := md5.Sum(data)
	etag := fmt.Sprintf("%x", hash)

	// Store shard metadata
	meta := &ShardMeta{
		OriginalSize: actualSize,
		DataShards:   e.cfg.DataShards,
		ParityShards: e.cfg.ParityShards,
		BlockSize:    e.cfg.BlockSize,
		ShardSizes:   make([]int64, len(shards)),
		ETag:         etag,
		CreatedAt:    time.Now().UTC(),
	}

	for i, shard := range shards {
		meta.ShardSizes[i] = int64(len(shard))
	}

	metaBytes, err := meta.Marshal()
	if err != nil {
		return 0, "", fmt.Errorf("marshal shard meta: %w", err)
	}

	// Store metadata file
	mKey := metaKey(key)
	if _, _, err := e.backendFor(0).PutObject(bucket, mKey, bytes.NewReader(metaBytes), int64(len(metaBytes))); err != nil {
		return 0, "", fmt.Errorf("store shard meta: %w", err)
	}

	// Distribute shards across backends
	for i, shard := range shards {
		backend := e.backendFor(i)
		sKey := shardKey(key, i)
		if _, _, err := backend.PutObject(bucket, sKey, bytes.NewReader(shard), int64(len(shard))); err != nil {
			return 0, "", fmt.Errorf("store shard %d: %w", i, err)
		}
	}

	return actualSize, etag, nil
}

func (e *Engine) GetObject(bucket, key string) (storage.ReadSeekCloser, int64, error) {
	// Check if this is an erasure-coded object (has shard metadata)
	mKey := metaKey(key)
	if e.backendFor(0).ObjectExists(bucket, mKey) {
		return e.getErasureCoded(bucket, key)
	}

	// Not erasure-coded — delegate to inner
	return e.inner.GetObject(bucket, key)
}

func (e *Engine) getErasureCoded(bucket, key string) (storage.ReadSeekCloser, int64, error) {
	// Read metadata
	mKey := metaKey(key)
	metaReader, _, err := e.backendFor(0).GetObject(bucket, mKey)
	if err != nil {
		return nil, 0, fmt.Errorf("read shard meta: %w", err)
	}
	metaBytes, err := io.ReadAll(metaReader)
	metaReader.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("read shard meta data: %w", err)
	}

	meta, err := UnmarshalShardMeta(metaBytes)
	if err != nil {
		return nil, 0, fmt.Errorf("parse shard meta: %w", err)
	}

	// Read all shards (nil for missing ones)
	totalShards := meta.DataShards + meta.ParityShards
	shards := make([][]byte, totalShards)
	missingCount := 0

	for i := 0; i < totalShards; i++ {
		backend := e.backendFor(i)
		sKey := shardKey(key, i)

		reader, _, err := backend.GetObject(bucket, sKey)
		if err != nil {
			shards[i] = nil
			missingCount++
			continue
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			shards[i] = nil
			missingCount++
			continue
		}
		shards[i] = data
	}

	if missingCount > meta.ParityShards {
		return nil, 0, fmt.Errorf("too many missing shards: %d missing, %d parity available", missingCount, meta.ParityShards)
	}

	if missingCount > 0 {
		slog.Warn("erasure: reconstructing from degraded shards",
			"bucket", bucket, "key", key,
			"missing", missingCount, "parity", meta.ParityShards,
		)
	}

	// Reconstruct original data
	// Need encoder with matching config
	encoder, err := NewEncoder(meta.DataShards, meta.ParityShards)
	if err != nil {
		return nil, 0, fmt.Errorf("create decoder: %w", err)
	}

	data, err := encoder.Decode(shards, meta.OriginalSize)
	if err != nil {
		return nil, 0, fmt.Errorf("erasure decode: %w", err)
	}

	return newBytesReadSeekCloser(data), meta.OriginalSize, nil
}

func (e *Engine) DeleteObject(bucket, key string) error {
	// Delete erasure-coded shards if they exist
	mKey := metaKey(key)
	if e.backendFor(0).ObjectExists(bucket, mKey) {
		// Read meta to know shard count
		metaReader, _, err := e.backendFor(0).GetObject(bucket, mKey)
		if err == nil {
			metaBytes, _ := io.ReadAll(metaReader)
			metaReader.Close()
			if meta, err := UnmarshalShardMeta(metaBytes); err == nil {
				totalShards := meta.DataShards + meta.ParityShards
				for i := 0; i < totalShards; i++ {
					backend := e.backendFor(i)
					_ = backend.DeleteObject(bucket, shardKey(key, i))
				}
			}
		}
		// Delete metadata
		_ = e.backendFor(0).DeleteObject(bucket, mKey)
		return nil
	}

	// Not erasure-coded
	return e.inner.DeleteObject(bucket, key)
}

func (e *Engine) ObjectExists(bucket, key string) bool {
	// Check for EC metadata first
	if e.backendFor(0).ObjectExists(bucket, metaKey(key)) {
		return true
	}
	return e.inner.ObjectExists(bucket, key)
}

func (e *Engine) ObjectSize(bucket, key string) (int64, error) {
	// Check for EC metadata
	mKey := metaKey(key)
	if e.backendFor(0).ObjectExists(bucket, mKey) {
		metaReader, _, err := e.backendFor(0).GetObject(bucket, mKey)
		if err != nil {
			return 0, err
		}
		metaBytes, _ := io.ReadAll(metaReader)
		metaReader.Close()
		meta, err := UnmarshalShardMeta(metaBytes)
		if err != nil {
			return 0, err
		}
		return meta.OriginalSize, nil
	}
	return e.inner.ObjectSize(bucket, key)
}

// --- List operations ---

func (e *Engine) ListObjects(bucket, prefix, startAfter string, maxKeys int) ([]storage.ObjectInfo, bool, error) {
	objects, truncated, err := e.inner.ListObjects(bucket, prefix, startAfter, maxKeys+100) // over-fetch to filter
	if err != nil {
		return nil, false, err
	}

	// Filter out .ec/ internal files
	filtered := make([]storage.ObjectInfo, 0, len(objects))
	for _, obj := range objects {
		if len(obj.Key) >= 4 && obj.Key[:4] == ".ec/" {
			continue
		}
		filtered = append(filtered, obj)
	}

	// Re-apply maxKeys limit
	if len(filtered) > maxKeys {
		return filtered[:maxKeys], true, nil
	}
	return filtered, truncated && len(filtered) >= maxKeys, nil
}

// --- Version operations (delegate — EC applies at object level, not version level for simplicity) ---

func (e *Engine) PutObjectVersion(bucket, key, versionID string, reader io.Reader, size int64) (int64, string, error) {
	return e.inner.PutObjectVersion(bucket, key, versionID, reader, size)
}

func (e *Engine) GetObjectVersion(bucket, key, versionID string) (storage.ReadSeekCloser, int64, error) {
	return e.inner.GetObjectVersion(bucket, key, versionID)
}

func (e *Engine) DeleteObjectVersion(bucket, key, versionID string) error {
	return e.inner.DeleteObjectVersion(bucket, key, versionID)
}

// --- Stats ---

func (e *Engine) BucketSize(bucket string) (int64, int64, error) {
	return e.inner.BucketSize(bucket)
}

// --- Paths ---

func (e *Engine) DataDir() string {
	return e.inner.DataDir()
}

func (e *Engine) ObjectPath(bucket, key string) string {
	return e.inner.ObjectPath(bucket, key)
}

// --- Helpers ---

// backendFor returns the storage backend for a given shard index.
// Distributes shards round-robin across available backends.
func (e *Engine) backendFor(shardIndex int) storage.Engine {
	if len(e.backends) <= 1 {
		return e.inner
	}
	return e.backends[shardIndex%len(e.backends)]
}

// bytesReadSeekCloser wraps a byte slice as ReadSeekCloser.
type bytesReadSeekCloser struct {
	*bytes.Reader
}

func newBytesReadSeekCloser(data []byte) storage.ReadSeekCloser {
	return &bytesReadSeekCloser{Reader: bytes.NewReader(data)}
}

func (b *bytesReadSeekCloser) Close() error { return nil }
