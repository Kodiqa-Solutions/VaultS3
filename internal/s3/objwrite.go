package s3

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// reqError is a refusal that carries the S3 error code, message and status it
// is answered with. It can travel through a storage engine as a read error, so a
// check made while the body streams reaches the client as itself rather than as
// an internal error.
type reqError struct {
	code   string
	msg    string
	status int
}

func (e *reqError) Error() string { return e.code + ": " + e.msg }

// writeReqError answers err if it is a reqError and reports whether it did.
func writeReqError(w http.ResponseWriter, err error) bool {
	var re *reqError
	if errors.As(err, &re) {
		writeS3Error(w, re.code, re.msg, re.status)
		return true
	}
	return false
}

// metaWriteError is a failed metadata write after the bytes were stored.
type metaWriteError struct {
	op  string
	err error
}

func (e *metaWriteError) Error() string { return e.op + ": " + e.err.Error() }
func (e *metaWriteError) Unwrap() error { return e.err }

// answerWriteError answers a failed writeObject.
func answerWriteError(w http.ResponseWriter, err error, bucket, key string) {
	if writeReqError(w, err) {
		return
	}
	var mw *metaWriteError
	if errors.As(err, &mw) {
		metaWriteFailed(w, mw.err, mw.op, bucket, key)
		return
	}
	writePutError(w, err)
}

// isMetaNotFound reports a metadata read that definitely found no record, as
// opposed to one that could not be answered. The stores report a missing record
// with an error rather than a sentinel, in the same words locally and from a
// remote shard, so the text is the signal.
func isMetaNotFound(err error) bool {
	if err == nil || errors.Is(err, metadata.ErrShardUnavailable) {
		return false
	}
	return strings.Contains(err.Error(), "not found")
}

// newObject is one object write: the bytes and the metadata the object should
// carry.
//
// Every path that creates or replaces an object goes through writeObject, so
// versioning, object lock and SSE-C apply the same way whichever request asked.
// CopyObject, CompleteMultipartUpload, the POST form upload and the Snowball
// import used to write the plain object path with no version id and no lock
// check: on a versioned bucket they overwrote the current object in place
// instead of adding a version, and on any bucket they replaced a COMPLIANCE
// locked object.
type newObject struct {
	bucket, key string
	body        io.Reader
	size        int64
	// meta is the template: content type, tags, user and HTTP metadata and
	// checksums. Identity, size, ETag, time and version are filled in here.
	meta metadata.ObjectMeta
	// ssec seals the object with a customer key. Refused on a versioned bucket.
	ssec *sseCustomerKey
	// digests, when set, is the streaming verifier PutObject reads the body
	// through. Its checksums are recorded on the object.
	digests *putDigests
	// bypassGovernance is an authorized request to replace an object under
	// GOVERNANCE retention.
	bypassGovernance bool
	// lockFrom is the request whose x-amz-object-lock-* headers apply. The
	// bucket's default retention applies either way.
	lockFrom *http.Request
	// etag replaces the engine's ETag, for a multipart object whose ETag is
	// the digest of its parts' digests.
	etag string
}

