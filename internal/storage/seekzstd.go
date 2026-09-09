package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/klauspost/compress/zstd"
)

// Seekable zstd for compressed objects.
//
// A compressed object used to be one zstd frame. That reads perfectly well from
// the front, which is what issue #38 needed, but a zstd frame cannot be entered
// in the middle: the only way to serve a Range request was to decompress the
// whole object into memory and slice it. With encryption made chunked in 4.4.53
// (issue #49) and the remaining whole-object read paths taken up after #53, that
// left compression as the last layer where twenty concurrent Range requests on
// one object meant twenty full plaintext copies of it.
//
// New objects are written in the zstd seekable format (facebook/zstd
// contrib/seekable_format): a sequence of ordinary, independent zstd frames of
// bounded plaintext size, followed by a skippable frame holding a seek table:
//
//	frame 0 | frame 1 | ... | frame N-1 | seek table
//
//	seek table : skippableMagic[4] 0x184D2A5E | frameSize[4]
//	             entries: N x (compressedSize[4] | decompressedSize[4])
//	             footer : numberOfFrames[4] | descriptor[1] | seekableMagic[4] 0x8F92EAB1
//
// All integers are little-endian. A Range read then costs one frame: find the
// frame that holds the offset in the table, seek the stored blob to it, and
// decompress just that frame. Any ordinary zstd decoder still reads the whole
// blob from the start, because it decodes concatenated frames and skips the
// skippable one, so a tool with no knowledge of the table sees a valid zstd
// stream and legacy code paths keep working.
//
// Frames are cut every defaultCompressFrame bytes of plaintext, aligned with
// defaultStreamChunk: compression runs before encryption, so a Range read that
// crosses both layers fetches one compressed frame, which spans at most two
// encrypted chunks.
const (
	seekSkippableMagic = 0x184D2A5E
	seekableMagic      = 0x8F92EAB1
	seekFooterLen      = 4 + 1 + 4 // frames | descriptor | magic
	seekEntryLen       = 8         // compressedSize | decompressedSize, no checksum
	seekEntryLenCk     = 12        // the same with the optional per-frame checksum
	seekSkipHeaderLen  = 8         // skippable magic | frame size
	seekChecksumFlag   = 1 << 7    // descriptor bit: entries carry a checksum

	// maxSeekFrame bounds what a seek table may ask us to allocate for one frame,
	// so a corrupt or hostile table cannot turn a Range read into a huge
	// allocation. Matches maxStreamChunk.
	maxSeekFrame = 64 << 20
)

// defaultCompressFrame is the plaintext per zstd frame for new writes. It is the
// per-reader memory floor for a compressed Range read (one compressed and one
// decompressed frame) and it matches defaultStreamChunk so a frame lands inside
// as few encrypted chunks as possible. A var rather than a const so tests can
// shrink it and exercise multi-frame objects without writing megabytes. Readers
// take frame sizes from the blob's own table, so changing this never breaks
// stored objects.
var defaultCompressFrame = 1 << 20

// zstdFrameDecoder decodes one frame at a time for the seekable reader. DecodeAll
// is safe for concurrent use. The memory cap is per decode, so the shared limit
// bounds one frame, not the object.
var zstdFrameDecoder, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxSeekFrame))

var errNotSeekable = errors.New("storage: not a seekable zstd blob")

// seekFrame is one entry of the seek table with its cumulative offsets filled in.
type seekFrame struct {
	compOff, compSize   int64
	plainOff, plainSize int64
}

// seekTableWriter accumulates frame sizes as frames are written and emits the
// skippable frame that ends a seekable blob.
type seekTableWriter struct {
	entries []byte
	frames  uint32
}

func (t *seekTableWriter) add(compressed, decompressed int) {
	t.entries = binary.LittleEndian.AppendUint32(t.entries, uint32(compressed))
	t.entries = binary.LittleEndian.AppendUint32(t.entries, uint32(decompressed))
	t.frames++
}

func (t *seekTableWriter) bytes() []byte {
	out := make([]byte, 0, seekSkipHeaderLen+len(t.entries)+seekFooterLen)
	out = binary.LittleEndian.AppendUint32(out, seekSkippableMagic)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(t.entries)+seekFooterLen))
	out = append(out, t.entries...)
	out = binary.LittleEndian.AppendUint32(out, t.frames)
	out = append(out, 0) // descriptor: no checksums
	out = binary.LittleEndian.AppendUint32(out, seekableMagic)
	return out
}

