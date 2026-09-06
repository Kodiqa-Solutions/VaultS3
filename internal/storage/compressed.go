package storage

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// zstdEncoder is reused across objects — EncodeAll is safe for concurrent use.
var zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))

// zstdWriters pools streaming encoders. Unlike EncodeAll, a streaming writer is
// stateful and cannot be shared, and building one per object allocates its whole
// compression window — exactly the per-request cost the streaming path exists to
// avoid.
var zstdWriters = sync.Pool{
	New: func() any {
		w, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		return w
	},
}

func acquireZstdWriter() *zstd.Encoder { return zstdWriters.Get().(*zstd.Encoder) }

// releaseZstdWriter returns an encoder to the pool with its reference to the
// previous destination dropped, so a finished upload's pipe is not pinned alive.
func releaseZstdWriter(e *zstd.Encoder) {
	e.Reset(nil)
	zstdWriters.Put(e)
}

// excludedExtensions lists file extensions that should NOT be compressed
// because they are already compressed or would not benefit from compression.
var excludedExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".gz": true, ".tgz": true, ".bz2": true, ".xz": true, ".zst": true, ".lz4": true,
	".zip": true, ".rar": true, ".7z": true, ".tar.gz": true,
	".mp4": true, ".mkv": true, ".avi": true, ".mov": true, ".webm": true,
	".mp3": true, ".flac": true, ".ogg": true, ".aac": true,
	".woff": true, ".woff2": true,
}

// CompressedEngine wraps another Engine and compresses/decompresses data transparently.
// New objects are compressed with zstd (better ratio and speed than gzip); objects
// written by older versions with gzip are still read transparently (the codec is
// detected by magic number on read). Files with already-compressed extensions are
// passed through without compression.
type CompressedEngine struct {
	inner         Engine
	ExcludedTypes map[string]bool // additional excluded extensions
}

func NewCompressedEngine(inner Engine) *CompressedEngine {
	return &CompressedEngine{inner: inner}
}

// shouldCompress returns true if the key should be compressed.
func (c *CompressedEngine) shouldCompress(key string) bool {
	ext := strings.ToLower(filepath.Ext(key))
	if excludedExtensions[ext] {
		return false
	}
	if c.ExcludedTypes != nil && c.ExcludedTypes[ext] {
		return false
	}
	return true
}

func (c *CompressedEngine) CreateBucketDir(bucket string) error {
	return c.inner.CreateBucketDir(bucket)
}

func (c *CompressedEngine) DeleteBucketDir(bucket string) error {
	return c.inner.DeleteBucketDir(bucket)
}

func (c *CompressedEngine) PutObject(bucket, key string, reader io.Reader, size int64) (int64, string, error) {
	if IsDirMarker(key) || !c.shouldCompress(key) {
		return c.inner.PutObject(bucket, key, reader, size)
	}
	return c.compressAndPut(reader, size, func(compressed io.Reader, compressedSize int64) (int64, string, error) {
		return c.inner.PutObject(bucket, key, compressed, compressedSize)
	})
}

func (c *CompressedEngine) GetObject(bucket, key string) (ReadSeekCloser, int64, error) {
	if IsDirMarker(key) || !c.shouldCompress(key) {
		return c.inner.GetObject(bucket, key)
	}
	return c.getAndDecompress(func() (ReadSeekCloser, int64, error) {
		return c.inner.GetObject(bucket, key)
	})
}

func (c *CompressedEngine) DeleteObject(bucket, key string) error {
	return c.inner.DeleteObject(bucket, key)
}

func (c *CompressedEngine) ObjectExists(bucket, key string) bool {
	return c.inner.ObjectExists(bucket, key)
}

func (c *CompressedEngine) ObjectSize(bucket, key string) (int64, error) {
	return c.inner.ObjectSize(bucket, key)
}

func (c *CompressedEngine) ListObjects(bucket, prefix, startAfter string, maxKeys int) ([]ObjectInfo, bool, error) {
	return c.inner.ListObjects(bucket, prefix, startAfter, maxKeys)
}

func (c *CompressedEngine) BucketSize(bucket string) (int64, int64, error) {
	return c.inner.BucketSize(bucket)
}