// writeObject stores o as the new current object of its key, the way the
// bucket's versioning state requires, and records its metadata.
func (h *ObjectHandler) writeObject(o newObject) (metadata.ObjectMeta, error) {
	versioning, _ := h.store.GetBucketVersioning(o.bucket)
	versioned := versioning == "Enabled" || versioning == "Suspended"
	if o.ssec != nil && versioned {
		return metadata.ObjectMeta{}, &reqError{"NotImplemented", "SSE-C is not yet supported on versioned buckets", http.StatusNotImplemented}
	}

	// Enabled versioning adds a version and replaces nothing, which S3 allows
	// on a locked object. The other two states replace a version in place, so a
	// lock on it has to refuse the write, as a delete would be refused.
	if versioning != "Enabled" {
		if err := h.checkReplaceLock(o.bucket, o.key, versioning, o.bypassGovernance); err != nil {
			return metadata.ObjectMeta{}, err
		}
	}

	var versionID string
	var written int64
	var etag string
	var err error
	switch versioning {
	case "Enabled":
		versionID = generateVersionID()
		written, etag, err = h.engine.PutObjectVersion(o.bucket, o.key, versionID, o.body, o.size)
	case "Suspended":
		versionID = nullVersionID
		written, etag, err = h.engine.PutObjectVersion(o.bucket, o.key, versionID, o.body, o.size)
	default:
		if o.ssec != nil {
			// SSE-C seals with the customer key in the same chunked streaming
			// format the encrypting engines use, so the body flows to the engine
			// a chunk at a time. `written` is the plaintext length.
			written, etag, err = ssecSealStream(o.ssec, o.body, o.size, func(sealed io.Reader, storedSize int64) (int64, string, error) {
				return h.engine.PutObject(o.bucket, o.key, sealed, storedSize)
			})
		} else {
			written, etag, err = h.engine.PutObject(o.bucket, o.key, o.body, o.size)
		}
	}
	if err != nil {
		// A check made inside the body reader failed the engine's own write, so
		// the engine discarded its temp file and the object that was there
		// before is untouched. Answer with the check's own error.
		if o.digests != nil && o.digests.failure != nil {
			return metadata.ObjectMeta{}, o.digests.failure
		}
		var re *reqError
		if errors.As(err, &re) {
			return metadata.ObjectMeta{}, re
		}
		return metadata.ObjectMeta{}, err
	}

	meta := o.meta
	if o.digests != nil {
		sums, verr := h.settleDigests(o.bucket, o.key, versionID, o.digests)
		if verr != nil {
			return metadata.ObjectMeta{}, verr
		}
		meta.ChecksumSHA256, meta.ChecksumCRC32, meta.ChecksumCRC32C, meta.ChecksumSHA1 = sums.SHA256, sums.CRC32, sums.CRC32C, sums.SHA1
	}

	now := time.Now().UTC()
	meta.Bucket = o.bucket
	meta.Key = o.key
	meta.ETag = etag
	if o.etag != "" {
		meta.ETag = o.etag
	}
	meta.Size = written
	meta.LastModified = now.Unix()
	meta.VersionID = ""
	meta.IsLatest = false
	meta.DeleteMarker = false
	if meta.ContentType == "" {
		meta.ContentType = "application/octet-stream"
	}
	if o.ssec != nil {
		meta.SSECustomerKeyMD5 = o.ssec.keyMD5
	}
	h.applyObjectLock(o.lockFrom, &meta, o.bucket, now)

	if !versioned {
		if err := h.store.PutObjectMeta(meta); err != nil {
			return metadata.ObjectMeta{}, &metaWriteError{"PutObjectMeta", err}
		}
		if h.replicatePlacement != nil {
			h.replicatePlacement(o.bucket, o.key) // copy data to replica-set peers (issue #37)
		}
		return meta, nil
	}

	meta.VersionID = versionID
	meta.IsLatest = true
	replacedPlain, err := h.demoteCurrent(o.bucket, o.key, versionID)
	if err != nil {
		// Losing this write would leave two versions both claiming to be
		// latest, so the request fails rather than half-applying.
		return metadata.ObjectMeta{}, &metaWriteError{"demote previous version", err}
	}
	if err := h.store.PutObjectVersion(meta); err != nil {
		return metadata.ObjectMeta{}, &metaWriteError{"PutObjectVersion", err}
	}
	if err := h.store.PutObjectMeta(meta); err != nil { // update "latest pointer"
		return metadata.ObjectMeta{}, &metaWriteError{"PutObjectMeta", err}
	}
	if replacedPlain {
		// The object written before versioning was the null version and has just
		// been replaced by a new null version, which lives under .vs/. Its bytes
		// at the plain path are referenced by nothing any more.
		if err := h.engine.DeleteObject(o.bucket, o.key); err != nil {
			slog.Warn("could not remove the data of a replaced pre-versioning object",
				"bucket", o.bucket, "key", o.key, "error", err)
		}
		h.reapElsewhere(o.bucket, o.key, "")
	}
	return meta, nil
}

