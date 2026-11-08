package s3

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strings"
)

// crc64NVMETable is CRC-64/NVME, the checksum S3 itself defaults to (reflected
// polynomial 0x9a6c9329ac4bc9b5, check value 0xae8b14860a799888).
var crc64NVMETable = crc64.MakeTable(0x9a6c9329ac4bc9b5)

// putDigests computes an upload's digests while its body streams to the storage
// engine, so a PUT no longer has to be held in memory to be validated.
//
// Buffering the whole object was the single largest source of memory pressure on
// a busy node: a 64 MiB object at 64 concurrent uploads is 4 GiB of live heap in
// the handler alone, which is what OOM-killed pods under a large-object
// concurrency sweep (issue #46). Hashes are incremental by nature, so nothing has
// to be retained to compute them.
//
// Only the digests the request actually asks about are computed. Hashing bytes
// nobody checks is pure CPU cost on the hot path.
type putDigests struct {
	src io.Reader
	n   int64
	// expect is the length the request declared (Content-Length, or the
	// decoded length of an aws-chunked body), or -1 when unknown.
	expect int64
	sum    io.Writer // fan-out to the active hashes; nil when none are needed

	md5    hash.Hash // set only when Content-MD5 was sent
	sha256 hash.Hash
	crc32  hash.Hash32
	crc32c hash.Hash32
	sha1   hash.Hash
	crc64  hash.Hash64 // CRC-64/NVME

	// req, once set by checkInline, makes the reader verify the digests itself
	// when the body ends and fail the read if they do not match. The engine
	// then abandons its own temp file and never renames it over the object
	// already at the key. Verifying after the engine returned was too late for
	// an overwrite: the new bytes had already replaced the old ones, and
	// discarding them deleted the object while its old metadata stayed behind.
	req *http.Request
	// limit is how many bytes the body may carry before it takes the bucket
	// over its size quota, or -1. It is enforced while reading for the same
	// reason, since a chunked body's real length is not known up front.
	limit int64
	// done is set once the body reached its end and the digests were checked.
	done    bool
	sums    objectChecksums
	failure *reqError
}

// checkInline arms the in-reader checks for request r and a byte limit (-1 for
// none).
func (d *putDigests) checkInline(r *http.Request, limit int64) {
	d.req = r
	d.limit = limit
	// A body declared empty is checked before the engine is called at all.
	// An engine given a size of 0 may save an empty object without reading
	// anything, which in a bucket without versioning already replaces the
	// object at the key, so a check made later could only delete it.
	if d.expect == 0 {
		if err := d.probeEnd(); err == io.EOF {
			d.done = true
			sums, code, message, ok := d.verify(r)
			if !ok {
				d.failure = &reqError{code, message, http.StatusBadRequest}
			}
			d.sums = sums
		}
	}
}

// newPutDigests wraps body, enabling exactly the hashes this request needs.
func newPutDigests(r *http.Request, body io.Reader) *putDigests {
	d := &putDigests{src: body, limit: -1, expect: r.ContentLength}
	var ws []io.Writer

	if r.Header.Get("Content-MD5") != "" {
		d.md5 = md5.New()
		ws = append(ws, d.md5)
	}
	// A checksum comes either as a header, known up front, or as a trailer
	// after an aws-chunked body, declared up front in X-Amz-Trailer. Either
	// way the hash runs over the body, and verify compares it with the value
	// (maybeDecodeAwsChunked copies a trailer's value into the header).
	wants := func(header string) bool {
		return r.Header.Get(header) != "" || trailerDeclares(r, header)
	}
	if wants("X-Amz-Checksum-Sha256") {
		d.sha256 = sha256.New()
		ws = append(ws, d.sha256)
	}
	if wants("X-Amz-Checksum-Crc32") {
		d.crc32 = crc32.NewIEEE()
		ws = append(ws, d.crc32)
	}
	if wants("X-Amz-Checksum-Crc32c") {
		d.crc32c = crc32.New(crc32.MakeTable(crc32.Castagnoli))
		ws = append(ws, d.crc32c)
	}
	if wants("X-Amz-Checksum-Sha1") {
		d.sha1 = sha1.New()
		ws = append(ws, d.sha1)
	}
	if wants("X-Amz-Checksum-Crc64nvme") {
		d.crc64 = crc64.New(crc64NVMETable)
		ws = append(ws, d.crc64)
	}

	if len(ws) > 0 {
		d.sum = io.MultiWriter(ws...)
	}
	return d
}

var errBodyTooLong = &reqError{"IncompleteBody",
	"The request body is longer than the length it declared.", http.StatusBadRequest}

var errBodyTooShort = &reqError{"IncompleteBody",
	"You did not provide the number of bytes specified by the Content-Length HTTP header.", http.StatusBadRequest}

