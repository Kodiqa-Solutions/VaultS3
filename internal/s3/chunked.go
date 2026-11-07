package s3

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maybeDecodeAwsChunked transparently decodes an `aws-chunked` request body.
//
// Modern AWS SDKs (boto3/botocore 1.36+, aws-sdk-js v3, aws-cli) default to
// "flexible checksums" and, when the transport supports it (notably HTTP/2, which
// Go negotiates for any TLS listener), stream the body using the AWS chunked
// content-encoding: the payload is framed as `<hex-size>\r\n<data>\r\n…0\r\n` with
// a trailing checksum, and `x-amz-content-sha256` is set to STREAMING-…-PAYLOAD.
//
// Without decoding, the chunk-size headers and trailer get stored verbatim as part
// of the object, corrupting every such upload (e.g. a 100-byte PUT is stored as
// 142 bytes). This strips the framing so handlers read the real object bytes.
//
// It decodes without checking chunk signatures. The S3 handler uses
// decodeAwsChunked with a signer, so a signed streaming upload is verified.
func maybeDecodeAwsChunked(r *http.Request) {
	decodeAwsChunked(r, nil)
}

// isAwsChunked reports whether r carries an aws-chunked body.
func isAwsChunked(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") ||
		strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-")
}

// isSignedStreaming reports whether r says its chunks are signed.
func isSignedStreaming(r *http.Request) bool {
	v := r.Header.Get("X-Amz-Content-Sha256")
	return v == signedStreaming || v == signedStreamingTrailer
}

// decodeAwsChunked replaces an aws-chunked body with its decoded bytes. With a
// signer, every chunk's signature is checked in sequence, and so is the
// trailer's, and a read that meets a bad one fails.
//
// SigV4 verification of the request itself is unaffected: the streaming modes
// sign the literal `STREAMING-…-PAYLOAD` string as the payload hash, so auth
// never reads the body and must run before this is called.
func decodeAwsChunked(r *http.Request, signer *chunkSigner) {
	if r.Body == nil || !isAwsChunked(r) {
		// A trailer can only follow an aws-chunked body, so a declaration on
		// any other body names nothing that will arrive.
		r.Header.Del("X-Amz-Trailer")
		return
	}
	r.Body = &awsChunkedBody{
		br:            bufio.NewReaderSize(r.Body, 64<<10),
		req:           r,
		signer:        signer,
		trailing:      r.Header.Get("X-Amz-Trailer") != "" || r.Header.Get("X-Amz-Content-Sha256") == signedStreamingTrailer,
		signedTrailer: signer != nil && r.Header.Get("X-Amz-Content-Sha256") == signedStreamingTrailer,
		closer:        r.Body,
	}

	// The decoded object length lives in this header, not Content-Length (which
	// counts the framed bytes). Fix ContentLength so size-based logic — quota,
	// multipart/part-size caps, metrics — sees the true size.
	if dl := r.Header.Get("X-Amz-Decoded-Content-Length"); dl != "" {
		if n, err := strconv.ParseInt(dl, 10, 64); err == nil {
			r.ContentLength = n
			r.Header.Set("Content-Length", dl)
		}
	}
	// Drop the encoding markers so nothing downstream double-decodes.
	r.Header.Del("Content-Encoding")
}

// maxTrailerBytes bounds the trailer read after an aws-chunked body. A real one
// is a checksum line and an optional signature line, well under 1 KiB.
const maxTrailerBytes = 4096

// maxChunkLine bounds one chunk header line (`<hex-size>;chunk-signature=<64 hex>`).
const maxChunkLine = 256

// trailerChecksums maps the checksum names a trailer may carry to the request
// header the digest checks read.
var trailerChecksums = map[string]string{
	"x-amz-checksum-crc32":     "X-Amz-Checksum-Crc32",
	"x-amz-checksum-crc32c":    "X-Amz-Checksum-Crc32c",
	"x-amz-checksum-sha1":      "X-Amz-Checksum-Sha1",
	"x-amz-checksum-sha256":    "X-Amz-Checksum-Sha256",
	"x-amz-checksum-crc64nvme": "X-Amz-Checksum-Crc64nvme",
}

var (
	errMalformedChunk = &reqError{"IncompleteBody", "The request body is not a well-formed aws-chunked body.", http.StatusBadRequest}
	errMalformedTrail = &reqError{"MalformedTrailerError", "The request contained trailing data that was not well-formed", http.StatusBadRequest}
	errMissingChunkSg = &reqError{"SignatureDoesNotMatch", "A signed streaming upload sent a chunk without its chunk-signature.", http.StatusForbidden}
	errMissingTrailSg = &reqError{"SignatureDoesNotMatch", "A signed streaming upload sent a trailer without its x-amz-trailer-signature.", http.StatusForbidden}
)

