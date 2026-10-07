package erasure

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
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
	// bucketEnabled, when set, decides per bucket whether writes are erasure
	// coded, so a bucket holding disposable data can skip the parity overhead
	// while others keep it (issue #39). Nil means every bucket is coded, which is
	// how erasure behaved before it could be set per bucket.
	//
	// Only writes consult this. Reads and deletes detect the format from the
	// object itself, so flipping the setting never strands data already written
	// either way, and a bucket may legitimately hold both kinds.
	bucketEnabled func(bucket string) bool
}

// SetBucketPolicy wires the per-bucket erasure decision (issue #39). No-op when
// unset: every bucket is erasure coded, matching the previous global behaviour.
func (e *Engine) SetBucketPolicy(fn func(bucket string) bool) { e.bucketEnabled = fn }

// encodesBucket reports whether new writes to a bucket should be erasure coded.
func (e *Engine) encodesBucket(bucket string) bool {
	if e.bucketEnabled == nil {
		return true
	}
	return e.bucketEnabled(bucket)
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
	// A bucket that opted out of erasure coding streams straight to the inner
	// engine, so it pays neither the parity overhead nor the whole-object buffer
	// that encoding requires (issue #39).
	if !e.encodesBucket(bucket) {
		return e.putPlain(bucket, key, reader, size)
	}

	// Stream when the length is known and the object is large enough to be
	// erasure coded. This avoids holding the object and its parity in memory,
	// which is what made concurrent large PUTs an OOM risk. A chunked upload
	// arrives with size < 0 and a small object is stored whole by the inner
	// engine, so both fall through to the buffering path below.
	if size >= e.cfg.BlockSize {
		return e.putObjectStreaming(bucket, key, reader, size)
	}

	// Read all data into memory
	data, err := io.ReadAll(reader)
	if err != nil {
		return 0, "", fmt.Errorf("read object data: %w", err)
	}
	actualSize := int64(len(data))

	// Small objects: store directly without EC
	if actualSize < e.cfg.BlockSize {
		return e.putPlain(bucket, key, bytes.NewReader(data), actualSize)
	}

	// Erasure code the object
	shards, err := e.encoder.Encode(data)
	if err != nil {
		return 0, "", fmt.Errorf("erasure encode: %w", err)
	}

	// Compute ETag on original data
	hash := md5.Sum(data)
	etag := fmt.Sprintf("%x", hash)

	// The shards go under a fresh generation and meta.json is switched last,
	// exactly as in the streaming path. This path used to write meta.json
	// FIRST, so for the length of the write every reader was pointed at a
	// half-written set of shards.
	meta := &ShardMeta{
		OriginalSize: actualSize,
		DataShards:   e.cfg.DataShards,
		ParityShards: e.cfg.ParityShards,
		BlockSize:    e.cfg.BlockSize,
		ShardSizes:   make([]int64, len(shards)),
		ETag:         etag,
		CreatedAt:    time.Now().UTC(),
		Generation:   newGeneration(),
		ShardCRC:     make([]string, len(shards)),
		CRCStripe:    crcStripeBytes,
	}

	// Distribute shards across backends
	for i, shard := range shards {
		meta.ShardSizes[i] = int64(len(shard))
		sum := newStripeSummer(crcStripeBytes)
		sum.Write(shard)
		meta.ShardCRC[i] = sum.encoded()
		if _, _, err := e.backendFor(i).PutObject(bucket, meta.shardPath(key, i), bytes.NewReader(shard), int64(len(shard))); err != nil {
			e.removeShards(bucket, key, meta, i+1)
			return 0, "", fmt.Errorf("store shard %d: %w", i, err)
		}
	}

	if err := e.commitMeta(bucket, key, meta); err != nil {
		e.removeShards(bucket, key, meta, -1)
		return 0, "", err
	}
	return actualSize, etag, nil
}

// putPlain stores an object whole in the inner engine and then retires any
// erasure-coded version of the same key. GetObject looks for meta.json first,
// so leaving it behind kept serving the OLD content after a small object, or
// any object in a bucket that opted out of erasure, replaced a coded one.
func (e *Engine) putPlain(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	n, etag, err := e.inner.PutObject(bucket, key, reader, size)
	if err != nil {
		return n, etag, err
	}
	if err := e.dropErasureCopy(bucket, key); err != nil {
		return 0, "", err
	}
	return n, etag, nil
}

