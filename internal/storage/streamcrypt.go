package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Streaming authenticated encryption ("VS3S" blobs).
//
// The original at-rest format sealed a whole object with one AES-GCM call. That
// is only expressible as a single buffer: encrypting meant holding the plaintext
// AND the ciphertext, and decrypting meant reading the entire ciphertext and
// allocating the entire plaintext before the first byte could be served. Peak
// memory therefore scaled with object size times concurrency, and eight
// concurrent readers of one 643 MiB object OOM-killed a node with an 8 GiB limit
// (issue #49). Compression and erasure coding had already been made streaming
// (issue #38); encryption was the last read path that had not.
//
// This format encrypts a chunk at a time, so a reader holds one chunk rather
// than one object:
//
//	header : magic[4] "VS3S" | format[1] | keyVersion[4] | chunkSize[4] | noncePrefix[7]
//	chunks : repeated seal(plaintext[0:chunkSize]) each followed by its 16-byte tag
//
// It is the STREAM construction (Rogaway et al., as used by age and Tink): the
// nonce is noncePrefix || chunkIndex || finalFlag, so chunks cannot be reordered,
// duplicated, or moved between objects, and truncating the file is caught because
// the chunk that ought to be final was not sealed as final. A plaintext whose
// length is a multiple of chunkSize is followed by an empty final chunk (a bare
// tag), so every object ends with an authenticated end-of-stream marker.
//
// Reading is chunk-at-a-time and each chunk is authenticated before any of its
// bytes are returned, so no unverified plaintext ever reaches a client.
const (
	streamMagic       = "VS3S"
	streamFormatV1    = byte(1)
	streamNoncePrefix = 7
	streamHeaderLen   = 4 + 1 + 4 + 4 + streamNoncePrefix // 20
	streamTagLen      = 16
	streamNonceLen    = 12

	// maxStreamChunk bounds what a header may ask us to allocate per reader, so a
	// corrupt or hostile header cannot turn a GET into a huge allocation.
	maxStreamChunk = 64 << 20
)

// defaultStreamChunk is the plaintext per chunk for new writes. It sets the
// per-request memory floor (a reader holds one plaintext and one ciphertext
// chunk), traded against the 16-byte tag each chunk adds on disk: at 1 MiB the
// overhead is 0.0015%. A var rather than a const so tests can shrink it and
// exercise multi-chunk objects without writing megabytes. Readers take the chunk
// size from the blob's own header, so changing this never breaks stored objects.
var defaultStreamChunk = 1 << 20

var errNotStream = errors.New("storage: not a VS3S stream")

