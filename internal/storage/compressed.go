package storage

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// zstdEncoder is reused across objects — EncodeAll is safe for concurrent use.
var zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))

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

// maxCompressedSize is the maximum plaintext size accepted for compression (1GB).
const maxCompressedSize int64 = 1 * 1024 * 1024 * 1024

// compressAndPut compresses reader as it flows through, computes the ETag of the
// plaintext, and writes the compressed blob to putFn.
//
// Compressing as the object flows through means a large upload costs a frame
// rather than two or three full copies of itself. Buffering here (plaintext +
// compressed + the handler's own copy) was a large part of the peak memory that
// OOM-killed nodes under concurrent 64 MiB uploads (issue #46).
//
// The blob is written in the seekable zstd format (see seekzstd.go), whose seek
// table records every frame's decompressed size. Reads take the object length
// from the table, so, unlike the single-frame format this replaces, the length
// no longer has to be known before the first byte is written and an upload of
// unknown length (chunked, size -1) streams exactly like one with a
// Content-Length. The size cap is enforced as the plaintext is counted, so an
// oversized upload fails partway through and the inner engine abandons its
// partial write instead of a gigabyte being buffered first.
func (c *CompressedEngine) compressAndPut(reader io.Reader, size int64, putFn func(io.Reader, int64) (int64, string, error)) (int64, string, error) {
	if size > maxCompressedSize {
		return 0, "", fmt.Errorf("object too large for compression (max %dMB)", maxCompressedSize/(1024*1024))
	}
	h := md5.New()
	pr, pw := io.Pipe()

	type encResult struct {
		n   int64
		err error
	}
	done := make(chan encResult, 1)

	go func() {
		limited := io.LimitReader(io.TeeReader(reader, h), maxCompressedSize+1)
		n, err := writeSeekableZstd(pw, limited, defaultCompressFrame)
		if err == nil && n > maxCompressedSize {
			err = fmt.Errorf("object too large for compression (max %dMB)", maxCompressedSize/(1024*1024))
		}
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
	return res.n, fmt.Sprintf("\"%x\"", h.Sum(nil)), nil
}

// getAndDecompress returns the object's plaintext as a STREAMING reader whose
// time-to-first-byte does not depend on object size (issue #38) and whose memory
// does not depend on it either. See decompressIfCompressed for the formats.
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
//
// Three compressed layouts are on disk and each gets the cheapest reader it
// allows:
//
//   - Seekable zstd (new writes): the blob starts with the zstd magic and ends
//     with the seekable footer magic. The seek table gives the plaintext size
//     and a Range read decompresses one frame. Memory is O(frame).
//   - Single-frame zstd (written before the seekable format) and gzip (written
//     before zstd): both stream from the front, with the size read from the
//     container (zstd frame header, gzip trailing ISIZE). Neither codec can be
//     entered in the middle, so a Seek restarts the decoder and discards up to
//     the offset. That is CPU proportional to the offset but memory O(decoder
//     window), where it used to materialise the whole object per reader.
//   - No magic (written while compression was off): streamed through untouched.
//
// Only a source that cannot seek at all still decodes into memory.
func decompressIfCompressed(src ReadSeekCloser, storedSize int64) (ReadSeekCloser, int64, error) {
	magic := make([]byte, 4)
	n, _ := io.ReadFull(src, magic)
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		// Source is not seekable — cannot peek/stream, use the buffered path.
		return bufferedDecompress(src)
	}

	switch {
	case n >= 4 && magic[0] == 0x28 && magic[1] == 0xB5 && magic[2] == 0x2F && magic[3] == 0xFD:
		table, err := readSeekTable(src)
		if err == nil {
			r := newSeekableReader(src, table)
			return r, r.size, nil
		}
		if !errors.Is(err, errNotSeekable) {
			src.Close()
			return nil, 0, err
		}
		newDec := func(r io.Reader) (io.ReadCloser, error) {
			d, err := zstd.NewReader(r)
			if err != nil {
				return nil, err
			}
			return zstdReadCloser{d}, nil
		}
		size, ok := zstdContentSize(src)
		if !ok {
			// EncodeAll always recorded the content size, so this is rare; count
			// the plaintext with one pass through the decoder rather than hold it.
			if size, err = decodedSize(src, newDec); err != nil {
				src.Close()
				return nil, 0, err
			}
		}
		return &decompressStream{src: src, size: size, newDec: newDec}, size, nil
	case n >= 2 && magic[0] == 0x1F && magic[1] == 0x8B:
		newDec := func(r io.Reader) (io.ReadCloser, error) { return gzip.NewReader(r) }
		size, ok := gzipISize(src)
		if !ok {
			var err error
			if size, err = decodedSize(src, newDec); err != nil {
				src.Close()
				return nil, 0, err
			}
		}
		return &decompressStream{src: src, size: size, newDec: newDec}, size, nil
	default:
		// Not a compressed blob (e.g. written while compression was disabled) — the
		// inner reader already streams the plaintext.
		return src, storedSize, nil
	}
}