func (c *CompressedEngine) PutObjectVersion(bucket, key, versionID string, reader io.Reader, size int64) (int64, string, error) {
	if !c.shouldCompress(key) {
		return c.inner.PutObjectVersion(bucket, key, versionID, reader, size)
	}
	return c.compressAndPut(reader, size, func(compressed io.Reader, compressedSize int64) (int64, string, error) {
		return c.inner.PutObjectVersion(bucket, key, versionID, compressed, compressedSize)
	})
}

func (c *CompressedEngine) GetObjectVersion(bucket, key, versionID string) (ReadSeekCloser, int64, error) {
	if !c.shouldCompress(key) {
		return c.inner.GetObjectVersion(bucket, key, versionID)
	}
	return c.getAndDecompress(func() (ReadSeekCloser, int64, error) {
		return c.inner.GetObjectVersion(bucket, key, versionID)
	})
}

func (c *CompressedEngine) DeleteObjectVersion(bucket, key, versionID string) error {
	return c.inner.DeleteObjectVersion(bucket, key, versionID)
}

func (c *CompressedEngine) DataDir() string {
	return c.inner.DataDir()
}

func (c *CompressedEngine) ObjectPath(bucket, key string) string {
	return c.inner.ObjectPath(bucket, key)
}

// maxCompressedSize is the maximum object size for in-memory compression (1GB).
const maxCompressedSize int64 = 1 * 1024 * 1024 * 1024

// compressAndPut reads all data, compresses it, computes ETag of original, writes compressed.
func (c *CompressedEngine) compressAndPut(reader io.Reader, size int64, putFn func(io.Reader, int64) (int64, string, error)) (int64, string, error) {
	if size >= 0 && size <= maxCompressedSize {
		return c.streamCompressAndPut(reader, size, putFn)
	}
	return c.bufferCompressAndPut(reader, putFn)
}

// streamCompressAndPut compresses as the object flows through, so a large upload
// costs a window rather than two or three full copies of itself. Buffering here
// (plaintext + compressed + the handler's own copy) was a large part of the peak
// memory that OOM-killed nodes under concurrent 64 MiB uploads (issue #46).
//
// size must be the real plaintext length: it is written into the zstd frame
// header as the content size, which is what lets reads stream instead of
// materialising the whole object to learn how big it is (issue #38). Dropping it
// would quietly undo that fix, so an unknown length uses the buffered path.
func (c *CompressedEngine) streamCompressAndPut(reader io.Reader, size int64, putFn func(io.Reader, int64) (int64, string, error)) (int64, string, error) {
	h := md5.New()
	pr, pw := io.Pipe()

	type encResult struct {
		n   int64
		err error
	}
	done := make(chan encResult, 1)

	go func() {
		enc := acquireZstdWriter()
		// ResetContentSize records the frame content size the reader relies on.
		enc.ResetContentSize(pw, size)
		n, err := io.Copy(enc, io.TeeReader(reader, h))
		if cerr := enc.Close(); err == nil {
			err = cerr
		}
		releaseZstdWriter(enc)
		// Closing the pipe with the error propagates a failed read or encode to
		// the inner engine, which then abandons its partial write.
		pw.CloseWithError(err)
		done <- encResult{n: n, err: err}
	}()

	// The compressed length is not known ahead of the stream; the inner engine
	// counts what it writes, and no engine requires the size up front.
	_, _, putErr := putFn(pr, -1)
	pr.CloseWithError(putErr)
	res := <-done

	if res.err != nil {
		return 0, "", fmt.Errorf("compress: %w", res.err)
	}
	if putErr != nil {
		return 0, "", putErr
	}
	if res.n > maxCompressedSize {
		return 0, "", fmt.Errorf("object too large for compression (max %dMB)", maxCompressedSize/(1024*1024))
	}
	return res.n, fmt.Sprintf("\"%x\"", h.Sum(nil)), nil
}

