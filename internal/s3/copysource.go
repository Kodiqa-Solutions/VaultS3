package s3

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// copySource is a parsed X-Amz-Copy-Source header.
type copySource struct {
	bucket    string
	key       string
	versionID string // empty means the current version
}

var errBadCopySource = errors.New("invalid x-amz-copy-source")

// parseCopySourceHeader parses an X-Amz-Copy-Source header into the bucket, key
// and optional version it names.
//
// Authorization and the two copy handlers each used to parse this header their
// own way. The router unescaped it and cut a ?versionId suffix off, CopyObject
// unescaped it but kept the suffix as part of the key, and UploadPartCopy did
// neither. So the key that was authorized was not always the key that was then
// read: a grant on one key could be used to read another. There is now exactly
// one parser and every caller uses it.
//
// The query is split off BEFORE unescaping, so a key holding an encoded '?'
// (%3F) stays one key instead of being cut at it.
func parseCopySourceHeader(header string) (copySource, error) {
	raw := header
	var rawQuery string
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw, rawQuery = raw[:i], raw[i+1:]
	}
	path, err := url.PathUnescape(raw)
	if err != nil {
		return copySource{}, errBadCopySource
	}
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return copySource{}, errBadCopySource
	}
	src := copySource{bucket: parts[0], key: parts[1]}
	if objectKeyProblem(src.key) != "" {
		return copySource{}, errBadCopySource
	}
	// Reject double-encoded path traversal (e.g. %252e%252e becomes %2e%2e,
	// then ..), which the copy handler has always refused.
	if decoded, err := url.PathUnescape(src.key); err == nil && decoded != src.key {
		if objectKeyProblem(decoded) != "" {
			return copySource{}, errBadCopySource
		}
	}
	if rawQuery != "" {
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return copySource{}, errBadCopySource
		}
		if vs, ok := q["versionId"]; ok {
			if len(vs) != 1 || vs[0] == "" {
				return copySource{}, errBadCopySource
			}
			src.versionID = vs[0]
		}
	}
	return src, nil
}

// Copy-source SSE-C headers name the key the SOURCE was sealed with. The
// destination is sealed, or not, by the ordinary SSE-C headers.
const (
	hdrCopySSECAlgo   = "X-Amz-Copy-Source-Server-Side-Encryption-Customer-Algorithm"
	hdrCopySSECKey    = "X-Amz-Copy-Source-Server-Side-Encryption-Customer-Key"
	hdrCopySSECKeyMD5 = "X-Amz-Copy-Source-Server-Side-Encryption-Customer-Key-Md5"
)

// openCopySource resolves a copy source through the metadata store and opens the
// plaintext of the version it names.
//
// The copy handlers used to open the source with a plain engine read of the key.
// That path holds nothing for an object in a versioned bucket, whose bytes live
// under .vs/, so copying FROM a versioned bucket answered NoSuchKey, and a named
// ?versionId was ignored. An SSE-C source was read as its stored ciphertext and
// then stored at the destination as if it were plaintext.
//
// On failure the response has been written and ok is false.
func (h *ObjectHandler) openCopySource(w http.ResponseWriter, r *http.Request, src copySource) (*metadata.ObjectMeta, storage.ReadSeekCloser, int64, bool) {
	if !h.store.BucketExists(src.bucket) {
		writeS3Error(w, "NoSuchBucket", "Source bucket does not exist", http.StatusNotFound)
		return nil, nil, 0, false
	}
	var meta *metadata.ObjectMeta
	var err error
	if src.versionID != "" {
		meta, err = h.store.GetObjectVersion(src.bucket, src.key, src.versionID)
		if err != nil && src.versionID == nullVersionID {
			// An object written before versioning was turned on is the null
			// version, recorded only in the latest pointer.
			if cur, cerr := h.store.GetObjectMeta(src.bucket, src.key); cerr == nil && cur != nil && cur.VersionID == "" {
				meta, err = cur, nil
			}
		}
		if err != nil {
			if metadataUnavailable(w, err) {
				return nil, nil, 0, false
			}
			writeS3Error(w, "NoSuchVersion", "The specified version does not exist", http.StatusNotFound)
			return nil, nil, 0, false
		}
		if meta.DeleteMarker {
			writeS3Error(w, "InvalidRequest", "The source of a copy request may not specifically refer to a delete marker by version id", http.StatusBadRequest)
			return nil, nil, 0, false
		}
	} else {
		meta, err = h.store.GetObjectMetaConsistent(src.bucket, src.key)
		if meta == nil && metadataUnavailable(w, err) {
			return nil, nil, 0, false
		}
		if meta == nil || meta.DeleteMarker {
			writeS3Error(w, "NoSuchKey", "Source object not found", http.StatusNotFound)
			return nil, nil, 0, false
		}
	}

	reader, size, err := h.readObjectData(src.bucket, src.key, meta.VersionID)
	if err != nil {
		writeS3Error(w, "NoSuchKey", "Source object not found", http.StatusNotFound)
		return nil, nil, 0, false
	}

	if meta.SSECustomerKeyMD5 != "" {
		k, perr := parseSSECHeadersNamed(r, hdrCopySSECAlgo, hdrCopySSECKey, hdrCopySSECKeyMD5)
		if perr != nil || k == nil {
			reader.Close()
			writeS3Error(w, "InvalidRequest", "The source object is SSE-C encrypted; its customer key is required in the x-amz-copy-source-server-side-encryption-customer-* headers", http.StatusBadRequest)
			return nil, nil, 0, false
		}
		if k.keyMD5 != meta.SSECustomerKeyMD5 {
			reader.Close()
			writeS3Error(w, "AccessDenied", "SSE-C key does not match the one used to encrypt the source object", http.StatusForbidden)
			return nil, nil, 0, false
		}
		plain, plainSize, derr := ssecOpenStored(k, reader, size)
		if derr != nil {
			reader.Close()
			writeS3Error(w, "AccessDenied", "SSE-C decryption failed", http.StatusForbidden)
			return nil, nil, 0, false
		}
		reader, size = plain, plainSize
	}
	return meta, reader, size, true
}
