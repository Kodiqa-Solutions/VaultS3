package erasure

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// degradedStripeBytes is how much of each shard is read and recovered per pass.
// Reed-Solomon works column-wise: byte j of a missing shard depends only on byte
// j of the others, so recovery can run on aligned slices instead of whole shards.
// 1 MiB keeps the read amplification of touching every shard reasonable while
// bounding the memory a degraded read holds to stripe x total shards.
const degradedStripeBytes = 1 << 20

// degradedStream serves an erasure-coded object that is missing a data shard by
// recovering one stripe at a time, so first-byte latency stays flat instead of
// growing with object size (issue #38).
//
// The healthy path (shardStream) never needs parity, but when a data shard is gone
// the old path called reconstruct(), which read every shard in full, decoded the
// whole object and only then emitted byte one. That put the entire object on the
// heap and made TTFB proportional to size, which is the exact behaviour #38 is
// about. Measured on a 64 MiB object with one data shard removed, first-byte went
// from single-digit ms to tens of ms locally, and scales with slower disks.
//
// Correctness is unchanged: the same encoder recovers the same bytes, just in
// aligned slices rather than one pass over the whole object.
type degradedStream struct {
	e      *Engine
	bucket string
	key    string
	meta   *ShardMeta
	enc    *Encoder

	perShard int64 // size of every shard file (data and parity are equal sized)
	size     int64 // OriginalSize: logical length of the object
	pos      int64 // current logical read offset

	readers []storage.ReadSeekCloser // per shard index, nil when absent or unreadable

	// The recovered stripe, held as one block per shard. stripeOff is its offset
	// within each shard, so it is valid for logical offsets whose in-shard offset
	// falls in [stripeOff, stripeOff+stripeLen).
	stripe    [][]byte
	stripeOff int64
	stripeLen int

	// bufs holds one reusable read buffer per shard so a long sequential read does
	// not allocate a fresh stripe on every fill. Reconstruct allocates the missing
	// shards itself, which is why only present shards are reused.
	bufs [][]byte
}

// newDegradedStream opens what shards it can and returns a streaming reader when
// enough survive to recover the object. It returns false when the layout is not
// the uniform one Split produces, or when too many shards are gone, leaving the
// caller to fall back to the buffering path (which will report the real error).
func (e *Engine) newDegradedStream(bucket, key string, meta *ShardMeta) (*degradedStream, bool) {
	total := meta.DataShards + meta.ParityShards
	if meta.DataShards <= 0 || meta.ParityShards < 0 || len(meta.ShardSizes) < total {
		return nil, false
	}
	perShard := meta.ShardSizes[0]
	if perShard <= 0 {
		return nil, false
	}
	// Column-wise recovery requires every shard to be the same length, which is
	// what Split produces. Anything else falls back rather than mis-mapping bytes.
	for i := 0; i < total; i++ {
		if meta.ShardSizes[i] != perShard {
			return nil, false
		}
	}
	if perShard*int64(meta.DataShards) < meta.OriginalSize {
		return nil, false
	}

	enc, err := NewEncoder(meta.DataShards, meta.ParityShards)
	if err != nil {
		return nil, false
	}

	s := &degradedStream{
		e: e, bucket: bucket, key: key, meta: meta, enc: enc,
		perShard: perShard, size: meta.OriginalSize,
		readers:   make([]storage.ReadSeekCloser, total),
		bufs:      make([][]byte, total),
		stripeOff: -1,
	}

	present := 0
	for i := 0; i < total; i++ {
		rc, _, err := e.backendFor(i).GetObject(bucket, shardKey(key, i))
		if err != nil {
			continue
		}
		s.readers[i] = rc
		present++
	}
	if present < meta.DataShards {
		s.Close()
		return nil, false
	}

	slog.Warn("erasure: streaming a degraded read from parity",
		"bucket", bucket, "key", key,
		"present", present, "needed", meta.DataShards, "total", total)
	return s, true
}

// fill recovers the stripe containing the given in-shard offset.
func (s *degradedStream) fill(inShard int64) error {
	off := (inShard / degradedStripeBytes) * degradedStripeBytes
	length := int(degradedStripeBytes)
	if rem := s.perShard - off; rem < int64(length) {
		length = int(rem)
	}
	if length <= 0 {
		return io.EOF
	}

	// Recovery needs exactly DataShards blocks, so stop there instead of reading
	// every surviving shard: reading the extras would cost I/O that changes
	// nothing. Data shards come first so the decoder has less to rebuild.
	blocks := make([][]byte, len(s.readers))
	present := 0
	for _, i := range s.readOrder() {
		if present >= s.meta.DataShards {
			break
		}
		rc := s.readers[i]
		if rc == nil {
			continue
		}
		if _, err := rc.Seek(off, io.SeekStart); err != nil {
			s.readers[i] = nil
			rc.Close()
			continue
		}
		if cap(s.bufs[i]) < length {
			s.bufs[i] = make([]byte, length)
		}
		buf := s.bufs[i][:length]
		if _, err := io.ReadFull(rc, buf); err != nil {
			// A shard that is present but short or unreadable is treated as missing,
			// exactly as the buffering path did, so parity covers it.
			s.readers[i] = nil
			rc.Close()
			continue
		}
		blocks[i] = buf
		present++
	}
	if present < s.meta.DataShards {
		return fmt.Errorf("erasure: %d shards readable at offset %d, need %d", present, off, s.meta.DataShards)
	}

	if err := s.enc.Reconstruct(blocks); err != nil {
		return fmt.Errorf("erasure: reconstruct stripe at %d: %w", off, err)
	}

	s.stripe, s.stripeOff, s.stripeLen = blocks, off, length
	return nil
}

func (s *degradedStream) Read(p []byte) (int, error) {
	if s.pos >= s.size {
		return 0, io.EOF
	}
	idx := int(s.pos / s.perShard)
	inShard := s.pos % s.perShard
	if idx >= s.meta.DataShards {
		return 0, io.EOF
	}

	if s.stripe == nil || inShard < s.stripeOff || inShard >= s.stripeOff+int64(s.stripeLen) {
		if err := s.fill(inShard); err != nil {
			return 0, err
		}
	}

	blockPos := inShard - s.stripeOff
	avail := int64(s.stripeLen) - blockPos
	// Never cross into the next data shard (its bytes are a later logical range)
	// or past the object's logical end (the last data shard is zero-padded).
	if rem := s.perShard - inShard; rem < avail {
		avail = rem
	}
	if rem := s.size - s.pos; rem < avail {
		avail = rem
	}
	if int64(len(p)) > avail {
		p = p[:avail]
	}

	n := copy(p, s.stripe[idx][blockPos:blockPos+int64(len(p))])
	s.pos += int64(n)
	return n, nil
}

func (s *degradedStream) Seek(offset int64, whence int) (int64, error) {
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
	s.pos = abs
	return abs, nil
}

func (s *degradedStream) Close() error {
	for i, rc := range s.readers {
		if rc != nil {
			rc.Close()
			s.readers[i] = nil
		}
	}
	s.stripe = nil
	return nil
}

// readOrder lists shard indexes data-shards-first, so a stripe is assembled from
// the shards that need no reconstruction wherever possible.
func (s *degradedStream) readOrder() []int {
	order := make([]int, 0, len(s.readers))
	for i := 0; i < s.meta.DataShards && i < len(s.readers); i++ {
		order = append(order, i)
	}
	for i := s.meta.DataShards; i < len(s.readers); i++ {
		order = append(order, i)
	}
	return order
}