// awsChunkedBody decodes `<hex-size>[;chunk-signature=<sig>]\r\n<data>\r\n`
// chunks up to the zero-length one, then reads the trailer.
//
// With a signer each chunk's signature is checked when the chunk ends, chained
// from the one before, and the trailer's signature after it. A chunk's data is
// handed on before its signature can be checked (the check needs the whole
// chunk), so a bad signature fails a later read: the storage engine then
// abandons the write, as it does for any body that fails part way.
//
// The trailer's checksum value is copied into the matching request header,
// where the digest checks compare it with the bytes that arrived. A value
// already given as a header wins, so a trailer cannot replace a checksum the
// client committed to up front.
type awsChunkedBody struct {
	br            *bufio.Reader
	req           *http.Request
	signer        *chunkSigner
	trailing      bool
	signedTrailer bool
	closer        io.Closer

	remaining int64     // bytes left in the current chunk
	sig       string    // the current chunk's declared signature
	sum       hash.Hash // SHA-256 of the current chunk, when signed
	err       error     // sticky: io.EOF at the end, or why the body failed
}

func (b *awsChunkedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	for b.remaining == 0 {
		if err := b.nextChunk(); err != nil {
			b.err = err
			return 0, err
		}
		if b.err != nil { // the zero-length chunk and the trailer were read
			return 0, b.err
		}
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.br.Read(p)
	b.remaining -= int64(n)
	if b.sum != nil {
		b.sum.Write(p[:n])
	}
	if err != nil {
		if err == io.EOF {
			err = errMalformedChunk // the body ended inside a chunk
		}
		b.err = err
		return n, err
	}
	if b.remaining == 0 {
		if err := b.endChunk(); err != nil {
			b.err = err
			return n, err
		}
	}
	return n, nil
}

func (b *awsChunkedBody) Close() error { return b.closer.Close() }

// readLine reads one line of at most max bytes, without its line ending.
// ReadSlice never buffers more than the reader's own buffer, so a line with no
// end cannot grow memory.
func (b *awsChunkedBody) readLine(max int) (string, error) {
	line, err := b.br.ReadSlice('\n')
	if len(line) > max+2 {
		return "", errMalformedChunk
	}
	if err != nil {
		if err == io.EOF && len(line) == 0 {
			return "", io.EOF
		}
		if err == io.EOF { // a last line without its line ending
			return strings.TrimRight(string(line), "\r"), nil
		}
		return "", errMalformedChunk // bufio.ErrBufferFull: no line ending in sight
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// nextChunk reads a chunk header. At the zero-length chunk it checks that
// chunk's signature, reads the trailer, and sets b.err to io.EOF.
func (b *awsChunkedBody) nextChunk() error {
	line, err := b.readLine(maxChunkLine)
	if err != nil {
		if err == io.EOF {
			return errMalformedChunk // the body ended before its last chunk
		}
		return err
	}
	sizeHex, ext, _ := strings.Cut(line, ";")
	size, perr := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
	if perr != nil || size < 0 {
		return errMalformedChunk
	}
	b.sig = ""
	if v, ok := strings.CutPrefix(strings.TrimSpace(ext), "chunk-signature="); ok {
		b.sig = v
	}
	if b.signer != nil {
		if b.sig == "" {
			return errMissingChunkSg
		}
		b.sum = sha256.New()
	}
	if size > 0 {
		b.remaining = size
		return nil
	}

	// The zero-length chunk ends the data.
	if b.signer != nil {
		if err := b.signer.chunk(b.sum.Sum(nil), b.sig); err != nil {
			return err
		}
	}
	if err := b.readTrailer(); err != nil {
		return err
	}
	b.err = io.EOF
	return nil
}

// endChunk reads the CRLF after a chunk's data and checks its signature.
func (b *awsChunkedBody) endChunk() error {
	crlf := make([]byte, 2)
	if _, err := io.ReadFull(b.br, crlf); err != nil || !bytes.Equal(crlf, []byte("\r\n")) {
		return errMalformedChunk
	}
	if b.signer != nil {
		return b.signer.chunk(b.sum.Sum(nil), b.sig)
	}
	return nil
}

// readTrailer reads `name:value` lines up to the blank line that ends the
// request, at most maxTrailerBytes in all. Checksum values go into the request
// headers. On a signed upload the x-amz-trailer-signature line must sign the
// other lines.
func (b *awsChunkedBody) readTrailer() error {
	var canonical strings.Builder
	trailerSig := ""
	read := 0
	for {
		line, err := b.readLine(maxTrailerBytes)
		if err == io.EOF {
			break // the body ended without the final blank line
		}
		if err != nil {
			return errMalformedTrail
		}
		read += len(line) + 2
		if read > maxTrailerBytes {
			return errMalformedTrail
		}
		if line == "" {
			// minio-go puts a blank line between the checksum lines and the
			// signature line; AWS's own layout has none. So on a signed
			// trailer a blank line before the signature is not the end.
			if b.signedTrailer && trailerSig == "" {
				continue
			}
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return errMalformedTrail
		}
		name, value = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)
		if name == "x-amz-trailer-signature" {
			trailerSig = value
			continue
		}
		canonical.WriteString(name + ":" + value + "\n")
		if header, known := trailerChecksums[name]; known && b.req.Header.Get(header) == "" {
			b.req.Header.Set(header, value)
		}
	}
	if b.signedTrailer {
		if trailerSig == "" {
			return errMissingTrailSg
		}
		return b.signer.trailer(canonical.String(), trailerSig)
	}
	return nil
}

// bodyFailure returns err as a reqError when it is one, so a check that failed
// while the body was read can be answered with its own code.
func bodyFailure(err error) *reqError {
	var re *reqError
	if errors.As(err, &re) {
		return re
	}
	return nil
}