// decodedSize learns the plaintext length of a blob whose container does not
// record it by decoding it once into io.Discard, then rewinds src. Memory is the
// decoder's window; the old fallback held the whole plaintext instead.
func decodedSize(src ReadSeekCloser, newDec func(io.Reader) (io.ReadCloser, error)) (int64, error) {
	dec, err := newDec(src)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(io.Discard, io.LimitReader(dec, maxCompressedSize+1))
	dec.Close()
	if err != nil {
		return 0, fmt.Errorf("decompress: %w", err)
	}
	if n > maxCompressedSize {
		return 0, fmt.Errorf("decompressed data exceeds size limit")
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return n, nil
}

// bufferedDecompress is the last-resort path for a source that cannot seek:
// read the whole blob, decompress in memory, serve from a bytes reader. Every
// engine hands out seekable readers, so this is not reached in practice.
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

// decompressStream streams a single-frame zstd or gzip object so GET
// time-to-first-byte is independent of object size (issue #38). Read pulls from
// a streaming decoder over the compressed source.
//
// These codecs cannot be entered in the middle, so Seek (Range/partNumber) is a
// forward seek by discard: the decoder is positioned at the target by decoding
// and dropping everything before it, restarting from the front when the target
// is behind the current position. It used to materialise the whole plaintext
// once per reader, which with N concurrent Range requests was N copies of the
// object; discarding costs CPU proportional to the offset but holds only the
// decoder window. New writes use the seekable format and do not pay even that.
type decompressStream struct {
	src    ReadSeekCloser
	newDec func(io.Reader) (io.ReadCloser, error)
	dec    io.ReadCloser
	size   int64
	pos    int64 // plaintext offset the decoder will yield next
	target int64 // plaintext offset the caller asked for; equals pos once positioned
}

// ensure positions the decoder at d.target, opening or restarting it as needed.
func (d *decompressStream) ensure() error {
	if d.dec != nil && d.target < d.pos {
		d.dec.Close()
		d.dec = nil
	}
	if d.dec == nil {
		if _, err := d.src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		dec, err := d.newDec(d.src)
		if err != nil {
			return err
		}
		d.dec = dec
		d.pos = 0
	}
	if d.target > d.pos {
		n, err := io.CopyN(io.Discard, d.dec, d.target-d.pos)
		d.pos += n
		if err != nil && err != io.EOF {
			return err
		}
	}
	return nil
}

func (d *decompressStream) Read(p []byte) (int, error) {
	if d.target >= d.size {
		return 0, io.EOF
	}
	if err := d.ensure(); err != nil {
		return 0, err
	}
	n, err := d.dec.Read(p)
	d.pos += int64(n)
	d.target = d.pos
	return n, err
}

func (d *decompressStream) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = d.target + offset
	case io.SeekEnd:
		abs = d.size + offset
	default:
		return 0, fmt.Errorf("storage: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("storage: negative seek position %d", abs)
	}
	d.target = abs
	return abs, nil
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
// The zstd decoder decodes concatenated frames and skips the seek table's
// skippable frame, so a seekable blob decodes here too.
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