// dropErasureCopy removes an erasure-coded version of key, meta.json first so
// reads switch to the plain object at once. Failing to remove meta.json is an
// error, because the old version would still be what every read returns. A
// shard left behind after that is only unreferenced space, so it is logged.
func (e *Engine) dropErasureCopy(bucket, key string) error {
	mKey := metaKey(key)
	if !e.backendFor(0).ObjectExists(bucket, mKey) {
		return nil
	}
	old, merr := e.readShardMeta(bucket, key)
	if err := e.backendFor(0).DeleteObject(bucket, mKey); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("retire erasure-coded version: %w", err)
	}
	if merr != nil {
		slog.Warn("erasure: shard meta unreadable, its shards are left behind", "bucket", bucket, "key", key, "error", merr)
		return nil
	}
	if err := e.removeShards(bucket, key, old, -1); err != nil {
		slog.Warn("erasure: could not remove superseded shards", "bucket", bucket, "key", key, "error", err)
	}
	return nil
}

// commitMeta makes a fully written set of shards the current version by
// replacing meta.json, which the inner engine does atomically (temp file and
// rename), and then retires what it replaced: the previous version's shards
// and any plain copy of the key. A failure after the switch does not fail the
// write, since the new version is already complete and current, so those
// leftovers are logged instead.
func (e *Engine) commitMeta(bucket, key string, meta *ShardMeta) error {
	var old *ShardMeta
	if e.backendFor(0).ObjectExists(bucket, metaKey(key)) {
		old, _ = e.readShardMeta(bucket, key)
	}
	metaBytes, err := meta.Marshal()
	if err != nil {
		return fmt.Errorf("marshal shard meta: %w", err)
	}
	if _, _, err := e.backendFor(0).PutObject(bucket, metaKey(key), bytes.NewReader(metaBytes), int64(len(metaBytes))); err != nil {
		return fmt.Errorf("store shard meta: %w", err)
	}
	if old != nil && old.Generation != meta.Generation {
		if err := e.removeShards(bucket, key, old, -1); err != nil {
			slog.Warn("erasure: could not remove superseded shards", "bucket", bucket, "key", key, "error", err)
		}
	}
	// A plain copy left by an earlier small write would otherwise be served
	// again once this version is deleted.
	if e.inner.ObjectExists(bucket, key) {
		if err := e.inner.DeleteObject(bucket, key); err != nil {
			slog.Warn("erasure: could not remove superseded plain object", "bucket", bucket, "key", key, "error", err)
		}
	}
	return nil
}

// removeShards deletes the first n shards of a version (all of them when n is
// negative) and returns the first error. A shard that is already gone is not
// an error.
func (e *Engine) removeShards(bucket, key string, meta *ShardMeta, n int) error {
	total := meta.totalShards()
	if n < 0 || n > total {
		n = total
	}
	var first error
	for i := 0; i < n; i++ {
		if err := e.backendFor(i).DeleteObject(bucket, meta.shardPath(key, i)); err != nil && !os.IsNotExist(err) && first == nil {
			first = fmt.Errorf("delete shard %d: %w", i, err)
		}
	}
	return first
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
	return e.openCoded(bucket, key, true)
}

func (e *Engine) openCoded(bucket, key string, mayRetry bool) (storage.ReadSeekCloser, int64, error) {
	meta, err := e.readShardMeta(bucket, key)
	if err != nil {
		return nil, 0, err
	}

	// Fast path: while every data shard is intact the object is simply their
	// concatenation, so stream them instead of reading and reassembling the whole
	// object before the first byte. This keeps GET time-to-first-byte flat rather
	// than proportional to object size (issue #38).
	if st, ok := e.newShardStream(bucket, key, meta); ok {
		return st, meta.OriginalSize, nil
	}

	// Degraded: a data shard is missing, so parity recovery is required. Recover
	// it a stripe at a time so first-byte latency stays flat, and only fall back
	// to reading and decoding the whole object if that is not possible.
	if ds, ok := e.newDegradedStream(bucket, key, meta); ok {
		return ds, meta.OriginalSize, nil
	}

	data, err := e.reconstruct(bucket, key, meta)
	if err != nil {
		// An overwrite that committed between reading meta.json and opening
		// the shards retires the generation this read was pointed at. That is
		// not damage, so follow meta.json to the new version once.
		if cur, merr := e.readShardMeta(bucket, key); mayRetry && merr == nil && meta.Generation != "" && cur.Generation != meta.Generation {
			return e.openCoded(bucket, key, false)
		}
		return nil, 0, err
	}
	return newBytesReadSeekCloser(data), meta.OriginalSize, nil
}

// readShardMeta loads and parses an erasure-coded object's shard metadata.
func (e *Engine) readShardMeta(bucket, key string) (*ShardMeta, error) {
	metaReader, _, err := e.backendFor(0).GetObject(bucket, metaKey(key))
	if err != nil {
		return nil, fmt.Errorf("read shard meta: %w", err)
	}
	metaBytes, err := io.ReadAll(metaReader)
	metaReader.Close()
	if err != nil {
		return nil, fmt.Errorf("read shard meta data: %w", err)
	}
	meta, err := UnmarshalShardMeta(metaBytes)
	if err != nil {
		return nil, fmt.Errorf("parse shard meta: %w", err)
	}
	return meta, nil
}

