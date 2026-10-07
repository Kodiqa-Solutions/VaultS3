package erasure

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// shardStream serves an erasure-coded object by streaming its data shards in
// order, so a GET emits its first byte after reading only the first shard block
// rather than after reading and reassembling the whole object (issue #38).
//
// This is possible because the code is systematic Reed-Solomon: Split() writes the
// original bytes into the data shards unchanged (equal sized, the last one
// zero-padded) and Join() simply concatenates data shards 0..k-1 and truncates to
// OriginalSize. So the plaintext at logical offset o lives in data shard
// o/perShard at offset o%perShard, and no parity math is needed while every data
// shard is intact.
//
// Reads go a stripe at a time (crcStripeBytes, aligned to the shard), so that
// when the version carries per-stripe checksums each stripe is verified before
// any of its bytes are returned. If a data shard turns out to be missing,
// unreadable or corrupt, the stream switches to the degraded reader with that
// shard excluded and continues from the same logical offset, so the read
// returns correct bytes, recovered from parity. Before checksums a
// corrupt-but-present shard was served as it was, since cross-shard parity is
// not run on the healthy path (it would mean reading every shard).
type shardStream struct {
	e      *Engine
	bucket string
	key    string
	meta   *ShardMeta

	perShard int64 // size of each data shard (all data shards are equal sized)
	size     int64 // OriginalSize: logical length of the object
	pos      int64 // current logical read offset

	// readers holds every data shard, opened up front. An open file survives
	// being unlinked, so a concurrent overwrite retiring this version cannot
	// pull a shard out from under a read in progress.
	readers []storage.ReadSeekCloser

	buf      []byte // the loaded stripe
	bufShard int    // which data shard buf came from, -1 when none
	bufOff   int64  // buf's offset within that shard

	// alt takes over once a data shard failed: a degraded stream, or the whole
	// reconstructed object when even that cannot be built.
	alt storage.ReadSeekCloser
}

// newShardStream builds a streaming reader when every data shard is present.
// Returns false when the object cannot be streamed (unexpected shard layout or a
// missing data shard), in which case the caller uses the reconstructing path.
func (e *Engine) newShardStream(bucket, key string, meta *ShardMeta) (*shardStream, bool) {
	if meta.DataShards <= 0 || len(meta.ShardSizes) < meta.DataShards {
		return nil, false
	}
	perShard := meta.ShardSizes[0]
	if perShard <= 0 {
		return nil, false
	}
	// The offset math assumes uniformly sized data shards, which is what Split
	// produces. Anything else falls back rather than risking a wrong mapping.
	for i := 0; i < meta.DataShards; i++ {
		if meta.ShardSizes[i] != perShard {
			return nil, false
		}
	}
	if perShard*int64(meta.DataShards) < meta.OriginalSize {
		return nil, false
	}
	s := &shardStream{
		e: e, bucket: bucket, key: key, meta: meta,
		perShard: perShard, size: meta.OriginalSize, bufShard: -1,
		readers: make([]storage.ReadSeekCloser, meta.DataShards),
	}
	for i := 0; i < meta.DataShards; i++ {
		rc, _, err := e.backendFor(i).GetObject(bucket, meta.shardPath(key, i))
		if err != nil {
			s.Close()
			return nil, false
		}
		s.readers[i] = rc
	}
	return s, true
}

// stripeLen is the read granularity: the checksum stripe when the version has
// checksums, so every load can be verified, and the same size otherwise.
func (s *shardStream) stripeLen() int64 {
	if s.meta.hasChecksums() {
		return s.meta.CRCStripe
	}
	return crcStripeBytes
}

// load reads and verifies the stripe of data shard idx that starts at off.
func (s *shardStream) load(idx int, off int64) error {
	n := s.stripeLen()
	if rem := s.perShard - off; rem < n {
		n = rem
	}
	rc := s.readers[idx]
	if _, err := rc.Seek(off, io.SeekStart); err != nil {
		return fmt.Errorf("seek data shard %d: %w", idx, err)
	}
	if int64(cap(s.buf)) < n {
		s.buf = make([]byte, n)
	}
	s.buf = s.buf[:n]
	if _, err := io.ReadFull(rc, s.buf); err != nil {
		s.bufShard = -1
		return fmt.Errorf("read data shard %d at %d: %w", idx, off, err)
	}
	if !s.meta.stripeOK(idx, off, s.buf) {
		s.bufShard = -1
		return fmt.Errorf("data shard %d failed its checksum at %d", idx, off)
	}
	s.bufShard, s.bufOff = idx, off
	return nil
}

func (s *shardStream) Read(p []byte) (int, error) {
	if s.alt != nil {
		return s.alt.Read(p)
	}
	if s.pos >= s.size {
		return 0, io.EOF
	}

	idx := int(s.pos / s.perShard)
	inShard := s.pos % s.perShard
	stripe := s.stripeLen()
	start := inShard / stripe * stripe
	if s.bufShard != idx || s.bufOff != start {
		if err := s.load(idx, start); err != nil {
			return s.recoverAndRead(p, idx, err)
		}
	}

	// Never read past this stripe or past the object's logical end (the last
	// data shard is zero-padded). The next Read moves on to the next stripe.
	within := inShard - s.bufOff
	avail := int64(len(s.buf)) - within
	if rem := s.size - s.pos; rem < avail {
		avail = rem
	}
	if int64(len(p)) > avail {
		p = p[:avail]
	}
	n := copy(p, s.buf[within:])
	s.pos += int64(n)
	return n, nil
}

// recoverAndRead takes over after data shard bad turned out to be unusable. It
// prefers the degraded stream, which recovers a stripe at a time, with that
// shard excluded so a corrupt shard is not read again. When that cannot be
// built it reconstructs the whole object, which is what this path always did.
func (s *shardStream) recoverAndRead(p []byte, bad int, cause error) (int, error) {
	s.closeReaders()
	slog.Warn("erasure: falling back to parity reconstruction for read",
		"bucket", s.bucket, "key", s.key, "reason", cause)

	if ds, ok := s.e.newDegradedStreamExcluding(s.bucket, s.key, s.meta, bad); ok {
		s.alt = ds
	} else {
		data, err := s.e.reconstruct(s.bucket, s.key, s.meta)
		if err != nil {
			return 0, fmt.Errorf("erasure: %w (after %v)", err, cause)
		}
		if int64(len(data)) < s.size {
			return 0, fmt.Errorf("erasure: reconstructed %d bytes, expected %d", len(data), s.size)
		}
		s.alt = newBytesReadSeekCloser(data[:s.size])
	}
	if _, err := s.alt.Seek(s.pos, io.SeekStart); err != nil {
		return 0, err
	}
	return s.alt.Read(p)
}

func (s *shardStream) Seek(offset int64, whence int) (int64, error) {
	if s.alt != nil {
		return s.alt.Seek(offset, whence)
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.pos + offset
	case io.SeekEnd:
		abs = s.size + offset
	default:
		return 0, fmt.Errorf("erasure: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("erasure: negative seek position %d", abs)
	}
	// Reposition lazily: the next Read loads the right stripe. Range and
	// partNumber reads therefore cost one stripe, not a full materialization.
	s.pos = abs
	return abs, nil
}

func (s *shardStream) Close() error {
	s.closeReaders()
	if s.alt != nil {
		s.alt.Close()
		s.alt = nil
	}
	return nil
}

func (s *shardStream) closeReaders() {
	for i, rc := range s.readers {
		if rc != nil {
			rc.Close()
			s.readers[i] = nil
		}
	}
	s.bufShard = -1
}