// isStreamBlob reports whether data begins with the streaming-format magic.
func isStreamBlob(data []byte) bool {
	return len(data) >= len(streamMagic) && string(data[:len(streamMagic)]) == streamMagic
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("storage: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// streamHeader is the parsed fixed-size header of a VS3S blob.
type streamHeader struct {
	keyVersion uint32
	chunkSize  int
	prefix     [streamNoncePrefix]byte
}

// streamNonce derives the nonce for one chunk: prefix || index || final flag.
// The flag is what makes truncation detectable, so it must be part of the nonce
// rather than a convention the reader could skip.
func streamNonce(prefix [streamNoncePrefix]byte, idx uint32, final bool) []byte {
	nonce := make([]byte, streamNonceLen)
	copy(nonce, prefix[:])
	binary.BigEndian.PutUint32(nonce[streamNoncePrefix:], idx)
	if final {
		nonce[streamNonceLen-1] = 1
	}
	return nonce
}

// parseStreamHeader reads and validates a header from the front of src.
func parseStreamHeader(src io.Reader) (streamHeader, error) {
	var h streamHeader
	buf := make([]byte, streamHeaderLen)
	if _, err := io.ReadFull(src, buf); err != nil {
		return h, errNotStream
	}
	if !isStreamBlob(buf) {
		return h, errNotStream
	}
	if buf[4] != streamFormatV1 {
		return h, fmt.Errorf("storage: unsupported stream format %d", buf[4])
	}
	h.keyVersion = binary.BigEndian.Uint32(buf[5:9])
	chunk := binary.BigEndian.Uint32(buf[9:13])
	if chunk == 0 || chunk > maxStreamChunk {
		return h, fmt.Errorf("storage: invalid stream chunk size %d", chunk)
	}
	h.chunkSize = int(chunk)
	copy(h.prefix[:], buf[13:streamHeaderLen])
	return h, nil
}

// peekStreamHeader decides between the streaming and legacy formats.
//
// On success src is left positioned at the first chunk and is deliberately NOT
// rewound: an inner reader is not always cheap to seek. The decompressor
// materialises the entire object to satisfy a Seek (it has to, the codecs are
// not seekable), so rewinding here would cost, per concurrent reader, exactly
// the copy of the object this format exists to avoid. Only when the blob turns
// out not to be a stream does src rewind, for the legacy path that buffers it
// anyway.
func peekStreamHeader(src ReadSeekCloser) (streamHeader, bool) {
	h, err := parseStreamHeader(src)
	if err == nil {
		return h, true
	}
	if _, serr := src.Seek(0, io.SeekStart); serr != nil {
		return streamHeader{}, false
	}
	return streamHeader{}, false
}

// streamChunkCount is the number of sealed chunks for a plaintext of n bytes.
// There is always a final chunk, empty when n lands on a chunk boundary.
func streamChunkCount(n int64, chunkSize int) int64 {
	return n/int64(chunkSize) + 1
}

// streamCipherSize is the exact stored size of a plaintext of n bytes, so a write
// can tell the inner engine its length up front instead of streaming blind.
func streamCipherSize(n int64, chunkSize int) int64 {
	return streamHeaderLen + n + streamChunkCount(n, chunkSize)*streamTagLen
}

// streamPlainSize recovers the plaintext length and chunk count from the stored
// size. Every chunk but the last is full, so both follow from the ciphertext
// length alone: no trailer or metadata lookup needed.
//
// The result is checked back against streamCipherSize, which makes any stored
// length that the format could not have produced an error at open time. That
// check is load-bearing: a file missing its trailing empty final chunk still
// yields a self-consistent plaintext length, and without it the reader would
// simply expect one more chunk than exists, never read the truncated tail, and
// serve the short object as if it were whole.
func streamPlainSize(stored int64, chunkSize int) (plain, chunks int64, err error) {
	rem := stored - streamHeaderLen
	if rem < streamTagLen {
		return 0, 0, fmt.Errorf("storage: stream too short (%d bytes)", stored)
	}
	per := int64(chunkSize) + streamTagLen
	chunks = (rem + per - 1) / per
	plain = rem - chunks*streamTagLen
	if plain < 0 || streamCipherSize(plain, chunkSize) != stored {
		return 0, 0, fmt.Errorf("storage: stream length %d is not a whole object (truncated or corrupt)", stored)
	}
	return plain, chunks, nil
}

// sealStream encrypts everything src yields into dst in the VS3S format and
// returns the number of plaintext bytes consumed. Memory is one chunk regardless
// of object size.
func sealStream(dst io.Writer, src io.Reader, key []byte, keyVersion uint32, chunkSize int) (int64, error) {
	if chunkSize <= 0 || chunkSize > maxStreamChunk {
		chunkSize = defaultStreamChunk
	}
	gcm, err := newAEAD(key)
	if err != nil {
		return 0, err
	}

	var prefix [streamNoncePrefix]byte
	if _, err := rand.Read(prefix[:]); err != nil {
		return 0, fmt.Errorf("storage: nonce prefix: %w", err)
	}

	header := make([]byte, 0, streamHeaderLen)
	header = append(header, streamMagic...)
	header = append(header, streamFormatV1)
	header = binary.BigEndian.AppendUint32(header, keyVersion)
	header = binary.BigEndian.AppendUint32(header, uint32(chunkSize))
	header = append(header, prefix[:]...)
	if _, err := dst.Write(header); err != nil {
		return 0, err
	}

	plain := make([]byte, chunkSize)
	sealed := make([]byte, 0, chunkSize+streamTagLen)
	var total int64
	var idx uint32

	for {
		n, rerr := io.ReadFull(src, plain)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return 0, rerr
		}
		// A short or empty read means the source is done, so this chunk closes the
		// stream. A full read may still be the last one, which is why a plaintext
		// ending on a boundary gets an extra empty final chunk below.
		final := n < chunkSize
		sealed = gcm.Seal(sealed[:0], streamNonce(prefix, idx, final), plain[:n], nil)
		if _, err := dst.Write(sealed); err != nil {
			return 0, err
		}
		total += int64(n)
		idx++
		if final {
			return total, nil
		}
	}
}

// sealStreamToEngine encrypts reader into an inner engine's PutObject-shaped
// callback without either side holding the object. The sealing runs against a
// pipe, so the inner engine writes bytes to disk as they are produced.
//
// size is the plaintext length (-1 when unknown). When it is known the exact
// stored length is computed and passed down, which matters because an inner
// engine given an unknown length can fall back to buffering (the compression
// engine does exactly that when it cannot record a frame content size).
func sealStreamToEngine(key []byte, keyVersion uint32, reader io.Reader, size int64,
	put func(sealed io.Reader, storedSize int64) (int64, string, error),
) (int64, string, error) {
	pr, pw := io.Pipe()

	type sealResult struct {
		n   int64
		err error
	}
	done := make(chan sealResult, 1)
	go func() {
		n, err := sealStream(pw, reader, key, keyVersion, defaultStreamChunk)
		// Closing with the error propagates a failed read or seal to the inner
		// engine, which then abandons its partial write.
		pw.CloseWithError(err)
		done <- sealResult{n: n, err: err}
	}()

	storedSize := int64(-1)
	if size >= 0 {
		storedSize = streamCipherSize(size, defaultStreamChunk)
	}
	_, etag, putErr := put(pr, storedSize)
	pr.CloseWithError(putErr)
	res := <-done

	if res.err != nil {
		return 0, "", fmt.Errorf("encrypt: %w", res.err)
	}
	if putErr != nil {
		return 0, "", putErr
	}
	return res.n, etag, nil
}

// streamReader serves a VS3S blob as a plaintext ReadSeekCloser, decrypting one
// chunk at a time. It holds a single chunk of plaintext and a single chunk of
// ciphertext, so a concurrent reader costs a fixed ~2 MiB rather than a copy of
// the object.
type streamReader struct {
	src       ReadSeekCloser
	gcm       cipher.AEAD
	prefix    [streamNoncePrefix]byte
	chunkSize int
	plainSize int64
	chunks    int64

	buf     []byte // plaintext of the chunk currently loaded
	bufIdx  int64  // which chunk that is; -1 when nothing is loaded
	ct      []byte // scratch for the sealed chunk
	pos     int64  // absolute plaintext offset of the next byte to return
	srcAt   int64  // where src is positioned, to skip redundant seeks
	lastErr error
}

// newStreamReader builds a reader over src, which peekStreamHeader has left
// positioned at the first chunk. stored is the size of the whole ciphertext
// blob. A straight read from start to finish never seeks src, which keeps the
// cost off inner readers that seek expensively.
func newStreamReader(src ReadSeekCloser, stored int64, h streamHeader, key []byte) (*streamReader, error) {
	gcm, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	plainSize, chunks, err := streamPlainSize(stored, h.chunkSize)
	if err != nil {
		return nil, err
	}
	return &streamReader{
		src:       src,
		gcm:       gcm,
		prefix:    h.prefix,
		chunkSize: h.chunkSize,
		plainSize: plainSize,
		chunks:    chunks,
		bufIdx:    -1,
		srcAt:     streamHeaderLen, // just past the header, i.e. at chunk 0
	}, nil
}

// loadChunk decrypts chunk idx into r.buf, verifying its tag first.
func (r *streamReader) loadChunk(idx int64) error {
	if r.bufIdx == idx {
		return nil
	}
	if idx < 0 || idx >= r.chunks {
		return io.EOF
	}

	per := int64(r.chunkSize) + streamTagLen
	off := int64(streamHeaderLen) + idx*per

	// Every chunk but the last is full; the last is whatever remains.
	want := per
	if idx == r.chunks-1 {
		want = r.plainSize - idx*int64(r.chunkSize) + streamTagLen
	}

	if r.srcAt != off {
		if _, err := r.src.Seek(off, io.SeekStart); err != nil {
			return err
		}
		r.srcAt = off
	}
	if int64(cap(r.ct)) < want {
		r.ct = make([]byte, want)
	}
	r.ct = r.ct[:want]
	if _, err := io.ReadFull(r.src, r.ct); err != nil {
		r.srcAt = -1
		return err
	}
	r.srcAt = off + want

	if int64(cap(r.buf)) < want {
		r.buf = make([]byte, 0, want)
	}
	// Open verifies the tag before it writes any plaintext, so a corrupt or
	// truncated chunk fails here rather than reaching the client.
	plain, err := r.gcm.Open(r.buf[:0], streamNonce(r.prefix, uint32(idx), idx == r.chunks-1), r.ct, nil)
	if err != nil {
		return fmt.Errorf("storage: chunk %d failed authentication: %w", idx, err)
	}
	r.buf = plain
	r.bufIdx = idx
	return nil
}

func (r *streamReader) Read(p []byte) (int, error) {
	if r.lastErr != nil {
		return 0, r.lastErr
	}
	if r.pos >= r.plainSize {
		return 0, io.EOF
	}
	idx := r.pos / int64(r.chunkSize)
	if err := r.loadChunk(idx); err != nil {
		r.lastErr = err
		return 0, err
	}
	within := int(r.pos - idx*int64(r.chunkSize))
	n := copy(p, r.buf[within:])
	r.pos += int64(n)
	return n, nil
}

func (r *streamReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.plainSize + offset
	default:
		return 0, fmt.Errorf("storage: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("storage: negative seek position %d", abs)
	}
	r.pos = abs
	r.lastErr = nil
	return abs, nil
}

func (r *streamReader) Close() error { return r.src.Close() }

// Size is the plaintext length, which callers need for Content-Length.
func (r *streamReader) Size() int64 { return r.plainSize }

// openLegacyWhole decrypts a blob written by the original whole-object format.
//
// It cannot stream: one GCM tag covers the entire object, so nothing can be
// released before all of it has been read and verified. What it can avoid is
// making several copies of it. Reading into one exact-size buffer skips
// io.ReadAll's grow-and-copy, and opening in place reuses that same buffer for
// the plaintext, which took the cost of a read from roughly 3x the object size
// down to 1x. Objects rewritten (or written after this change) use the streaming
// format above and cost a chunk instead.
func openLegacyWhole(src io.Reader, stored int64, gcm cipher.AEAD) ([]byte, error) {
	if stored < 0 || stored > maxEncryptedSize+int64(gcm.NonceSize())+streamTagLen {
		return nil, fmt.Errorf("storage: encrypted object too large (%d bytes)", stored)
	}
	buf := make([]byte, stored)
	if _, err := io.ReadFull(src, buf); err != nil {
		return nil, fmt.Errorf("read encrypted data: %w", err)
	}
	ns := gcm.NonceSize()
	if len(buf) < ns {
		return nil, errors.New("encrypted data too short")
	}
	nonce := buf[:ns]
	ct := buf[ns:]
	// dst and src start at the same address (exact overlap), which crypto/cipher
	// permits, and Open checks the tag before writing, so this cannot emit
	// unauthenticated bytes.
	return gcm.Open(ct[:0], nonce, ct, nil)
}

// openSealed prepares a stored blob for decryption, and is the one place every
// encryption engine goes through on the way in.
//
// Until VaultS3 4.4.70 the compressor wrapped the encryptor, so an object
// written with both enabled is compress(encrypt(plaintext)) and its bytes start
// with a zstd or gzip magic rather than the VS3S stream header. That layering
// saved nothing, because ciphertext does not compress, which is why the order is
// now the other way round. Those objects still have to read, so the outer
// compression is unwrapped here before the blob reaches the decryptor.
//
// A blob that is not compressed is streamed through untouched, so this costs a
// four-byte peek on the ordinary path.
func openSealed(reader ReadSeekCloser, stored int64) (ReadSeekCloser, int64, error) {
	return decompressIfCompressed(reader, stored)
}