// reconstruct reads every available shard and rebuilds the original object with
// Reed-Solomon parity recovery. This is the degraded path: correct but it must read
// the whole object (and verify it) before returning any bytes.
func (e *Engine) reconstruct(bucket, key string, meta *ShardMeta) ([]byte, error) {
	totalShards := meta.DataShards + meta.ParityShards
	shards := make([][]byte, totalShards)
	missingCount := 0

	for i := 0; i < totalShards; i++ {
		data, ok := e.readWholeShard(bucket, key, meta, i)
		if !ok {
			shards[i] = nil
			missingCount++
			continue
		}
		shards[i] = data
	}

	if missingCount > meta.ParityShards {
		return nil, fmt.Errorf("too many missing shards: %d missing, %d parity available", missingCount, meta.ParityShards)
	}

	if missingCount > 0 {
		slog.Warn("erasure: reconstructing from degraded shards",
			"bucket", bucket, "key", key,
			"missing", missingCount, "parity", meta.ParityShards,
		)
	}

	encoder, err := NewEncoder(meta.DataShards, meta.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("create decoder: %w", err)
	}

	data, err := encoder.Decode(shards, meta.OriginalSize)
	if err != nil {
		return nil, fmt.Errorf("erasure decode: %w", err)
	}
	return data, nil
}

// readWholeShard reads shard i in full and reports whether it is usable: present,
// readable, the length meta.json recorded, and matching its checksums when the
// version has them. An unusable shard is treated as missing, so parity covers it.
func (e *Engine) readWholeShard(bucket, key string, meta *ShardMeta, i int) ([]byte, bool) {
	reader, _, err := e.backendFor(i).GetObject(bucket, meta.shardPath(key, i))
	if err != nil {
		return nil, false
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		return nil, false
	}
	if i < len(meta.ShardSizes) && int64(len(data)) != meta.ShardSizes[i] {
		return nil, false
	}
	if !meta.stripeOK(i, 0, data) {
		slog.Warn("erasure: shard failed its checksum", "bucket", bucket, "key", key, "shard", i)
		return nil, false
	}
	return data, true
}

func (e *Engine) DeleteObject(bucket, key string) error {
	mKey := metaKey(key)
	if !e.backendFor(0).ObjectExists(bucket, mKey) {
		// Not erasure-coded
		return e.inner.DeleteObject(bucket, key)
	}
	meta, merr := e.readShardMeta(bucket, key)
	// meta.json goes first: once it is gone the object reads as deleted, and a
	// shard a later step fails to remove is only unreferenced space. Every
	// error used to be discarded here and the delete reported success, so a
	// failed delete left the whole object readable.
	if err := e.backendFor(0).DeleteObject(bucket, mKey); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete shard meta: %w", err)
	}
	var first error
	if merr != nil {
		slog.Warn("erasure: shard meta unreadable, its shards are left behind", "bucket", bucket, "key", key, "error", merr)
	} else if err := e.removeShards(bucket, key, meta, -1); err != nil {
		first = err
	}
	// A plain copy beside the coded one would be served once meta.json is gone.
	if err := e.inner.DeleteObject(bucket, key); err != nil && !os.IsNotExist(err) && first == nil {
		first = err
	}
	return first
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
	// The inner listing includes the .ec/ files that hold shards, which are
	// filtered out here. Two things went wrong with the old fixed over-fetch of
	// maxKeys+100: maxKeys 0 (unlimited, which backups use) became a limit of
	// 100 and then returned nothing at all, and a page made entirely of .ec/
	// files ended the listing early. This pages through the inner engine until
	// it has enough real objects or runs out.
	var out []storage.ObjectInfo
	after := startAfter
	for {
		want := 0
		if maxKeys > 0 {
			want = maxKeys - len(out) + 1 // one extra to learn whether more exist
		}
		objects, truncated, err := e.inner.ListObjects(bucket, prefix, after, want)
		if err != nil {
			return nil, false, err
		}
		for _, obj := range objects {
			if strings.HasPrefix(obj.Key, ".ec/") {
				continue
			}
			out = append(out, obj)
		}
		if maxKeys > 0 && len(out) > maxKeys {
			return out[:maxKeys], true, nil
		}
		if !truncated || len(objects) == 0 {
			return out, false, nil
		}
		after = objects[len(objects)-1].Key
		// Inside the shard tree, jump past all of it rather than paging
		// through every shard file.
		if strings.HasPrefix(after, ".ec/") && after < ecListSkip {
			after = ecListSkip
		}
	}
}

// ecListSkip sorts after every key under .ec/ and before any other key: object
// keys are UTF-8, which never contains the byte 0xff.
const ecListSkip = ".ec/\xff"

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