// bufferCompressAndPut is the fallback for an upload whose length is not known
// in advance, where the frame content size can only be learned by reading it all.
func (c *CompressedEngine) bufferCompressAndPut(reader io.Reader, putFn func(io.Reader, int64) (int64, string, error)) (int64, string, error) {
	plaintext, err := io.ReadAll(io.LimitReader(reader, maxCompressedSize+1))
	if err != nil {
		return 0, "", fmt.Errorf("read plaintext: %w", err)
	}
	if int64(len(plaintext)) > maxCompressedSize {
		return 0, "", fmt.Errorf("object too large for compression (max %dMB)", maxCompressedSize/(1024*1024))
	}

	// Compute ETag of original data
	h := md5.Sum(plaintext)
	etag := fmt.Sprintf("\"%x\"", h)

	// Compress with zstd. EncodeAll on the shared encoder is concurrent-safe and
	// avoids per-object allocations.
	compressed := zstdEncoder.EncodeAll(plaintext, nil)

	if _, _, err = putFn(bytes.NewReader(compressed), int64(len(compressed))); err != nil {
		return 0, "", err
	}

	// Return original plaintext size and ETag
	return int64(len(plaintext)), etag, nil
}

// getAndDecompress reads compressed data from inner engine, decompresses it.
// getAndDecompress returns the object's plaintext as a STREAMING reader whose
// time-to-first-byte does not depend on object size (issue #38). zstd and gzip are
// both streaming codecs, and both record the decompressed size in the container (zstd
// frame header, gzip trailing ISIZE), so we can report Content-Length without first
// materializing the object. Only Range/partNumber reads (which Seek) fall back to
// buffering, since the codecs are not seekable. If the stored blob matches neither
// magic (written while compression was off) it is streamed through untouched, and if
// the size cannot be read cheaply we fall back to the old buffered decode.
func (c *CompressedEngine) getAndDecompress(getFn func() (ReadSeekCloser, int64, error)) (ReadSeekCloser, int64, error) {
	src, storedSize, err := getFn()
	if err != nil {
		return nil, 0, err
	}
	return decompressIfCompressed(src, storedSize)
}

// decompressIfCompressed decodes a stored blob when it carries a zstd or gzip
// magic and streams it through untouched when it does not. Split out of the
// engine method so the encryption layer can use it to unwrap the legacy
// compress-outside-encrypt layering (see openSealed).
func decompressIfCompressed(src ReadSeekCloser, storedSize int64) (ReadSeekCloser, int64, error) {

	magic := make([]byte, 4)
	n, _ := io.ReadFull(src, magic)
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		// Source is not seekable — cannot peek/stream, use the buffered path.
		return bufferedDecompress(src)
	}

	switch {
	case n >= 4 && magic[0] == 0x28 && magic[1] == 0xB5 && magic[2] == 0x2F && magic[3] == 0xFD:
		size, ok := zstdContentSize(src)
		if !ok || size > maxCompressedSize {
			return bufferedDecompress(src)
		}
		return &decompressStream{src: src, size: size, newDec: func(r io.Reader) (io.ReadCloser, error) {
			d, err := zstd.NewReader(r)
			if err != nil {
				return nil, err
			}
			return zstdReadCloser{d}, nil
		}}, size, nil
	case n >= 2 && magic[0] == 0x1F && magic[1] == 0x8B:
		size, ok := gzipISize(src)
		if !ok || size > maxCompressedSize {
			return bufferedDecompress(src)
		}
		return &decompressStream{src: src, size: size, newDec: func(r io.Reader) (io.ReadCloser, error) {
			return gzip.NewReader(r)
		}}, size, nil
	default:
		// Not a compressed blob (e.g. written while compression was disabled) — the
		// inner reader already streams the plaintext.
		return src, storedSize, nil
	}
}