// demoteCurrent marks the key's current version as no longer the latest, before
// newVersionID takes its place.
//
// An object written before versioning was enabled carries no version id. It
// used to be skipped here, so the new version's latest pointer overwrote the
// only record of it and it was lost, with its bytes orphaned on disk. S3 calls
// it the "null" version, so it is adopted as one, exactly as writeDeleteMarker
// does. replacedPlain reports that the new version is itself the null version,
// which means such an object is being replaced rather than kept.
func (h *ObjectHandler) demoteCurrent(bucket, key, newVersionID string) (replacedPlain bool, err error) {
	old, gerr := h.store.GetObjectMeta(bucket, key)
	if gerr != nil || old == nil {
		return false, nil
	}
	if old.VersionID == "" {
		if old.DeleteMarker {
			return false, nil
		}
		if newVersionID == nullVersionID {
			return true, nil
		}
		old.VersionID = nullVersionID
	} else if old.VersionID == newVersionID {
		// The null version replaces its own record.
		return false, nil
	}
	old.IsLatest = false
	return false, h.store.PutObjectVersion(*old)
}

// checkReplaceLock refuses a write that would replace a locked version in place:
// the current object of an unversioned bucket, or the null version of a
// suspended one. Overwriting used to skip object lock entirely, so a COMPLIANCE
// locked object was replaced by any PUT.
func (h *ObjectHandler) checkReplaceLock(bucket, key, versioning string, bypassGovernance bool) error {
	versionID := ""
	if versioning == "Suspended" {
		versionID = nullVersionID
		// A null version written before versioning was turned on is still the
		// latest pointer with no version id, and that is where its lock lives.
		if cur, err := h.store.GetObjectMeta(bucket, key); err == nil && cur != nil && cur.VersionID == "" {
			versionID = ""
		}
	}
	return h.checkObjectLock(bucket, key, versionID, bypassGovernance)
}

// settleDigests finishes the streaming checks. They normally ran inside the
// body reader, where a failure stopped the engine before it replaced anything.
// An engine that stopped reading before the end of the body never let them run,
// so they run here instead, and the rejected bytes are removed the old way.
func (h *ObjectHandler) settleDigests(bucket, key, versionID string, d *putDigests) (objectChecksums, error) {
	if d.done {
		if d.failure != nil {
			return d.sums, d.failure
		}
		return d.sums, nil
	}
	if d.req == nil {
		return objectChecksums{}, nil
	}
	sums, code, message, ok := d.verify(d.req)
	if ok {
		return sums, nil
	}
	var err error
	if versionID != "" {
		err = h.engine.DeleteObjectVersion(bucket, key, versionID)
	} else {
		err = h.engine.DeleteObject(bucket, key)
	}
	if err != nil {
		slog.Warn("could not remove the data of a rejected upload; it has no metadata so it is not served, run `vaults3-cli object verify --repair` to reclaim it",
			"bucket", bucket, "key", key, "version", versionID, "error", err)
	}
	return sums, &reqError{code, message, http.StatusBadRequest}
}

// quotaByteLimit is how many bytes an upload to bucket may add before it takes
// the bucket over its size quota, or -1 when nothing limits it. FIFO buckets
// make room instead of refusing, so they have no limit here.
func (h *ObjectHandler) quotaByteLimit(bucket string) int64 {
	info, err := h.store.GetBucket(bucket)
	if err != nil || info.MaxSizeBytes == 0 || info.FIFOQuota {
		return -1
	}
	currentSize, _, _ := h.engine.BucketSize(bucket)
	return info.MaxSizeBytes - currentSize
}