// writeSeekableZstd compresses src into dst as a seekable blob, one frame per
// frameSize bytes of plaintext, and returns the plaintext length consumed. Memory
// is one plaintext frame and one compressed frame regardless of object size,
// which is what keeps a large upload from costing copies of itself (issue #46).
// An empty source still yields one (empty) frame so the blob begins with the
// zstd magic that reads use to recognise it.
func writeSeekableZstd(dst io.Writer, src io.Reader, frameSize int) (int64, error) {
	if frameSize <= 0 || frameSize > maxSeekFrame {
		frameSize = defaultCompressFrame
	}
	plain := make([]byte, frameSize)
	var comp []byte
	var table seekTableWriter
	var total int64
	for {
		n, rerr := readFrame(src, plain)
		if rerr != nil {
			return total, rerr
		}
		if n == 0 && table.frames > 0 {
			break
		}
		// EncodeAll records the frame content size in each frame header, so a
		// decoder that ignores the table still knows every frame's size.
		comp = zstdEncoder.EncodeAll(plain[:n], comp[:0])
		if _, err := dst.Write(comp); err != nil {
			return total, err
		}
		table.add(len(comp), n)
		total += int64(n)
		if n < frameSize {
			break
		}
	}
	_, err := dst.Write(table.bytes())
	return total, err
}