// bufferedDecompress is the fallback path: read the whole blob, decompress in memory,
// serve from a bytes reader. Used when the source is not seekable or the decompressed
// size cannot be read from the header.
func bufferedDecompress(src ReadSeekCloser) (ReadSeekCloser, int64, error) {
	defer src.Close()
	compressed, err := io.ReadAll(io.LimitReader(src, maxCompressedSize+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read compressed data: %w", err)
	}
	plaintext, err := decompressBlock(compressed)
	if err != nil {
		return nil, 0, fmt.Errorf("decompress: %w", err)
	}
	if int64(len(plaintext)) > maxCompressedSize {
		return nil, 0, fmt.Errorf("decompressed data exceeds size limit")
	}
	return &bytesReadSeekCloser{Reader: bytes.NewReader(plaintext)}, int64(len(plaintext)), nil
}

// zstdContentSize reads the frame content size from a zstd frame header without
// decompressing, then rewinds src to the start. EncodeAll (used on write) always
// records it. Returns false if the header lacks it.
func zstdContentSize(src ReadSeekCloser) (int64, bool) {
	buf := make([]byte, zstd.HeaderMaxSize)
	n, _ := io.ReadFull(src, buf)
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, false
	}
	var h zstd.Header
	if err := h.Decode(buf[:n]); err != nil || !h.HasFCS {
		return 0, false
	}
	return int64(h.FrameContentSize), true
}

// gzipISize reads the uncompressed size from the gzip trailer (ISIZE, the last 4
// bytes, little-endian), then rewinds src. ISIZE is the size modulo 2^32, which is
// exact here because objects are capped at maxCompressedSize (1 GiB).
func gzipISize(src ReadSeekCloser) (int64, bool) {
	if _, err := src.Seek(-4, io.SeekEnd); err != nil {
		return 0, false
	}
	var tail [4]byte
	if _, err := io.ReadFull(src, tail[:]); err != nil {
		src.Seek(0, io.SeekStart)
		return 0, false
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint32(tail[:])), true
}

// zstdReadCloser adapts *zstd.Decoder (whose Close returns nothing) to io.ReadCloser.
type zstdReadCloser struct{ *zstd.Decoder }

func (z zstdReadCloser) Close() error { z.Decoder.Close(); return nil }

// decompressStream streams decompression so GET time-to-first-byte is independent of
// object size (issue #38). Read pulls from a streaming decoder over the compressed
// source. Seek (Range/partNumber only) materializes once, since the codecs are not
// seekable.
type decompressStream struct {
	src    ReadSeekCloser
	newDec func(io.Reader) (io.ReadCloser, error)
	dec    io.ReadCloser
	buf    *bytes.Reader
	size   int64
}

func (d *decompressStream) Read(p []byte) (int, error) {
	if d.buf != nil {
		return d.buf.Read(p)
	}
	if d.dec == nil {
		if _, err := d.src.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		dec, err := d.newDec(d.src)
		if err != nil {
			return 0, err
		}
		d.dec = dec
	}
	return d.dec.Read(p)
}

func (d *decompressStream) Seek(offset int64, whence int) (int64, error) {
	if d.buf == nil {
		if d.dec != nil {
			d.dec.Close()
			d.dec = nil
		}
		if _, err := d.src.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		dec, err := d.newDec(d.src)
		if err != nil {
			return 0, err
		}
		data, err := io.ReadAll(io.LimitReader(dec, maxCompressedSize+1))
		dec.Close()
		if err != nil {
			return 0, err
		}
		d.buf = bytes.NewReader(data)
	}
	return d.buf.Seek(offset, whence)
}

func (d *decompressStream) Close() error {
	if d.dec != nil {
		d.dec.Close()
	}
	return d.src.Close()
}

// decompressBlock decompresses a stored object, detecting the codec by magic
// number so both new (zstd) and legacy (gzip) objects read correctly. Data that
// matches neither magic (e.g. written while compression was disabled) is returned
// unchanged. The LimitReader caps output to guard against decompression bombs.
func decompressBlock(data []byte) ([]byte, error) {
	switch {
	case len(data) >= 4 && data[0] == 0x28 && data[1] == 0xB5 && data[2] == 0x2F && data[3] == 0xFD:
		dec, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("zstd reader: %w", err)
		}
		defer dec.Close()
		return io.ReadAll(io.LimitReader(dec, maxCompressedSize+1))
	case len(data) >= 2 && data[0] == 0x1F && data[1] == 0x8B:
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
		return io.ReadAll(io.LimitReader(gz, maxCompressedSize+1))
	default:
		return data, nil
	}
}
