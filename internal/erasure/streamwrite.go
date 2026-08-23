package erasure

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// parityStripeBytes is how much of each shard is held while parity is generated.
// Reed-Solomon is column-wise, so parity at a given offset depends only on the
// data shards at that same offset, which is what lets this run on aligned slices
// instead of whole shards.
const parityStripeBytes = 1 << 20

// putObjectStreaming writes an erasure-coded object without ever holding it in
// memory, which is what the buffering path did: io.ReadAll of the whole object,
// then Split and Encode on top of it, so a single PUT allocated roughly the
// object plus its parity, and concurrent PUTs multiplied that. Large uploads at
// concurrency were reported as OOM kills.
//
// The on-disk layout is unchanged, which matters because the read path depends
// on it: Split writes the original bytes into equal-sized contiguous data shards
// (the last zero-padded), so plaintext offset o lives in data shard o/perShard.
// Preserving that means shardStream, the degraded reader and the healer need no
// changes and existing objects stay readable.
//
// It takes two passes because parity at offset o needs every data shard at
// offset o, and in a sequential stream those bytes arrive far apart: shard 0's
// byte o near the start, shard k-1's byte o near the end. So pass one streams the
// plaintext into the data shards, and pass two reads them back in stripes to
// generate parity. The re-read is normally served from page cache.
func (e *Engine) putObjectStreaming(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	k := e.cfg.DataShards
	m := e.cfg.ParityShards
	perShard := (size + int64(k) - 1) / int64(k)

	hasher := md5.New()
	// Tee the source so every real byte is hashed exactly once. Padding is
	// written to the last shard but must not reach the hash: the ETag covers the
	// object the client sent, not the padded shard layout.
	src := io.TeeReader(reader, hasher)

	// Pass 1: plaintext straight into the data shards.
	remaining := size
	for i := 0; i < k; i++ {
		n := perShard
		if remaining < n {
			n = remaining
		}
		if n < 0 {
			n = 0
		}
		body := io.Reader(io.LimitReader(src, n))
		if pad := perShard - n; pad > 0 {
			body = io.MultiReader(body, zeroReader(pad))
		}
		if _, _, err := e.backendFor(i).PutObject(bucket, shardKey(key, i), body, perShard); err != nil {
			return 0, "", fmt.Errorf("store data shard %d: %w", i, err)
		}
		remaining -= n
	}
	if remaining > 0 {
		return 0, "", fmt.Errorf("erasure: source ended %d bytes short of the declared %d", remaining, size)
	}

	// Pass 2: parity, one shard at a time, generated on demand from the data
	// shards rather than from a copy of the object.
	for j := 0; j < m; j++ {
		pr, err := e.newParityReader(bucket, key, perShard, j)
		if err != nil {
			return 0, "", err
		}
		_, _, err = e.backendFor(k+j).PutObject(bucket, shardKey(key, k+j), pr, perShard)
		pr.Close()
		if err != nil {
			return 0, "", fmt.Errorf("store parity shard %d: %w", j, err)
		}
	}

	etag := hex.EncodeToString(hasher.Sum(nil))
	meta := &ShardMeta{
		OriginalSize: size,
		DataShards:   k,
		ParityShards: m,
		BlockSize:    e.cfg.BlockSize,
		ShardSizes:   make([]int64, k+m),
		ETag:         etag,
		CreatedAt:    time.Now().UTC(),
	}
	for i := range meta.ShardSizes {
		meta.ShardSizes[i] = perShard
	}
	metaBytes, err := meta.Marshal()
	if err != nil {
		return 0, "", fmt.Errorf("marshal shard meta: %w", err)
	}
	if _, _, err := e.backendFor(0).PutObject(bucket, metaKey(key), byteReader(metaBytes), int64(len(metaBytes))); err != nil {
		return 0, "", fmt.Errorf("store shard meta: %w", err)
	}
	return size, etag, nil
}

// parityReader produces one parity shard on demand by encoding the data shards a
// stripe at a time, so generating parity costs a stripe of memory rather than a
// copy of the object.
type parityReader struct {
	enc     *Encoder
	readers []storage.ReadSeekCloser // data shards, index 0..k-1
	index   int                      // which parity shard this produces, 0-based
	total   int64                    // perShard, the length of every shard
	pos     int64
	buf     []byte
	bufOff  int
	blocks  [][]byte // reused stripe buffers
}

func (e *Engine) newParityReader(bucket, key string, perShard int64, index int) (*parityReader, error) {
	k := e.cfg.DataShards
	enc, err := NewEncoder(k, e.cfg.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("create encoder: %w", err)
	}
	p := &parityReader{
		enc: enc, index: index, total: perShard,
		readers: make([]storage.ReadSeekCloser, k),
		blocks:  make([][]byte, k+e.cfg.ParityShards),
	}
	for i := 0; i < k; i++ {
		rc, _, err := e.backendFor(i).GetObject(bucket, shardKey(key, i))
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("reopen data shard %d for parity: %w", i, err)
		}
		p.readers[i] = rc
	}
	return p, nil
}

func (p *parityReader) fill() error {
	length := int(parityStripeBytes)
	if rem := p.total - p.pos; rem < int64(length) {
		length = int(rem)
	}
	if length <= 0 {
		return io.EOF
	}
	for i, rc := range p.readers {
		if cap(p.blocks[i]) < length {
			p.blocks[i] = make([]byte, length)
		}
		p.blocks[i] = p.blocks[i][:length]
		if _, err := io.ReadFull(rc, p.blocks[i]); err != nil {
			return fmt.Errorf("read data shard %d at %d: %w", i, p.pos, err)
		}
	}
	for j := len(p.readers); j < len(p.blocks); j++ {
		if cap(p.blocks[j]) < length {
			p.blocks[j] = make([]byte, length)
		}
		p.blocks[j] = p.blocks[j][:length]
	}
	if err := p.enc.EncodeStripe(p.blocks); err != nil {
		return fmt.Errorf("encode parity stripe at %d: %w", p.pos, err)
	}
	p.buf = p.blocks[len(p.readers)+p.index]
	p.bufOff = 0
	p.pos += int64(length)
	return nil
}

func (p *parityReader) Read(b []byte) (int, error) {
	if p.bufOff >= len(p.buf) {
		if p.pos >= p.total {
			return 0, io.EOF
		}
		if err := p.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(b, p.buf[p.bufOff:])
	p.bufOff += n
	return n, nil
}

func (p *parityReader) Close() error {
	for i, rc := range p.readers {
		if rc != nil {
			rc.Close()
			p.readers[i] = nil
		}
	}
	return nil
}

// zeroReader yields n zero bytes, used to pad the final data shard.
func zeroReader(n int64) io.Reader {
	return io.LimitReader(zeroSource{}, n)
}

type zeroSource struct{}

func (zeroSource) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 0
	}
	return len(b), nil
}

func byteReader(b []byte) io.Reader { return io.NewSectionReader(bytesReaderAt(b), 0, int64(len(b))) }

type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