// readFrame fills buf from src, stopping early only at end of input. Unlike
// io.ReadFull it passes a source's own io.ErrUnexpectedEOF through as a failure,
// so a body that breaks off mid-upload is not mistaken for a short last frame.
func readFrame(src io.Reader, buf []byte) (int, error) {
	var n int
	for n < len(buf) {
		m, err := src.Read(buf[n:])
		n += m
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// readSeekTable recognises a seekable blob by its footer and loads the table.
// src is left positioned at the start. Returns errNotSeekable for a plain zstd
// blob so the caller can take the single-frame path.
func readSeekTable(src io.ReadSeeker) ([]seekFrame, error) {
	end, err := src.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if end < seekSkipHeaderLen+seekFooterLen {
		return nil, rewindWith(src, errNotSeekable)
	}
	var footer [seekFooterLen]byte
	if _, err := src.Seek(end-seekFooterLen, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(src, footer[:]); err != nil {
		return nil, rewindWith(src, err)
	}
	if binary.LittleEndian.Uint32(footer[5:]) != seekableMagic {
		return nil, rewindWith(src, errNotSeekable)
	}
	frames := int64(binary.LittleEndian.Uint32(footer[:4]))
	entryLen := int64(seekEntryLen)
	if footer[4]&seekChecksumFlag != 0 {
		entryLen = seekEntryLenCk
	}
	tableLen := seekSkipHeaderLen + frames*entryLen + seekFooterLen
	if tableLen > end {
		return nil, rewindWith(src, fmt.Errorf("storage: seek table of %d frames does not fit in %d bytes", frames, end))
	}
	if _, err := src.Seek(end-tableLen, io.SeekStart); err != nil {
		return nil, err
	}
	raw := make([]byte, tableLen-seekFooterLen)
	if _, err := io.ReadFull(src, raw); err != nil {
		return nil, rewindWith(src, err)
	}
	if binary.LittleEndian.Uint32(raw[:4]) != seekSkippableMagic ||
		int64(binary.LittleEndian.Uint32(raw[4:8])) != tableLen-seekSkipHeaderLen {
		return nil, rewindWith(src, fmt.Errorf("storage: seek table header is corrupt"))
	}

	table := make([]seekFrame, frames)
	var compOff, plainOff int64
	for i := range table {
		e := raw[seekSkipHeaderLen+int64(i)*entryLen:]
		f := seekFrame{
			compOff:   compOff,
			compSize:  int64(binary.LittleEndian.Uint32(e[:4])),
			plainOff:  plainOff,
			plainSize: int64(binary.LittleEndian.Uint32(e[4:8])),
		}
		if f.plainSize > maxSeekFrame || f.compSize > maxSeekFrame {
			return nil, rewindWith(src, fmt.Errorf("storage: seek table frame %d is %d/%d bytes, over the %d limit", i, f.compSize, f.plainSize, maxSeekFrame))
		}
		table[i] = f
		compOff += f.compSize
		plainOff += f.plainSize
	}
	// The frames and the table must account for exactly the whole blob, which
	// catches truncation and a table that belongs to some other object.
	if compOff+tableLen != end {
		return nil, rewindWith(src, fmt.Errorf("storage: seek table covers %d bytes of a %d byte blob", compOff+tableLen, end))
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return table, nil
}

// rewindWith rewinds src to the start and returns err, so every early exit from
// readSeekTable leaves the source where the next detection step expects it.
func rewindWith(src io.Seeker, err error) error {
	if _, serr := src.Seek(0, io.SeekStart); serr != nil {
		return serr
	}
	return err
}

// seekableReader serves a seekable zstd blob as plaintext, decompressing one
// frame at a time. It holds one compressed and one decompressed frame, so a
// concurrent Range reader costs a fixed ~2 MiB rather than a copy of the object.
type seekableReader struct {
	src    ReadSeekCloser
	frames []seekFrame
	size   int64

	buf     []byte // plaintext of the frame currently loaded
	bufIdx  int    // which frame that is; -1 when nothing is loaded
	comp    []byte // scratch for the compressed frame
	pos     int64  // absolute plaintext offset of the next byte to return
	srcAt   int64  // where src is positioned, to skip redundant seeks
	lastErr error
}

func newSeekableReader(src ReadSeekCloser, frames []seekFrame) *seekableReader {
	var size int64
	if n := len(frames); n > 0 {
		size = frames[n-1].plainOff + frames[n-1].plainSize
	}
	return &seekableReader{src: src, frames: frames, size: size, bufIdx: -1}
}

// frameFor finds the frame holding plaintext offset off. Frames are uniform on
// write but the table is the authority, so this is a search rather than a divide.
func (r *seekableReader) frameFor(off int64) int {
	return sort.Search(len(r.frames), func(i int) bool {
		return r.frames[i].plainOff+r.frames[i].plainSize > off
	})
}

// loadFrame decompresses frame idx into r.buf.
func (r *seekableReader) loadFrame(idx int) error {
	if r.bufIdx == idx {
		return nil
	}
	if idx < 0 || idx >= len(r.frames) {
		return io.EOF
	}
	f := r.frames[idx]
	if r.srcAt != f.compOff {
		if _, err := r.src.Seek(f.compOff, io.SeekStart); err != nil {
			return err
		}
		r.srcAt = f.compOff
	}
	if int64(cap(r.comp)) < f.compSize {
		r.comp = make([]byte, f.compSize)
	}
	r.comp = r.comp[:f.compSize]
	if _, err := io.ReadFull(r.src, r.comp); err != nil {
		r.srcAt = -1
		return fmt.Errorf("storage: read compressed frame %d: %w", idx, err)
	}
	r.srcAt = f.compOff + f.compSize
	if int64(cap(r.buf)) < f.plainSize {
		r.buf = make([]byte, 0, f.plainSize)
	}
	plain, err := zstdFrameDecoder.DecodeAll(r.comp, r.buf[:0])
	if err != nil {
		return fmt.Errorf("storage: decompress frame %d: %w", idx, err)
	}
	if int64(len(plain)) != f.plainSize {
		return fmt.Errorf("storage: frame %d decompressed to %d bytes, seek table says %d", idx, len(plain), f.plainSize)
	}
	r.buf = plain
	r.bufIdx = idx
	return nil
}

func (r *seekableReader) Read(p []byte) (int, error) {
	if r.lastErr != nil {
		return 0, r.lastErr
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	idx := r.frameFor(r.pos)
	if err := r.loadFrame(idx); err != nil {
		r.lastErr = err
		return 0, err
	}
	within := int(r.pos - r.frames[idx].plainOff)
	n := copy(p, r.buf[within:])
	r.pos += int64(n)
	return n, nil
}

func (r *seekableReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.size + offset
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

func (r *seekableReader) Close() error { return r.src.Close() }