func (d *putDigests) Read(p []byte) (int, error) {
	if d.failure != nil {
		return 0, d.failure
	}
	if d.done {
		return 0, io.EOF
	}
	n, err := d.src.Read(p)
	if n > 0 {
		d.n += int64(n)
		if d.sum != nil {
			d.sum.Write(p[:n]) // hash.Hash never returns an error
		}
		if d.limit >= 0 && d.n > d.limit {
			d.failure = &reqError{"QuotaExceeded", "Maximum bucket size exceeded", http.StatusForbidden}
			return n, d.failure
		}
		if d.expect >= 0 && d.n > d.expect {
			d.failure = errBodyTooLong
			return n, d.failure
		}
	}
	if err != nil && err != io.EOF {
		// A check made while the body was decoded (a chunk or trailer
		// signature, the body's SHA-256, a malformed trailer) is this upload's
		// answer, whichever reader met it.
		if re := bodyFailure(err); re != nil {
			d.failure = re
		}
		return n, err
	}
	// The declared length has arrived. An engine that knows the size, like
	// the erasure-coding one, stops reading here and never sees EOF, so a
	// trailer after the data, and every check that runs at EOF, would only
	// happen after the engine had saved the object. Confirm the body ends
	// here and run the checks now, inside the engine's own read, so a refusal
	// still makes it abandon the write.
	if err == nil && d.req != nil && d.expect >= 0 && d.n == d.expect {
		err = d.probeEnd()
		if err != nil && err != io.EOF {
			return n, err
		}
	}
	// A body that ends before its declared length was cut short: an
	// aws-chunked body declares its decoded length, and an unsigned one can
	// lose its tail on the way. (A short plain body already fails in net/http.)
	if err == io.EOF && d.req != nil && d.expect >= 0 && d.n < d.expect {
		d.failure = errBodyTooShort
		return n, d.failure
	}
	if err == io.EOF && d.req != nil {
		d.done = true
		sums, code, message, ok := d.verify(d.req)
		if !ok {
			d.failure = &reqError{code, message, http.StatusBadRequest}
			return n, d.failure
		}
		d.sums = sums
	}
	return n, err
}

// probeEnd reads past the declared length to reach the body's end. More data
// there means the body is longer than it said, and is refused.
func (d *putDigests) probeEnd() error {
	var b [1]byte
	for i := 0; i < 16; i++ {
		m, err := d.src.Read(b[:])
		if m > 0 {
			d.failure = errBodyTooLong
			return d.failure
		}
		if err == io.EOF {
			return io.EOF
		}
		if err != nil {
			if re := bodyFailure(err); re != nil {
				d.failure = re
			}
			return err
		}
	}
	return nil // no end yet after repeated empty reads: leave it to the engine
}

// size is how many bytes actually arrived, which is the authoritative length
// once the body has been consumed: a chunked or aws-chunked upload's decoded
// length is not knowable from Content-Length.
func (d *putDigests) size() int64 { return d.n }

// verify checks the computed digests against what the client promised and
// returns the values to record on the object. It reports an S3 error code and
// message instead of writing a response, because the caller has to undo the
// write it already performed before answering.
//
// The checks and their error codes mirror the pre-streaming validation exactly,
// so a client sees the same rejection as before, only after the bytes have been
// written and discarded rather than before.
func (d *putDigests) verify(r *http.Request) (sums objectChecksums, code, message string, ok bool) {
	if v := r.Header.Get("Content-MD5"); v != "" && d.md5 != nil {
		expected, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(expected) != md5.Size {
			return sums, "InvalidDigest", "Content-MD5 is invalid", false
		}
		if !equalBytes(expected, d.md5.Sum(nil)) {
			return sums, "BadDigest", "Content-MD5 does not match", false
		}
	}

	checks := []struct {
		on     bool
		header string
		name   string
		sum    func() string
		out    *string
	}{
		{d.sha256 != nil, "X-Amz-Checksum-Sha256", "SHA256", func() string { return base64.StdEncoding.EncodeToString(d.sha256.Sum(nil)) }, &sums.SHA256},
		{d.crc32 != nil, "X-Amz-Checksum-Crc32", "CRC32", func() string { return base32Sum(d.crc32.Sum32()) }, &sums.CRC32},
		{d.crc32c != nil, "X-Amz-Checksum-Crc32c", "CRC32C", func() string { return base32Sum(d.crc32c.Sum32()) }, &sums.CRC32C},
		{d.sha1 != nil, "X-Amz-Checksum-Sha1", "SHA1", func() string { return base64.StdEncoding.EncodeToString(d.sha1.Sum(nil)) }, &sums.SHA1},
		{d.crc64 != nil, "X-Amz-Checksum-Crc64nvme", "CRC64NVME", func() string { return base64Sum64(d.crc64.Sum64()) }, &sums.CRC64NVME},
	}
	for _, c := range checks {
		if !c.on {
			continue
		}
		computed := c.sum()
		v := r.Header.Get(c.header)
		if v == "" {
			// Declared as a trailer that never arrived. Before, a trailer was
			// never read at all, so whatever it said was accepted.
			return sums, "MalformedTrailerError", "The request declared " + strings.ToLower(c.header) + " in X-Amz-Trailer but did not send it", false
		}
		if v != computed {
			return sums, "BadDigest", errChecksumMismatch(c.name).Error(), false
		}
		*c.out = computed
	}
	return sums, "", "", true
}

// trailerDeclares reports whether X-Amz-Trailer lists header (a comma list,
// any case).
func trailerDeclares(r *http.Request, header string) bool {
	// Get, not Values: the signature covers the first value only, so a second
	// X-Amz-Trailer header added on the way must not count.
	for _, t := range strings.Split(r.Header.Get("X-Amz-Trailer"), ",") {
		if strings.EqualFold(strings.TrimSpace(t), header) {
			return true
		}
	}
	return false
}

// objectChecksums are the S3 checksum values recorded on an object.
type objectChecksums struct {
	SHA256, CRC32, CRC32C, SHA1, CRC64NVME string
}

func base64Sum64(v uint64) string {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return base64.StdEncoding.EncodeToString(b)
}

func base32Sum(v uint32) string {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return base64.StdEncoding.EncodeToString(b)
}

// equalBytes compares digests. Not constant-time on purpose: these are integrity
// checks on data the client just sent, not secrets.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
