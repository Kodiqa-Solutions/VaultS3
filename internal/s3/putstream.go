package s3

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
)

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
	sum io.Writer // fan-out to the active hashes; nil when none are needed

	md5    hash.Hash // set only when Content-MD5 was sent
	sha256 hash.Hash
	crc32  hash.Hash32
	crc32c hash.Hash32
	sha1   hash.Hash

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
}

// newPutDigests wraps body, enabling exactly the hashes this request needs.
func newPutDigests(r *http.Request, body io.Reader) *putDigests {
	d := &putDigests{src: body, limit: -1}
	var ws []io.Writer

	if r.Header.Get("Content-MD5") != "" {
		d.md5 = md5.New()
		ws = append(ws, d.md5)
	}
	// The trailer form carries the checksum after the body, so the value is not
	// known up front; it is still computed and recorded, matching the buffered
	// behaviour this replaces.
	if r.Header.Get("X-Amz-Checksum-Sha256") != "" || r.Header.Get("X-Amz-Trailer") == "x-amz-checksum-sha256" {
		d.sha256 = sha256.New()
		ws = append(ws, d.sha256)
	}
	if r.Header.Get("X-Amz-Checksum-Crc32") != "" {
		d.crc32 = crc32.NewIEEE()
		ws = append(ws, d.crc32)
	}
	if r.Header.Get("X-Amz-Checksum-Crc32c") != "" {
		d.crc32c = crc32.New(crc32.MakeTable(crc32.Castagnoli))
		ws = append(ws, d.crc32c)
	}
	if r.Header.Get("X-Amz-Checksum-Sha1") != "" {
		d.sha1 = sha1.New()
		ws = append(ws, d.sha1)
	}

	if len(ws) > 0 {
		d.sum = io.MultiWriter(ws...)
	}
	return d
}

func (d *putDigests) Read(p []byte) (int, error) {
	if d.failure != nil {
		return 0, d.failure
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

	if d.sha256 != nil {
		computed := base64.StdEncoding.EncodeToString(d.sha256.Sum(nil))
		if v := r.Header.Get("X-Amz-Checksum-Sha256"); v != "" && v != computed {
			return sums, "BadDigest", errChecksumMismatch("SHA256").Error(), false
		}
		sums.SHA256 = computed
	}
	if d.crc32 != nil {
		computed := base32Sum(d.crc32.Sum32())
		if v := r.Header.Get("X-Amz-Checksum-Crc32"); v != computed {
			return sums, "BadDigest", errChecksumMismatch("CRC32").Error(), false
		}
		sums.CRC32 = computed
	}
	if d.crc32c != nil {
		computed := base32Sum(d.crc32c.Sum32())
		if v := r.Header.Get("X-Amz-Checksum-Crc32c"); v != computed {
			return sums, "BadDigest", errChecksumMismatch("CRC32C").Error(), false
		}
		sums.CRC32C = computed
	}
	if d.sha1 != nil {
		computed := base64.StdEncoding.EncodeToString(d.sha1.Sum(nil))
		if v := r.Header.Get("X-Amz-Checksum-Sha1"); v != computed {
			return sums, "BadDigest", errChecksumMismatch("SHA1").Error(), false
		}
		sums.SHA1 = computed
	}
	return sums, "", "", true
}

// objectChecksums are the S3 checksum values recorded on an object.
type objectChecksums struct {
	SHA256, CRC32, CRC32C, SHA1 string
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
