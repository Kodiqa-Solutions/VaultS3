package s3

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// traceReads, when set via VAULTS3_TRACE_READS=1, logs the cause of every GET/HEAD
// 404 in cluster mode (metadata-missing vs data-missing) plus whether the request
// was proxied here and by which node. This distinguishes a metadata replication lag
// (which the consistent read waits out) from a request being served by a node that
// isn't the data owner (a routing/ownership problem the read path can't fix), for
// diagnosing issue #37. Off by default, zero overhead.
var traceReads = os.Getenv("VAULTS3_TRACE_READS") == "1"

// traceRead404 logs a read 404 with its cause when read tracing is enabled.
func traceRead404(r *http.Request, method, bucket, key, cause string) {
	if !traceReads {
		return
	}
	slog.Warn("read 404",
		"method", method,
		"bucket", bucket,
		"key", key,
		"cause", cause,
		"proxied_from", r.Header.Get("X-VaultS3-Proxy"),
	)
}

type ObjectHandler struct {
	// auth authorizes individual entries of a multi-object request, which the
	// router cannot do because it decides before the body is parsed.
	auth *Authenticator

	store metadata.StoreAPI
	// mpStore holds in-progress multipart upload metadata. In a cluster this is the
	// node-LOCAL store, not the Raft-replicated one: every request for an object
	// routes to the same owner node and its part data is written to that node's
	// local disk, so replicating the metadata through Raft only added a
	// read-after-write lag that returned 404 NoSuchUpload for a part uploaded right
	// after CreateMultipartUpload on a follower (issue #32). Defaults to store.
	mpStore metadata.StoreAPI
	// multipartHolder, if set (cluster mode), forwards a request naming an upload
	// this node has no record of to the node that does. In-progress multipart state
	// is node-local (issue #32) while these requests route by object key, so any
	// change to the hash ring strands an upload on its creating node: it is still
	// listed, but abort and ListParts route elsewhere and answer NoSuchUpload
	// forever, leaving parts on disk that nothing can reclaim (issue #47 bug B).
	// Returns true when it handled the request.
	multipartHolder func(w http.ResponseWriter, r *http.Request, uploadID string) bool
	// multipartPeers, if set (cluster mode), returns the in-progress uploads the
	// OTHER nodes hold for a bucket. ListMultipartUploads is a bucket-level request
	// routed to a single node, so on its own it only ever sees the uploads whose key
	// happens to hash to that node, roughly 1/N of them; the rest were invisible and
	// their parts unreclaimable (issue #47 bug B).
	multipartPeers    func(bucket string) []metadata.MultipartUpload
	engine            storage.Engine
	encryptionEnabled bool
	// reapReplicas, if set (cluster mode), removes an object's data file from every
	// OTHER node after a delete. Writes land on a single node, but a ring/primary
	// change can leave an orphan copy elsewhere; without reaping it lingers on disk
	// (issue #34 layer 2). Best-effort and asynchronous — correctness already comes
	// from metadata being authoritative (layer 1), this just reclaims disk.
	//
	// EVERY path that removes object data must call this, not just the single-object
	// DELETE. A bucket-level request like the multi-object delete is routed by
	// hash(bucket, "") to one node, so its local engine holds only that node's share
	// of the keys; deleting there while the metadata goes cluster-wide through Raft
	// orphaned (N-1)/N of the bytes with no way left to reach them (issue #47).
	// versionID is empty for a plain (non-versioned) object.
	reapReplicas func(bucket, key, versionID string)
	// reapReplicasBatch is the multi-key form, used by the multi-object delete. A
	// Spark-style job deletes keys a thousand at a time, and reaping those one at a
	// time would be peers*keys separate requests, so this sends one request per peer
	// carrying the whole key list.
	reapReplicasBatch func(bucket string, keys []string)
	// replicatePlacement, if set (cluster mode with replica_count > 1), copies a
	// just-written object's data to the other nodes in its replica set so a node
	// loss doesn't make it unavailable (issue #37). Best-effort + asynchronous —
	// never blocks or fails the client write; GET failover already tries replicas.
	replicatePlacement func(bucket, key string)
	// dataHolderFallback, if set (cluster mode), re-routes a read this node has
	// metadata for but no readable data to a peer that holds the object's bytes.
	// That gap is normal and transient while replicatePlacement above is still in
	// flight, and without this a GET on the wrong holder 404s an object that was
	// just written successfully (issue #42).
	dataHolderFallback DataHolderFallbackFunc
	onNotification     NotificationFunc
	onReplication      ReplicationFunc
	onScan             ScanFunc
	onSearchUpdate     SearchUpdateFunc
	onLambda           LambdaFunc
	accessUpdater      *metadata.AccessUpdater
}

// serveFromDataHolder asks a peer holder to serve a read this node has metadata
// for but no readable data.
//
// It returns true once the response belongs to it — either a peer served the
// object, or no holder could be reached and the client was told to retry. A false
// return means the object's data is genuinely missing cluster-wide and the caller
// should report it not-found. Always false outside a cluster.
func (h *ObjectHandler) serveFromDataHolder(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
	if h.dataHolderFallback == nil {
		return false
	}
	served, unreachable := h.dataHolderFallback(w, r, bucket, key)
	if served {
		return true
	}
	if unreachable {
		// The object exists and a holder has its data, but that node is currently
		// unreachable (restarting, rescheduled, briefly overloaded). Answering
		// "not found" here would be a lie the client cannot recover from, so say
		// "try again" instead, which every S3 SDK retries on its own (issue #42).
		slog.Warn("object data temporarily unavailable: holder unreachable, asking client to retry",
			"bucket", bucket, "key", key)
		writeS3Error(w, "SlowDown", "Object data is temporarily unavailable, please retry", http.StatusServiceUnavailable)
		return true
	}
	return false
}

// reapElsewhere removes this object's data file from every OTHER node after the
// local engine deleted its own copy. In a cluster the node serving a delete holds
// the data only when it happens to be the key's hash owner: a bucket-level request
// (the multi-object delete) is routed by hash(bucket, "") and a background sweep
// runs wherever it runs, so without this the bytes on the (N-1) other nodes are
// orphaned the instant the Raft-replicated metadata goes away, and nothing can
// reach them again (issue #47). Best-effort and asynchronous, exactly like the
// original single-object reaper (issue #34 layer 2): correctness comes from
// metadata being authoritative, this only reclaims disk. No-op single-node.
func (h *ObjectHandler) reapElsewhere(bucket, key, versionID string) {
	if h.reapReplicas != nil {
		h.reapReplicas(bucket, key, versionID)
	}
}

// multipartStore returns the store used for in-progress multipart upload
// metadata (node-local in a cluster; see the mpStore field). Falls back to the
// main store when not separately configured.
func (h *ObjectHandler) multipartStore() metadata.StoreAPI {
	if h.mpStore != nil {
		return h.mpStore
	}
	return h.store
}

// checkQuota verifies bucket quota limits before writing.
// If FIFOQuota is enabled, oldest objects are deleted to make room.
func (h *ObjectHandler) checkQuota(w http.ResponseWriter, bucket string, incomingSize int64) bool {
	info, err := h.store.GetBucket(bucket)
	if err != nil {
		return true // no bucket info, allow
	}
	if info.MaxSizeBytes == 0 && info.MaxObjects == 0 {
		return true // no limits
	}

	currentSize, currentCount, _ := h.engine.BucketSize(bucket)

	if info.FIFOQuota {
		// FIFO: delete oldest objects to make room
		if info.MaxObjects > 0 && currentCount >= info.MaxObjects {
			h.fifoEvict(bucket, 1, 0)
		}
		if info.MaxSizeBytes > 0 && incomingSize > 0 && currentSize+incomingSize > info.MaxSizeBytes {
			needed := currentSize + incomingSize - info.MaxSizeBytes
			h.fifoEvict(bucket, 0, needed)
		}
		return true
	}

	if info.MaxObjects > 0 && currentCount >= info.MaxObjects {
		writeS3Error(w, "QuotaExceeded", "Maximum object count exceeded", http.StatusForbidden)
		return false
	}
	if info.MaxSizeBytes > 0 && incomingSize > 0 && currentSize+incomingSize > info.MaxSizeBytes {
		writeS3Error(w, "QuotaExceeded", "Maximum bucket size exceeded", http.StatusForbidden)
		return false
	}

	return true
}

// fifoEvict deletes oldest objects until count or size requirements are met.
func (h *ObjectHandler) fifoEvict(bucket string, countToFree int64, bytesToFree int64) {
	objects, _, err := h.engine.ListObjects(bucket, "", "", 10000)
	if err != nil || len(objects) == 0 {
		return
	}

	// Objects from ListObjects are typically in alphabetical order.
	// Sort by modified time to find oldest.
	type objMeta struct {
		key  string
		size int64
		mod  time.Time
	}
	var metas []objMeta
	for _, obj := range objects {
		meta, err := h.store.GetObjectMeta(bucket, obj.Key)
		if err != nil {
			continue
		}
		metas = append(metas, objMeta{key: obj.Key, size: meta.Size, mod: time.Unix(0, meta.LastModified)})
	}
	// Sort oldest first
	for i := 0; i < len(metas); i++ {
		for j := i + 1; j < len(metas); j++ {
			if metas[j].mod.Before(metas[i].mod) {
				metas[i], metas[j] = metas[j], metas[i]
			}
		}
	}

	var freedCount int64
	var freedBytes int64
	for _, m := range metas {
		if countToFree > 0 && freedCount >= countToFree && bytesToFree <= 0 {
			break
		}
		if bytesToFree > 0 && freedBytes >= bytesToFree && countToFree <= 0 {
			break
		}
		if countToFree > 0 && freedCount >= countToFree && bytesToFree > 0 && freedBytes >= bytesToFree {
			break
		}

		h.engine.DeleteObject(bucket, m.key)
		h.store.DeleteObjectMeta(bucket, m.key)
		freedCount++
		freedBytes += m.size

		if h.onSearchUpdate != nil {
			h.onSearchUpdate("delete", bucket, m.key)
		}
	}
}

// generateVersionID creates a unique version ID using timestamp + random bytes.
func generateVersionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return fmt.Sprintf("%016x%s", time.Now().UnixNano(), hex.EncodeToString(b[:4]))
}

// detectContentType determines the content type for an object.
func detectContentType(r *http.Request, key string) string {
	ct := r.Header.Get("Content-Type")
	if ct == "" || ct == "application/octet-stream" {
		if detected := mime.TypeByExtension(filepath.Ext(key)); detected != "" {
			return detected
		}
		return "application/octet-stream"
	}
	return ct
}

// settleUpload validates a streamed upload after its bytes have been written and
// undoes the write if the client's promise was not kept.
//
// Streaming means validation can no longer happen before the write (issue #46),
// so a rejected upload is removed here. No metadata has been stored at this
// point, and metadata is authoritative (issue #34), so the object is invisible
// either way; deleting the bytes keeps the disk honest as well. versionID is ""
// for the non-versioned path.
//
// Returns false once it has written the error response, meaning the caller must
// stop.
func (h *ObjectHandler) settleUpload(w http.ResponseWriter, r *http.Request, bucket, key, versionID string, d *putDigests) (objectChecksums, bool) {
	discard := func() {
		var err error
		if versionID != "" {
			err = h.engine.DeleteObjectVersion(bucket, key, versionID)
		} else {
			err = h.engine.DeleteObject(bucket, key)
		}
		if err != nil {
			// Not fatal: without metadata the bytes are unreachable over S3, and
			// `vaults3-cli object verify` finds them. Say so rather than hide it.
			slog.Warn("could not remove the data of a rejected upload; it has no metadata so it is not served, run `vaults3-cli object verify --repair` to reclaim it",
				"bucket", bucket, "key", key, "version", versionID, "error", err)
		}
	}

	sums, code, message, ok := d.verify(r)
	if !ok {
		discard()
		status := http.StatusBadRequest
		writeS3Error(w, code, message, status)
		return sums, false
	}

	// The pre-write quota check used the declared Content-Length, which an
	// aws-chunked client controls via X-Amz-Decoded-Content-Length. Re-check
	// against the length that actually arrived so a bucket quota cannot be
	// undercut by a false declared length.
	if d.size() > r.ContentLength && !h.quotaAllowsAfterWrite(bucket) {
		discard()
		writeS3Error(w, "QuotaExceeded", "Maximum bucket size exceeded", http.StatusForbidden)
		return sums, false
	}
	return sums, true
}

// quotaAllowsAfterWrite reports whether a bucket is still inside its limits once
// a streamed upload has landed. It differs from checkQuota in being a post-write
// check: the object is already counted in the engine's totals, so the comparison
// is against the totals themselves rather than totals-plus-incoming. FIFO buckets
// make room instead of rejecting, so they always pass.
func (h *ObjectHandler) quotaAllowsAfterWrite(bucket string) bool {
	info, err := h.store.GetBucket(bucket)
	if err != nil || (info.MaxSizeBytes == 0 && info.MaxObjects == 0) || info.FIFOQuota {
		return true
	}
	currentSize, currentCount, _ := h.engine.BucketSize(bucket)
	if info.MaxObjects > 0 && currentCount > info.MaxObjects {
		return false
	}
	if info.MaxSizeBytes > 0 && currentSize > info.MaxSizeBytes {
		return false
	}
	return true
}

// PutObject handles PUT /{bucket}/{key}.
func (h *ObjectHandler) PutObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	// Snowball/TAR auto-extract
	if strings.EqualFold(r.Header.Get("X-Amz-Meta-Snowball-Auto-Extract"), "true") {
		h.SnowballUpload(w, r, bucket)
		return
	}

	// Enforce max single object size (5GB, per S3 spec)
	const maxPutSize int64 = 5 * 1024 * 1024 * 1024 // 5GB
	if r.ContentLength > maxPutSize {
		writeS3Error(w, "EntityTooLarge", "Object size exceeds 5GB limit. Use multipart upload for larger files.", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPutSize)

	if !h.checkQuota(w, bucket, r.ContentLength) {
		return
	}

	// Conditional PUT: check If-Match / If-None-Match. When a conditional header
	// is present, hold the per-key lock across the check and the subsequent write
	// so the compare-and-swap is atomic — two concurrent `If-None-Match: *` PUTs
	// to the same key must not both succeed.
	if r.Header.Get("If-Match") != "" || r.Header.Get("If-None-Match") != "" {
		unlock := lockObjectKey(bucket, key)
		defer unlock()
	}
	if checkPutPreconditions(w, r, h.store, bucket, key) {
		return
	}

	versioning, _ := h.store.GetBucketVersioning(bucket)
	ct := detectContentType(r, key)
	now := time.Now().UTC()

	// Parse extended metadata from headers
	userMeta := parseUserMetadata(r)
	tags := parseInlineTags(r)

	// SSE-C (customer-provided keys). Supported on the non-versioned path for now.
	ssecKey, ssecErr := parseSSECHeaders(r)
	if ssecErr != nil {
		writeS3Error(w, "InvalidArgument", ssecErr.Error(), http.StatusBadRequest)
		return
	}
	if ssecKey != nil && (versioning == "Enabled" || versioning == "Suspended") {
		writeS3Error(w, "NotImplemented", "SSE-C is not yet supported on versioned buckets", http.StatusNotImplemented)
		return
	}

	// The body streams to the engine while its digests are computed in passing,
	// so a large upload costs a copy buffer rather than its whole size in memory
	// (issue #46). Validation therefore happens AFTER the bytes are written, and a
	// rejected upload is undone below. That is safe because metadata is written
	// only after validation and metadata is authoritative (issue #34): an object
	// whose bytes exist without metadata is invisible to every API, and
	// `vaults3-cli object verify` reports it.
	digests := newPutDigests(r, r.Body)

	if versioning == "Enabled" {
		versionID := generateVersionID()

		written, etag, err := h.engine.PutObjectVersion(bucket, key, versionID, digests, r.ContentLength)
		if err != nil {
			slog.Error("internal error", "error", err)
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
			return
		}
		sums, ok := h.settleUpload(w, r, bucket, key, versionID, digests)
		if !ok {
			return
		}
		csha256, ccrc32, ccrc32c, csha1 := sums.SHA256, sums.CRC32, sums.CRC32C, sums.SHA1

		// Mark previous latest as not latest
		if oldMeta, err := h.store.GetObjectMeta(bucket, key); err == nil && oldMeta.VersionID != "" {
			oldMeta.IsLatest = false
			if err := h.store.PutObjectVersion(*oldMeta); err != nil {
				// Losing this write would leave two versions both claiming to be
				// latest, so the request fails rather than half-applying.
				metaWriteFailed(w, err, "demote previous version", bucket, key)
				return
			}
		}

		meta := metadata.ObjectMeta{
			Bucket:             bucket,
			Key:                key,
			ContentType:        ct,
			ETag:               etag,
			Size:               written,
			LastModified:       now.Unix(),
			VersionID:          versionID,
			IsLatest:           true,
			Tags:               tags,
			UserMetadata:       userMeta,
			ContentEncoding:    r.Header.Get("Content-Encoding"),
			ContentDisposition: r.Header.Get("Content-Disposition"),
			CacheControl:       r.Header.Get("Cache-Control"),
			ContentLanguage:    r.Header.Get("Content-Language"),
			WebsiteRedirect:    r.Header.Get("X-Amz-Website-Redirect-Location"),
			ChecksumSHA256:     csha256,
			ChecksumCRC32:      ccrc32,
			ChecksumCRC32C:     ccrc32c,
			ChecksumSHA1:       csha1,
		}

		h.applyObjectLock(r, &meta, bucket, now)

		if err := h.store.PutObjectVersion(meta); err != nil {
			metaWriteFailed(w, err, "PutObjectVersion", bucket, key)
			return
		}
		if err := h.store.PutObjectMeta(meta); err != nil { // update "latest pointer"
			metaWriteFailed(w, err, "PutObjectMeta", bucket, key)
			return
		}

		w.Header().Set("ETag", etag)
		w.Header().Set("X-Amz-Version-Id", versionID)
		if h.encryptionEnabled {
			w.Header().Set("X-Amz-Server-Side-Encryption", "AES256")
		}
		setChecksumHeaders(w, &meta)
		w.WriteHeader(http.StatusOK)
		if h.onNotification != nil {
			h.onNotification("s3:ObjectCreated:Put", bucket, key, written, etag, versionID)
		}
		if h.onReplication != nil {
			h.onReplication("s3:ObjectCreated:Put", bucket, key, written, etag, versionID)
		}
		if h.onLambda != nil {
			h.onLambda("s3:ObjectCreated:Put", bucket, key, written, etag, versionID)
		}
		if h.onScan != nil {
			h.onScan(bucket, key, written)
		}
		if h.onSearchUpdate != nil {
			h.onSearchUpdate("put", bucket, key)
		}
		return
	}

	if versioning == "Suspended" {
		// Suspended versioning: overwrite the "null" version
		written, etag, err := h.engine.PutObjectVersion(bucket, key, "null", digests, r.ContentLength)
		if err != nil {
			slog.Error("internal error", "error", err)
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
			return
		}
		sums, ok := h.settleUpload(w, r, bucket, key, "null", digests)
		if !ok {
			return
		}
		csha256, ccrc32, ccrc32c, csha1 := sums.SHA256, sums.CRC32, sums.CRC32C, sums.SHA1

		// Remove any existing null version
		if oldMeta, err := h.store.GetObjectVersion(bucket, key, "null"); err == nil {
			oldMeta.IsLatest = false
			if err := h.store.PutObjectVersion(*oldMeta); err != nil {
				metaWriteFailed(w, err, "demote null version", bucket, key)
				return
			}
		}

		meta := metadata.ObjectMeta{
			Bucket:             bucket,
			Key:                key,
			ContentType:        ct,
			ETag:               etag,
			Size:               written,
			LastModified:       now.Unix(),
			VersionID:          "null",
			IsLatest:           true,
			Tags:               tags,
			UserMetadata:       userMeta,
			ContentEncoding:    r.Header.Get("Content-Encoding"),
			ContentDisposition: r.Header.Get("Content-Disposition"),
			CacheControl:       r.Header.Get("Cache-Control"),
			ContentLanguage:    r.Header.Get("Content-Language"),
			WebsiteRedirect:    r.Header.Get("X-Amz-Website-Redirect-Location"),
			ChecksumSHA256:     csha256,
			ChecksumCRC32:      ccrc32,
			ChecksumCRC32C:     ccrc32c,
			ChecksumSHA1:       csha1,
		}
		h.applyObjectLock(r, &meta, bucket, now)

		if err := h.store.PutObjectVersion(meta); err != nil {
			metaWriteFailed(w, err, "PutObjectVersion", bucket, key)
			return
		}
		if err := h.store.PutObjectMeta(meta); err != nil {
			metaWriteFailed(w, err, "PutObjectMeta", bucket, key)
			return
		}

		w.Header().Set("ETag", etag)
		w.Header().Set("X-Amz-Version-Id", "null")
		if h.encryptionEnabled {
			w.Header().Set("X-Amz-Server-Side-Encryption", "AES256")
		}
		setChecksumHeaders(w, &meta)
		w.WriteHeader(http.StatusOK)
		if h.onNotification != nil {
			h.onNotification("s3:ObjectCreated:Put", bucket, key, written, etag, "null")
		}
		if h.onReplication != nil {
			h.onReplication("s3:ObjectCreated:Put", bucket, key, written, etag, "null")
		}
		if h.onLambda != nil {
			h.onLambda("s3:ObjectCreated:Put", bucket, key, written, etag, "null")
		}
		if h.onScan != nil {
			h.onScan(bucket, key, written)
		}
		if h.onSearchUpdate != nil {
			h.onSearchUpdate("put", bucket, key)
		}
		return
	}

	// Non-versioned path.
	var written int64
	var etag string
	var err error
	var plainSize int64
	if ssecKey != nil {
		// SSE-C still seals the object as one AEAD message and so still buffers it.
		// That is now a gap rather than a necessity: the chunked format in
		// internal/storage/streamcrypt.go would bound this too, but SSE-C keys are
		// per-request and the stored format would need the same read-both-formats
		// migration, so it is left for its own change. Memory here scales with
		// object size times concurrent SSE-C requests (the shape of issue #49).
		body, rerr := io.ReadAll(digests)
		if rerr != nil {
			writeS3Error(w, "InternalError", "Failed to read request body", http.StatusInternalServerError)
			return
		}
		plainSize = int64(len(body))
		sealed, serr := ssecSeal(ssecKey, body)
		if serr != nil {
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
			return
		}
		written, etag, err = h.engine.PutObject(bucket, key, bytes.NewReader(sealed), int64(len(sealed)))
	} else {
		written, etag, err = h.engine.PutObject(bucket, key, digests, r.ContentLength)
	}
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	sums, ok := h.settleUpload(w, r, bucket, key, "", digests)
	if !ok {
		return
	}
	csha256, ccrc32, ccrc32c, csha1 := sums.SHA256, sums.CRC32, sums.CRC32C, sums.SHA1
	if ssecKey != nil {
		written = plainSize // report the plaintext size, not the SSE-C ciphertext size
	}

	meta := metadata.ObjectMeta{
		Bucket:             bucket,
		Key:                key,
		ContentType:        ct,
		ETag:               etag,
		Size:               written,
		LastModified:       now.Unix(),
		Tags:               tags,
		UserMetadata:       userMeta,
		ContentEncoding:    r.Header.Get("Content-Encoding"),
		ContentDisposition: r.Header.Get("Content-Disposition"),
		CacheControl:       r.Header.Get("Cache-Control"),
		ContentLanguage:    r.Header.Get("Content-Language"),
		WebsiteRedirect:    r.Header.Get("X-Amz-Website-Redirect-Location"),
		ChecksumSHA256:     csha256,
		ChecksumCRC32:      ccrc32,
		ChecksumCRC32C:     ccrc32c,
		ChecksumSHA1:       csha1,
	}
	if ssecKey != nil {
		meta.SSECustomerKeyMD5 = ssecKey.keyMD5
	}
	h.applyObjectLock(r, &meta, bucket, now)

	if err := h.store.PutObjectMeta(meta); err != nil {
		metaWriteFailed(w, err, "PutObjectMeta", bucket, key)
		return
	}
	if h.replicatePlacement != nil {
		h.replicatePlacement(bucket, key) // copy data to replica-set peers (issue #37)
	}

	w.Header().Set("ETag", etag)
	if ssecKey != nil {
		w.Header().Set(hdrSSECAlgo, "AES256")
		w.Header().Set(hdrSSECKeyMD5, ssecKey.keyMD5)
	} else if h.encryptionEnabled {
		w.Header().Set("X-Amz-Server-Side-Encryption", "AES256")
	}
	setChecksumHeaders(w, &meta)
	w.WriteHeader(http.StatusOK)
	if h.onNotification != nil {
		h.onNotification("s3:ObjectCreated:Put", bucket, key, written, etag, "")
	}
	if h.onReplication != nil {
		h.onReplication("s3:ObjectCreated:Put", bucket, key, written, etag, "")
	}
	if h.onLambda != nil {
		h.onLambda("s3:ObjectCreated:Put", bucket, key, written, etag, "")
	}
	if h.onScan != nil {
		h.onScan(bucket, key, written)
	}
	if h.onSearchUpdate != nil {
		h.onSearchUpdate("put", bucket, key)
	}
}

// GetObject handles GET /{bucket}/{key} with optional Range support and ?versionId.
func (h *ObjectHandler) GetObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	versionID := r.URL.Query().Get("versionId")

	var reader storage.ReadSeekCloser
	var size int64
	var meta *metadata.ObjectMeta
	var err error

	if versionID != "" {
		// Get specific version
		meta, err = h.store.GetObjectVersion(bucket, key, versionID)
		if err != nil {
			if metadataUnavailable(w, err) {
				return
			}
			writeS3Error(w, "NoSuchVersion", "Version not found", http.StatusNotFound)
			return
		}
		if meta.DeleteMarker {
			w.Header().Set("X-Amz-Delete-Marker", "true")
			w.Header().Set("X-Amz-Version-Id", versionID)
			writeS3Error(w, "NoSuchKey", "Object is a delete marker", http.StatusNotFound)
			return
		}
		reader, size, err = h.engine.GetObjectVersion(bucket, key, versionID)
		if err != nil {
			// The version's metadata is here but its bytes may live on another
			// holder that has not been copied to yet (issue #42).
			if h.serveFromDataHolder(w, r, bucket, key) {
				return
			}
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Amz-Version-Id", versionID)
	} else {
		// Get latest version. Consistent read: barrier-on-miss so a GET right after
		// a PUT on another cluster node doesn't spuriously 404 (issue #37).
		var metaErr error
		meta, metaErr = h.store.GetObjectMetaConsistent(bucket, key)
		if meta == nil && metadataUnavailable(w, metaErr) {
			return
		}
		if meta != nil && meta.DeleteMarker {
			w.Header().Set("X-Amz-Delete-Marker", "true")
			if meta.VersionID != "" {
				w.Header().Set("X-Amz-Version-Id", meta.VersionID)
			}
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}

		if meta == nil {
			// Metadata is authoritative: a deleted object is gone even if a data
			// file lingers on a replica node, so don't serve phantom bytes from the
			// engine (issue #34, same root cause as the phantom HEAD).
			traceRead404(r, "GET", bucket, key, "meta_nil")
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}
		if meta.VersionID != "" {
			// Versioned bucket — read from version storage
			reader, size, err = h.engine.GetObjectVersion(bucket, key, meta.VersionID)
			w.Header().Set("X-Amz-Version-Id", meta.VersionID)
		} else {
			reader, size, err = h.engine.GetObject(bucket, key)
		}
		if err != nil {
			// Metadata says the object exists but this node cannot read its data.
			// In a cluster that is usually not corruption at all: the object was
			// written on another holder and its bytes have not been copied here yet,
			// so ask a holder that does have them before giving up (issue #42).
			if h.serveFromDataHolder(w, r, bucket, key) {
				return
			}
			// No peer could serve it either, so this is a genuine desync: the object
			// appears in listings but cannot be downloaded over S3 (issue #40). Log it
			// loudly so operators can find and reconcile it with
			// `vaults3-cli object verify [--repair]`.
			slog.Warn("object metadata/data desync: metadata present but data is unreadable, object lists but cannot be served over S3",
				"bucket", bucket, "key", key, "error", err)
			traceRead404(r, "GET", bucket, key, "data_missing")
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}
	}
	defer reader.Close()

	// SSE-C: object was encrypted with a customer-provided key. Require + verify it,
	// then decrypt into an in-memory reader so range/part logic runs on plaintext.
	// This buffers the whole object; see the note on the SSE-C write path.
	if meta != nil && meta.SSECustomerKeyMD5 != "" {
		ssecKey, perr := parseSSECHeaders(r)
		if perr != nil || ssecKey == nil {
			writeS3Error(w, "InvalidArgument", "object is SSE-C encrypted; a customer key is required", http.StatusBadRequest)
			return
		}
		if ssecKey.keyMD5 != meta.SSECustomerKeyMD5 {
			writeS3Error(w, "AccessDenied", "SSE-C key does not match the one used to encrypt this object", http.StatusForbidden)
			return
		}
		sealed, rerr := io.ReadAll(reader)
		if rerr != nil {
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
			return
		}
		plain, derr := ssecOpen(ssecKey, sealed)
		if derr != nil {
			writeS3Error(w, "AccessDenied", "SSE-C decryption failed", http.StatusForbidden)
			return
		}
		reader = ssecReader{bytes.NewReader(plain)}
		size = int64(len(plain))
	}

	// Conditional GET: check preconditions before sending body
	if checkGetPreconditions(w, r, meta) {
		return
	}

	// A whole-object checksum must not be sent on a partial (206) response: modern
	// SDKs (boto3 >= 1.36, aws-cli v2) validate x-amz-checksum-* against the bytes
	// they actually receive, and a whole-object checksum never matches a range or a
	// single part, so range downloads would fail with a checksum mismatch.
	isPartial := r.Header.Get("Range") != "" || r.URL.Query().Get("partNumber") != ""
	if meta != nil {
		w.Header().Set("Content-Type", meta.ContentType)
		w.Header().Set("ETag", meta.ETag)
		w.Header().Set("Last-Modified", time.Unix(meta.LastModified, 0).UTC().Format(http.TimeFormat))
		setHTTPMetadataHeaders(w, meta)
		setUserMetadataHeaders(w, meta)
		if !isPartial {
			setChecksumHeaders(w, meta)
		}
		if meta.PartsCount > 0 {
			w.Header().Set("X-Amz-Mp-Parts-Count", strconv.Itoa(meta.PartsCount))
		}
	}
	w.Header().Set("Accept-Ranges", "bytes")
	if h.encryptionEnabled {
		w.Header().Set("X-Amz-Server-Side-Encryption", "AES256")
	}

	// Apply response header overrides from query params
	applyResponseOverrides(w, r)

	// Track last access time for tiering
	if meta != nil {
		if h.accessUpdater != nil {
			h.accessUpdater.MarkAccess(bucket, key)
		} else {
			go h.store.UpdateLastAccess(bucket, key)
		}
	}

	// GetObject by part number: ?partNumber=N
	if pn := r.URL.Query().Get("partNumber"); pn != "" {
		partNum, err := strconv.Atoi(pn)
		if err != nil || partNum < 1 {
			writeS3Error(w, "InvalidArgument", "Invalid partNumber", http.StatusBadRequest)
			return
		}
		if meta == nil || meta.PartsCount == 0 || len(meta.PartBoundaries) == 0 {
			writeS3Error(w, "InvalidArgument", "Object is not a multipart upload", http.StatusBadRequest)
			return
		}
		if partNum > meta.PartsCount {
			writeS3Error(w, "InvalidArgument", "partNumber exceeds total parts", http.StatusBadRequest)
			return
		}
		var partStart int64
		if partNum > 1 {
			partStart = meta.PartBoundaries[partNum-2]
		}
		partEnd := meta.PartBoundaries[partNum-1] - 1
		partLen := partEnd - partStart + 1

		if _, err := reader.Seek(partStart, io.SeekStart); err != nil {
			writeS3Error(w, "InternalError", "Seek failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", partStart, partEnd, size))
		w.Header().Set("Content-Length", strconv.FormatInt(partLen, 10))
		w.WriteHeader(http.StatusPartialContent)
		io.CopyN(w, reader, partLen)
		return
	}

	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		h.serveRange(w, reader, size, rangeHeader)
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}

// serveRange handles partial content responses.
func (h *ObjectHandler) serveRange(w http.ResponseWriter, reader storage.ReadSeekCloser, totalSize int64, rangeHeader string) {
	// Parse "bytes=START-END"
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		writeS3Error(w, "InvalidRange", "Invalid Range header", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	spec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		writeS3Error(w, "InvalidRange", "Invalid Range header", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	var start, end int64

	if parts[0] == "" {
		// Suffix range: bytes=-500 (last 500 bytes)
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			writeS3Error(w, "InvalidRange", "Invalid Range header", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start = totalSize - suffix
		if start < 0 {
			start = 0
		}
		end = totalSize - 1
	} else {
		var err error
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 {
			writeS3Error(w, "InvalidRange", "Invalid Range header", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if parts[1] == "" {
			// Open-ended: bytes=500-
			end = totalSize - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				writeS3Error(w, "InvalidRange", "Invalid Range header", http.StatusRequestedRangeNotSatisfiable)
				return
			}
		}
	}

	if start > end || start >= totalSize {
		writeS3Error(w, "InvalidRange", "Range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= totalSize {
		end = totalSize - 1
	}

	length := end - start + 1

	if _, err := reader.Seek(start, io.SeekStart); err != nil {
		writeS3Error(w, "InternalError", "Seek failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	io.CopyN(w, reader, length)
}

// DeleteObject handles DELETE /{bucket}/{key}.
func (h *ObjectHandler) DeleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	versionID := r.URL.Query().Get("versionId")
	versioning, _ := h.store.GetBucketVersioning(bucket)
	bypassGov := strings.EqualFold(r.Header.Get("X-Amz-Bypass-Governance-Retention"), "true")

	del, err := h.deleteOneObject(bucket, key, versionID, versioning, bypassGov)
	if err != nil {
		var refused *deleteRefused
		if errors.As(err, &refused) {
			writeS3Error(w, "AccessDenied", refused.Error(), http.StatusForbidden)
			return
		}
		var notRecorded *deleteNotRecorded
		if errors.As(err, &notRecorded) {
			metaWriteFailed(w, notRecorded.err, notRecorded.op, bucket, key)
			return
		}
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	if del.Reap {
		h.reapElsewhere(bucket, key, del.ReapVersion)
	}
	if del.DeleteMarker {
		w.Header().Set("X-Amz-Delete-Marker", "true")
	}
	if del.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", del.VersionID)
	}
	w.WriteHeader(http.StatusNoContent)
	h.notifyDeleted(bucket, key, del.VersionID)
}

// One key's delete, applied the same way whichever request asked for it.
//
// DeleteObject and BatchDelete used to decide independently what a delete means,
// and they disagreed: the multi-object path removed the data and the metadata
// unconditionally, so on a versioning-enabled bucket it DESTROYED the object
// where a single delete would have written a delete marker and kept every
// version. That is silent data loss on the operation Spark and Hadoop S3A use to
// clean up, and it is invisible until someone asks for an old version. Both
// paths now go through deleteOneObject so they cannot drift again.

// objectDeletion says what a delete actually did, so the caller can answer with
// the right headers or the right entry in a multi-object result.
type objectDeletion struct {
	// VersionID is the version removed, or the delete marker written. Empty on a
	// non-versioned bucket, where a delete names no version.
	VersionID string
	// DeleteMarker reports that a marker was written rather than data removed.
	DeleteMarker bool
	// Reap reports that a data file was removed here, so the copies other nodes
	// hold have to go too (issue #47). ReapVersion is the version to remove.
	Reap        bool
	ReapVersion string
}

// deleteRefused means object lock refused the delete. It is a permanent answer,
// not something to retry.
type deleteRefused struct{ err error }

func (e *deleteRefused) Error() string { return e.err.Error() }
func (e *deleteRefused) Unwrap() error { return e.err }

// deleteNotRecorded means a metadata write failed. The bytes may already be
// gone, so the caller must report the delete as UNSUCCESSFUL: metadata is
// authoritative (issue #34), and a delete reported as done while the object
// still lists is the failure mode issue #50 P0 closed on the write path.
type deleteNotRecorded struct {
	op  string
	err error
}

func (e *deleteNotRecorded) Error() string { return e.op + ": " + e.err.Error() }
func (e *deleteNotRecorded) Unwrap() error { return e.err }

// deleteOneObject applies the versioning-correct delete of one key.
//
// versionID names a specific version to remove permanently; empty means "delete
// the object", which on a versioned bucket means writing a delete marker and
// keeping the data.
func (h *ObjectHandler) deleteOneObject(bucket, key, versionID, versioning string, bypassGovernance bool) (objectDeletion, error) {
	if versionID != "" {
		return h.deleteObjectVersion(bucket, key, versionID, bypassGovernance)
	}
	switch versioning {
	case "Enabled":
		return h.writeDeleteMarker(bucket, key, generateVersionID())
	case "Suspended":
		// Suspended versioning replaces the null version with a null delete
		// marker, so the previous null version's data does go.
		h.engine.DeleteObjectVersion(bucket, key, "null")
		if err := h.store.DeleteObjectVersion(bucket, key, "null"); err != nil {
			return objectDeletion{}, &deleteNotRecorded{op: "DeleteObjectVersion", err: err}
		}
		del, err := h.writeDeleteMarker(bucket, key, "null")
		if err != nil {
			return del, err
		}
		del.Reap, del.ReapVersion = true, "null"
		return del, nil
	}
	return h.deleteCurrentObject(bucket, key, bypassGovernance)
}

// deleteObjectVersion removes one version permanently and repoints "latest" if
// that version was the current one.
func (h *ObjectHandler) deleteObjectVersion(bucket, key, versionID string, bypassGovernance bool) (objectDeletion, error) {
	// Version "null" names an object stored before its bucket was versioned. Its
	// bytes sit at the ordinary object path, NOT under .vs/, so the version-aware
	// delete below finds nothing to remove while the metadata still goes away.
	// That left the file on disk with no record pointing at it: an orphan of the
	// same kind as issue #47, and a bucket that could never be deleted because
	// DeleteBucket asks the storage engine, not the index, whether it is empty.
	//
	// Deleting the null version of such an object is simply deleting the object,
	// which is what S3 does.
	if versionID == nullVersionID {
		if meta, err := h.store.GetObjectMeta(bucket, key); err == nil && meta != nil && meta.VersionID == "" {
			return h.deleteCurrentObject(bucket, key, bypassGovernance)
		}
	}
	if err := h.checkObjectLock(bucket, key, versionID, bypassGovernance); err != nil {
		return objectDeletion{}, &deleteRefused{err: err}
	}
	h.engine.DeleteObjectVersion(bucket, key, versionID)
	if err := h.store.DeleteObjectVersion(bucket, key, versionID); err != nil {
		return objectDeletion{}, &deleteNotRecorded{op: "DeleteObjectVersion", err: err}
	}

	// The newest surviving version becomes current. Both records have to move:
	// the version entry carries IsLatest, and the objects bucket holds the
	// "latest" pointer, which still names the version just removed. Repointing
	// only one of them is what left a deleted delete marker hiding a live object.
	latest, _ := h.store.LatestObjectVersion(bucket, key)
	if latest != nil {
		latest.IsLatest = true
		if err := h.store.UpdateObjectVersionMeta(*latest); err != nil {
			return objectDeletion{}, &deleteNotRecorded{op: "promote next version", err: err}
		}
		if err := h.store.PutObjectMeta(*latest); err != nil {
			return objectDeletion{}, &deleteNotRecorded{op: "repoint latest version", err: err}
		}
	} else if err := h.store.DeleteObjectMeta(bucket, key); err != nil {
		return objectDeletion{}, &deleteNotRecorded{op: "DeleteObjectMeta", err: err}
	}
	return objectDeletion{VersionID: versionID, Reap: true, ReapVersion: versionID}, nil
}

// writeDeleteMarker hides the object behind a marker, keeping every version.
// No object-lock check: nothing is destroyed, which is also why S3 allows it on
// a locked object.
func (h *ObjectHandler) writeDeleteMarker(bucket, key, markerVersionID string) (objectDeletion, error) {
	if old, err := h.store.GetObjectMeta(bucket, key); err == nil && old.VersionID != "" {
		old.IsLatest = false
		if err := h.store.PutObjectVersion(*old); err != nil {
			return objectDeletion{}, &deleteNotRecorded{op: "demote previous version", err: err}
		}
	}
	dm := metadata.ObjectMeta{
		Bucket:       bucket,
		Key:          key,
		VersionID:    markerVersionID,
		IsLatest:     true,
		DeleteMarker: true,
		LastModified: time.Now().UTC().Unix(),
	}
	if err := h.store.PutObjectVersion(dm); err != nil {
		return objectDeletion{}, &deleteNotRecorded{op: "write delete marker", err: err}
	}
	if err := h.store.PutObjectMeta(dm); err != nil { // the latest pointer now names the marker
		return objectDeletion{}, &deleteNotRecorded{op: "write delete marker", err: err}
	}
	return objectDeletion{VersionID: markerVersionID, DeleteMarker: true}, nil
}

// deleteCurrentObject removes an unversioned object's data and metadata.
func (h *ObjectHandler) deleteCurrentObject(bucket, key string, bypassGovernance bool) (objectDeletion, error) {
	// Enforce WORM retention and legal hold before destroying anything, or an
	// object under a COMPLIANCE lock could be deleted outright.
	if err := h.checkObjectLock(bucket, key, "", bypassGovernance); err != nil {
		return objectDeletion{}, &deleteRefused{err: err}
	}
	if err := h.engine.DeleteObject(bucket, key); err != nil {
		return objectDeletion{}, err
	}
	if err := h.store.DeleteObjectMeta(bucket, key); err != nil {
		return objectDeletion{}, &deleteNotRecorded{op: "DeleteObjectMeta", err: err}
	}
	return objectDeletion{Reap: true}, nil
}

// notifyDeleted fires the four delete hooks, which every delete path shares.
func (h *ObjectHandler) notifyDeleted(bucket, key, versionID string) {
	if h.onNotification != nil {
		h.onNotification("s3:ObjectRemoved:Delete", bucket, key, 0, "", versionID)
	}
	if h.onReplication != nil {
		h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", versionID)
	}
	if h.onLambda != nil {
		h.onLambda("s3:ObjectRemoved:Delete", bucket, key, 0, "", versionID)
	}
	if h.onSearchUpdate != nil {
		h.onSearchUpdate("delete", bucket, key)
	}
}

// applyObjectLock populates meta's retention and legal-hold from the request's
// inline object-lock headers, falling back to the bucket's default retention. It
// is shared by every PutObject path (versioned, suspended, non-versioned) so WORM
// works the same regardless of a bucket's versioning state; previously only the
// versioned path applied these, so inline locks were silently dropped on
// non-versioned buckets.
func (h *ObjectHandler) applyObjectLock(r *http.Request, meta *metadata.ObjectMeta, bucket string, now time.Time) {
	if mode := r.Header.Get("X-Amz-Object-Lock-Mode"); mode != "" {
		meta.RetentionMode = mode
		if until := r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"); until != "" {
			if t, err := time.Parse(time.RFC3339, until); err == nil {
				meta.RetentionUntil = t.Unix()
			}
		}
	}
	if meta.RetentionMode == "" {
		if bucketInfo, err := h.store.GetBucket(bucket); err == nil {
			if bucketInfo.DefaultRetentionMode != "" && bucketInfo.DefaultRetentionDays > 0 {
				meta.RetentionMode = bucketInfo.DefaultRetentionMode
				meta.RetentionUntil = now.Unix() + int64(bucketInfo.DefaultRetentionDays*86400)
			}
		}
	}
	if lh := r.Header.Get("X-Amz-Object-Lock-Legal-Hold"); strings.EqualFold(lh, "ON") {
		meta.LegalHold = true
	}
}

// checkObjectLock checks if an object version is locked (legal hold or retention).
// If bypassGovernance is true, GOVERNANCE retention is skipped (requires s3:BypassGovernanceRetention).
func (h *ObjectHandler) checkObjectLock(bucket, key, versionID string, bypassGovernance ...bool) error {
	var meta *metadata.ObjectMeta
	var err error
	if versionID == "" {
		// No version specified: check the current object (non-versioned buckets, or
		// the latest pointer). GetObjectVersion(...,"") does not resolve to the
		// current object, so read it directly.
		meta, err = h.store.GetObjectMeta(bucket, key)
	} else {
		meta, err = h.store.GetObjectVersion(bucket, key, versionID)
	}
	if err != nil {
		return nil // object/version doesn't exist in metadata, allow delete
	}

	if meta.LegalHold {
		return fmt.Errorf("object is under legal hold")
	}

	if meta.RetentionMode != "" && meta.RetentionUntil > 0 {
		if time.Now().UTC().Unix() < meta.RetentionUntil {
			// Allow governance bypass if requested
			if meta.RetentionMode == "GOVERNANCE" && len(bypassGovernance) > 0 && bypassGovernance[0] {
				return nil
			}
			return fmt.Errorf("object is under %s retention until %s",
				meta.RetentionMode,
				time.Unix(meta.RetentionUntil, 0).UTC().Format(time.RFC3339))
		}
	}

	return nil
}

// HeadObject handles HEAD /{bucket}/{key}.
func (h *ObjectHandler) HeadObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	versionID := r.URL.Query().Get("versionId")

	var meta *metadata.ObjectMeta

	if versionID != "" {
		var err error
		meta, err = h.store.GetObjectVersion(bucket, key, versionID)
		if err != nil {
			if metadataUnavailable(w, err) {
				return
			}
			writeS3Error(w, "NoSuchVersion", "Version not found", http.StatusNotFound)
			return
		}
		if meta.DeleteMarker {
			w.Header().Set("X-Amz-Delete-Marker", "true")
			w.Header().Set("X-Amz-Version-Id", versionID)
			writeS3Error(w, "NoSuchKey", "Object is a delete marker", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Amz-Version-Id", versionID)
	} else {
		// Consistent read (barrier-on-miss) so a HEAD right after a PUT on another
		// cluster node doesn't spuriously 404 (issue #37).
		var metaErr error
		meta, metaErr = h.store.GetObjectMetaConsistent(bucket, key)
		if meta == nil && metadataUnavailable(w, metaErr) {
			return
		}
		if meta != nil && meta.DeleteMarker {
			w.Header().Set("X-Amz-Delete-Marker", "true")
			if meta.VersionID != "" {
				w.Header().Set("X-Amz-Version-Id", meta.VersionID)
			}
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}
		if meta == nil {
			// Metadata is the single source of truth for existence. A deleted object
			// removes its metadata cluster-wide (via Raft), but a data file can
			// linger on a replica node; do NOT fall back to the engine here or a
			// deleted object reappears as a phantom HEAD 200 with null
			// Last-Modified/ETag and a stale Content-Length (issue #34).
			//
			// Trace the HEAD miss (method + whether it was proxied here) so a
			// read-after-write HEAD 404 is visible to VAULTS3_TRACE_READS the same way
			// a GET miss is — HEAD is the operation `mc stat`/`warp` verify with, so
			// this is the line that localizes the miss to the owner vs a non-owner
			// (issue #37).
			traceRead404(r, "HEAD", bucket, key, "meta_nil")
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}
		if meta.VersionID != "" {
			w.Header().Set("X-Amz-Version-Id", meta.VersionID)
		}
	}

	// Conditional HEAD: check preconditions
	if checkGetPreconditions(w, r, meta) {
		return
	}

	// SSE-C objects require the matching customer key, even for HEAD.
	if meta.SSECustomerKeyMD5 != "" {
		ssecKey, perr := parseSSECHeaders(r)
		if perr != nil || ssecKey == nil {
			writeS3Error(w, "InvalidArgument", "object is SSE-C encrypted; a customer key is required", http.StatusBadRequest)
			return
		}
		if ssecKey.keyMD5 != meta.SSECustomerKeyMD5 {
			writeS3Error(w, "AccessDenied", "SSE-C key does not match", http.StatusForbidden)
			return
		}
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("Last-Modified", time.Unix(meta.LastModified, 0).UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	setHTTPMetadataHeaders(w, meta)
	setUserMetadataHeaders(w, meta)
	setChecksumHeaders(w, meta)
	if meta.PartsCount > 0 {
		w.Header().Set("X-Amz-Mp-Parts-Count", strconv.Itoa(meta.PartsCount))
	}
	if meta.SSECustomerKeyMD5 != "" {
		w.Header().Set(hdrSSECAlgo, "AES256")
		w.Header().Set(hdrSSECKeyMD5, meta.SSECustomerKeyMD5)
	} else if h.encryptionEnabled {
		w.Header().Set("X-Amz-Server-Side-Encryption", "AES256")
	}
	w.WriteHeader(http.StatusOK)
}

// CopyObject handles PUT /{bucket}/{key} with x-amz-copy-source header.
func (h *ObjectHandler) CopyObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Destination bucket does not exist", http.StatusNotFound)
		return
	}

	// Parse x-amz-copy-source: /source-bucket/source-key or source-bucket/source-key
	copySource := r.Header.Get("X-Amz-Copy-Source")
	copySource, _ = url.PathUnescape(copySource)
	copySource = strings.TrimPrefix(copySource, "/")

	srcBucket, srcKey := parseCopySource(copySource)
	if srcBucket == "" || srcKey == "" {
		writeS3Error(w, "InvalidArgument", "Invalid x-amz-copy-source", http.StatusBadRequest)
		return
	}
	// Validate source key against path traversal (check after unescaping AND
	// also check for double-encoded traversals by unescaping again)
	for _, segment := range strings.Split(srcKey, "/") {
		if segment == ".." {
			writeS3Error(w, "InvalidArgument", "Invalid x-amz-copy-source key", http.StatusBadRequest)
			return
		}
	}
	// Reject double-encoded path traversal (e.g. %252e%252e → %2e%2e → ..)
	if decoded, err := url.PathUnescape(srcKey); err == nil && decoded != srcKey {
		for _, segment := range strings.Split(decoded, "/") {
			if segment == ".." {
				writeS3Error(w, "InvalidArgument", "Invalid x-amz-copy-source key", http.StatusBadRequest)
				return
			}
		}
	}
	// Reject null bytes
	if strings.ContainsRune(srcKey, 0) {
		writeS3Error(w, "InvalidArgument", "Invalid x-amz-copy-source key", http.StatusBadRequest)
		return
	}

	if !h.store.BucketExists(srcBucket) {
		writeS3Error(w, "NoSuchBucket", "Source bucket does not exist", http.StatusNotFound)
		return
	}

	// Get source metadata for conditional copy checks and metadata copy
	srcMeta, _ := h.store.GetObjectMeta(srcBucket, srcKey)

	// Check conditional copy preconditions
	if checkCopyPreconditions(w, r, srcMeta) {
		return
	}

	// Read source object
	reader, size, err := h.engine.GetObject(srcBucket, srcKey)
	if err != nil {
		writeS3Error(w, "NoSuchKey", "Source object not found", http.StatusNotFound)
		return
	}
	defer reader.Close()

	// Write to destination
	written, etag, err := h.engine.PutObject(bucket, key, reader, size)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC()

	// Determine metadata: REPLACE uses request headers, COPY (default) uses source
	metadataDirective := r.Header.Get("X-Amz-Metadata-Directive")

	meta := metadata.ObjectMeta{
		Bucket:       bucket,
		Key:          key,
		ETag:         etag,
		Size:         written,
		LastModified: now.Unix(),
	}

	if strings.EqualFold(metadataDirective, "REPLACE") {
		// Use metadata from request headers
		meta.ContentType = detectContentType(r, key)
		meta.UserMetadata = parseUserMetadata(r)
		meta.Tags = parseInlineTags(r)
		meta.ContentEncoding = r.Header.Get("Content-Encoding")
		meta.ContentDisposition = r.Header.Get("Content-Disposition")
		meta.CacheControl = r.Header.Get("Cache-Control")
		meta.ContentLanguage = r.Header.Get("Content-Language")
		meta.WebsiteRedirect = r.Header.Get("X-Amz-Website-Redirect-Location")
	} else if srcMeta != nil {
		// COPY (default): copy metadata from source
		meta.ContentType = srcMeta.ContentType
		meta.UserMetadata = srcMeta.UserMetadata
		meta.Tags = srcMeta.Tags
		meta.ContentEncoding = srcMeta.ContentEncoding
		meta.ContentDisposition = srcMeta.ContentDisposition
		meta.CacheControl = srcMeta.CacheControl
		meta.ContentLanguage = srcMeta.ContentLanguage
		meta.WebsiteRedirect = srcMeta.WebsiteRedirect
		meta.ChecksumSHA256 = srcMeta.ChecksumSHA256
		meta.ChecksumCRC32 = srcMeta.ChecksumCRC32
		meta.ChecksumCRC32C = srcMeta.ChecksumCRC32C
		meta.ChecksumSHA1 = srcMeta.ChecksumSHA1
	} else {
		meta.ContentType = "application/octet-stream"
	}

	if err := h.store.PutObjectMeta(meta); err != nil {
		metaWriteFailed(w, err, "PutObjectMeta (copy)", bucket, key)
		return
	}

	type copyResult struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		ETag         string   `xml:"ETag"`
		LastModified string   `xml:"LastModified"`
	}

	writeXML(w, http.StatusOK, copyResult{
		ETag:         etag,
		LastModified: now.Format(time.RFC3339),
	})
	if h.onNotification != nil {
		h.onNotification("s3:ObjectCreated:Copy", bucket, key, written, etag, "")
	}
	if h.onReplication != nil {
		h.onReplication("s3:ObjectCreated:Copy", bucket, key, written, etag, "")
	}
	if h.onLambda != nil {
		h.onLambda("s3:ObjectCreated:Copy", bucket, key, written, etag, "")
	}
	if h.onScan != nil {
		h.onScan(bucket, key, written)
	}
	if h.onSearchUpdate != nil {
		h.onSearchUpdate("put", bucket, key)
	}
}

func parseCopySource(source string) (bucket, key string) {
	parts := strings.SplitN(source, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// maxDeleteRequestBody bounds a multi-object delete body. S3 caps the request
// at 1000 keys, and a key can be 1024 bytes, so the old 256 KiB limit could
// reject a legal request; this leaves room for the envelope as well.
const maxDeleteRequestBody = 4 << 20

// BatchDelete handles POST /{bucket}?delete.
//
// Every key goes through the same deleteOneObject the single-object DELETE uses.
// It used to have its own logic that removed the data and the metadata
// unconditionally, so on a versioning-enabled bucket a multi-object delete
// DESTROYED objects that a single delete would only have hidden behind a delete
// marker. Nothing reported it: the response said "Deleted" either way.
func (h *ObjectHandler) BatchDelete(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	var req deleteRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, maxDeleteRequestBody)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse request body", http.StatusBadRequest)
		return
	}

	versioning, _ := h.store.GetBucketVersioning(bucket)
	bypassGov := strings.EqualFold(r.Header.Get("X-Amz-Bypass-Governance-Retention"), "true")

	var result deleteResult
	// Keys whose data must also be dropped on the other nodes. This request was
	// routed by hash(bucket, "") to a single node, so the engine delete below only
	// frees the keys that happen to live here; the rest sit on the other nodes and
	// become unreachable orphans the moment the Raft-replicated metadata is gone.
	// That is how a delete-heavy workload grew to ~9x its logical size (issue #47).
	var reaped []string
	for _, obj := range req.Objects {
		// Validate key against path traversal
		invalid := false
		for _, segment := range strings.Split(obj.Key, "/") {
			if segment == ".." {
				invalid = true
				break
			}
		}
		if invalid {
			result.Errors = append(result.Errors, deleteError{
				Key:     obj.Key,
				Code:    "InvalidArgument",
				Message: "Invalid key",
			})
			continue
		}

		// Authorize this entry on its own. The router cannot: it decides before
		// the body is parsed, and each entry may name its own version. Deleting a
		// named version is s3:DeleteObjectVersion, which is a different permission
		// from the recoverable s3:DeleteObject, so a policy that allows deletes
		// while denying permanent destruction must hold here too.
		entryAction := "s3:DeleteObject"
		if obj.VersionID != "" {
			entryAction = "s3:DeleteObjectVersion"
		}
		if err := h.authorizeEntry(r, entryAction, formatResource(bucket, obj.Key)); err != nil {
			// AWS reports a per-key error rather than failing the whole request,
			// so one denied key does not hide the outcome of the others.
			result.Errors = append(result.Errors, deleteError{
				Key:     obj.Key,
				Code:    "AccessDenied",
				Message: err.Error(),
			})
			continue
		}

		del, err := h.deleteOneObject(bucket, obj.Key, obj.VersionID, versioning, bypassGov)
		if err != nil {
			result.Errors = append(result.Errors, batchDeleteError(obj, err))
			continue
		}

		switch {
		case !del.Reap:
			// A delete marker removed nothing, so there is nothing to reap.
		case del.ReapVersion == "":
			reaped = append(reaped, obj.Key)
		default:
			// A version-specific delete cannot go through the batch reaper, which
			// only ever removes a key's CURRENT data. Sending it there would make
			// the peers delete the wrong file.
			h.reapElsewhere(bucket, obj.Key, del.ReapVersion)
		}

		if !req.Quiet {
			entry := deletedObject{Key: obj.Key, VersionID: obj.VersionID}
			if del.DeleteMarker {
				entry.DeleteMarker = true
				entry.DeleteMarkerVersionID = del.VersionID
			}
			result.Deleted = append(result.Deleted, entry)
		}
		h.notifyDeleted(bucket, obj.Key, del.VersionID)
	}

	if len(reaped) > 0 {
		if h.reapReplicasBatch != nil {
			h.reapReplicasBatch(bucket, reaped)
		} else if h.reapReplicas != nil {
			for _, k := range reaped {
				h.reapReplicas(bucket, k, "")
			}
		}
	}

	writeXML(w, http.StatusOK, result)
}

// batchDeleteError turns one key's failure into its entry in the result. The
// codes match what the single-object path answers with, so a client sees the
// same reason whichever call it made.
func batchDeleteError(obj deleteObject, err error) deleteError {
	entry := deleteError{Key: obj.Key, VersionID: obj.VersionID}
	var refused *deleteRefused
	if errors.As(err, &refused) {
		entry.Code, entry.Message = "AccessDenied", refused.Error()
		return entry
	}
	var notRecorded *deleteNotRecorded
	if errors.As(err, &notRecorded) {
		// Metadata is authoritative, so a key whose record survives must not be
		// reported as deleted: it would still list (issue #34).
		slog.Error("metadata delete failed in multi-object delete",
			"key", obj.Key, "op", notRecorded.op, "error", notRecorded.err)
		entry.Code, entry.Message = "SlowDown", "The delete could not be recorded, please retry"
		return entry
	}
	entry.Code, entry.Message = "InternalError", err.Error()
	return entry
}

// PutObjectTagging handles PUT /{bucket}/{key}?tagging.
func (h *ObjectHandler) PutObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	if !h.engine.ObjectExists(bucket, key) {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}

	var req taggingRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse tagging XML", http.StatusBadRequest)
		return
	}

	if len(req.TagSet.Tags) > 10 {
		writeS3Error(w, "BadRequest", "Object tags cannot be greater than 10", http.StatusBadRequest)
		return
	}

	meta, err := h.store.GetObjectMeta(bucket, key)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	meta.Tags = make(map[string]string, len(req.TagSet.Tags))
	for _, tag := range req.TagSet.Tags {
		meta.Tags[tag.Key] = tag.Value
	}

	if err := h.store.PutObjectMeta(*meta); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	if h.onSearchUpdate != nil {
		h.onSearchUpdate("put", bucket, key)
	}
}

// GetObjectTagging handles GET /{bucket}/{key}?tagging.
func (h *ObjectHandler) GetObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	if !h.engine.ObjectExists(bucket, key) {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}

	meta, err := h.store.GetObjectMeta(bucket, key)
	if err != nil {
		// No metadata yet — return empty tag set
		writeXML(w, http.StatusOK, taggingResponse{
			Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		})
		return
	}

	resp := taggingResponse{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	for k, v := range meta.Tags {
		resp.TagSet.Tags = append(resp.TagSet.Tags, xmlTag{Key: k, Value: v})
	}

	writeXML(w, http.StatusOK, resp)
}

// DeleteObjectTagging handles DELETE /{bucket}/{key}?tagging.
func (h *ObjectHandler) DeleteObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	if !h.engine.ObjectExists(bucket, key) {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}

	meta, err := h.store.GetObjectMeta(bucket, key)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	meta.Tags = nil
	if err := h.store.PutObjectMeta(*meta); err != nil {
		metaWriteFailed(w, err, "DeleteObjectTagging", bucket, key)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	if h.onSearchUpdate != nil {
		h.onSearchUpdate("put", bucket, key)
	}
}

// nullVersionID is the version id S3 reports for an object stored before its
// bucket had versioning enabled.
const nullVersionID = "null"

// listNullVersions returns the objects that predate versioning on this bucket,
// which S3 reports as the version id "null". They are the entries in the
// latest-pointer index that carry no version id: an object written while
// versioning was enabled always has one.
//
// A delete marker is never a null version, so those are skipped.
func (h *ObjectHandler) listNullVersions(bucket, prefix, keyMarker, versionMarker string, maxKeys int) ([]metadata.ObjectMeta, bool, error) {
	// A version-id marker only makes sense once a key marker names the key it
	// belongs to, and "null" sorts as its own single version, so a request that
	// resumes past a specific version has already passed the null one.
	startAfter := keyMarker
	if keyMarker != "" && versionMarker != "" && versionMarker != nullVersionID {
		startAfter = keyMarker
	}

	latest, truncated, err := h.store.ListLatestObjects(bucket, prefix, startAfter, maxKeys)
	if err != nil {
		return nil, false, err
	}

	out := make([]metadata.ObjectMeta, 0, len(latest))
	for _, m := range latest {
		if m.VersionID != "" || m.DeleteMarker {
			continue // a real version, or a marker: already covered above
		}
		m.VersionID = nullVersionID
		m.IsLatest = true
		out = append(out, m)
	}
	return out, truncated, nil
}

// sortVersionsForListing puts the merged list back into the order S3 promises:
// by key, and within a key the latest version first.
func sortVersionsForListing(versions []metadata.ObjectMeta) {
	sort.SliceStable(versions, func(i, j int) bool {
		if versions[i].Key != versions[j].Key {
			return versions[i].Key < versions[j].Key
		}
		if versions[i].IsLatest != versions[j].IsLatest {
			return versions[i].IsLatest
		}
		return versions[i].LastModified > versions[j].LastModified
	})
}

// ListObjectVersions handles GET /{bucket}?versions.
func (h *ObjectHandler) ListObjectVersions(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	prefix := r.URL.Query().Get("prefix")
	keyMarker := r.URL.Query().Get("key-marker")
	versionMarker := r.URL.Query().Get("version-id-marker")
	maxKeysStr := r.URL.Query().Get("max-keys")
	maxKeys := 1000
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 && mk <= 1000 {
			maxKeys = mk
		}
	}

	versions, truncated, err := h.store.ListObjectVersions(bucket, prefix, keyMarker, versionMarker, maxKeys)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	// Objects written while a bucket was NOT versioned have no version record at
	// all: they live only in the latest-pointer index with an empty VersionID.
	// S3 still lists them here, as a version whose id is the literal "null".
	//
	// Returning nothing for them is not a cosmetic gap. ListObjectVersions is how
	// tools enumerate a bucket in order to empty it, so a caller saw an empty
	// bucket, deleted nothing, and then could not delete the bucket either. The
	// ceph/s3-tests fixture does exactly that, which is how this was found: one
	// undeletable bucket failed the setup of every following test.
	nullVersions, nullTruncated, err := h.listNullVersions(bucket, prefix, keyMarker, versionMarker, maxKeys)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	versions = append(versions, nullVersions...)
	truncated = truncated || nullTruncated
	sortVersionsForListing(versions)
	if len(versions) > maxKeys {
		versions = versions[:maxKeys]
		truncated = true
	}

	type xmlVersion struct {
		Key          string `xml:"Key"`
		VersionId    string `xml:"VersionId"`
		IsLatest     bool   `xml:"IsLatest"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag,omitempty"`
		Size         int64  `xml:"Size"`
		StorageClass string `xml:"StorageClass,omitempty"`
	}
	type xmlDeleteMarker struct {
		Key          string `xml:"Key"`
		VersionId    string `xml:"VersionId"`
		IsLatest     bool   `xml:"IsLatest"`
		LastModified string `xml:"LastModified"`
	}
	type xmlListVersionsResult struct {
		XMLName         xml.Name          `xml:"ListVersionsResult"`
		Xmlns           string            `xml:"xmlns,attr"`
		Name            string            `xml:"Name"`
		Prefix          string            `xml:"Prefix,omitempty"`
		KeyMarker       string            `xml:"KeyMarker"`
		VersionIdMarker string            `xml:"VersionIdMarker"`
		MaxKeys         int               `xml:"MaxKeys"`
		IsTruncated     bool              `xml:"IsTruncated"`
		Versions        []xmlVersion      `xml:"Version,omitempty"`
		DeleteMarkers   []xmlDeleteMarker `xml:"DeleteMarker,omitempty"`
	}

	resp := xmlListVersionsResult{
		Xmlns:           "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:            bucket,
		Prefix:          prefix,
		KeyMarker:       keyMarker,
		VersionIdMarker: versionMarker,
		MaxKeys:         maxKeys,
		IsTruncated:     truncated,
	}

	for _, v := range versions {
		if v.DeleteMarker {
			resp.DeleteMarkers = append(resp.DeleteMarkers, xmlDeleteMarker{
				Key:          v.Key,
				VersionId:    v.VersionID,
				IsLatest:     v.IsLatest,
				LastModified: time.Unix(v.LastModified, 0).UTC().Format(time.RFC3339),
			})
		} else {
			resp.Versions = append(resp.Versions, xmlVersion{
				Key:          v.Key,
				VersionId:    v.VersionID,
				IsLatest:     v.IsLatest,
				LastModified: time.Unix(v.LastModified, 0).UTC().Format(time.RFC3339),
				ETag:         v.ETag,
				Size:         v.Size,
				StorageClass: "STANDARD",
			})
		}
	}

	writeXML(w, http.StatusOK, resp)
}

// ListObjects handles GET /{bucket}?list-type=2.
// listObjects returns the latest objects for a bucket. For versioned (Enabled
// or Suspended) buckets, object data is stored under .vs/ and is invisible to
// the storage engine's filesystem walk, so the metadata store's latest-pointer
// index is used as the source of truth. Non-versioned buckets use the engine.
func (h *ObjectHandler) listObjects(bucket, prefix, startAfter string, maxKeys int) ([]storage.ObjectInfo, bool, error) {
	// All listing goes through the BoltDB metadata index (sorted keys → seek to
	// the page, O(log n + pageSize)), regardless of versioning. Every write path
	// updates the store, so it is the authoritative listing source — and this
	// avoids the O(n) filesystem walk that doesn't scale to very large buckets.
	metas, truncated, err := h.store.ListLatestObjects(bucket, prefix, startAfter, maxKeys)
	if err != nil {
		return nil, false, err
	}
	objects := make([]storage.ObjectInfo, 0, len(metas))
	for _, m := range metas {
		objects = append(objects, storage.ObjectInfo{
			Key:          m.Key,
			Size:         m.Size,
			LastModified: m.LastModified,
			ETag:         m.ETag,
		})
	}
	return objects, truncated, nil
}

func (h *ObjectHandler) ListObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	startAfter := r.URL.Query().Get("start-after")
	contToken := r.URL.Query().Get("continuation-token")
	maxKeysStr := r.URL.Query().Get("max-keys")
	maxKeys := 1000
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 && mk <= 1000 {
			maxKeys = mk
		}
	}

	// A continuation token (opaque, base64 of the cursor after the last returned
	// entry) takes precedence over start-after and resumes exactly where the
	// previous page ended — this is what lets clients walk past the first page at
	// any scale.
	effectiveStart := startAfter
	if contToken != "" {
		if dec, err := base64.StdEncoding.DecodeString(contToken); err == nil {
			effectiveStart = string(dec)
		}
	}

	// A delimiter collapses keys sharing the next path segment into CommonPrefixes
	// ("folders"): how clients (aws s3 ls, the dashboard file browser) browse a
	// bucket. The store does the grouping at the sorted index level so it stays
	// O(page) even for huge prefixes. With no delimiter this returns a flat page.
	metas, commonPrefixes, truncated, nextCursor, err := h.store.ListLatestObjectsDelimited(bucket, prefix, delimiter, effectiveStart, maxKeys)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	type xmlContent struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	}
	type xmlCommonPrefix struct {
		Prefix string `xml:"Prefix"`
		// LastModified is a VaultS3 extension: standard S3 CommonPrefixes carry no
		// timestamp, so folders list dateless and clients fake a date (issue #35).
		// This surfaces the folder's real date (its directory marker or first child)
		// for clients that read it; standard clients ignore the extra element.
		LastModified string `xml:"LastModified,omitempty"`
	}
	type xmlResponse struct {
		XMLName               xml.Name          `xml:"ListBucketResult"`
		Xmlns                 string            `xml:"xmlns,attr"`
		Name                  string            `xml:"Name"`
		Prefix                string            `xml:"Prefix"`
		Delimiter             string            `xml:"Delimiter,omitempty"`
		MaxKeys               int               `xml:"MaxKeys"`
		IsTruncated           bool              `xml:"IsTruncated"`
		Contents              []xmlContent      `xml:"Contents"`
		CommonPrefixes        []xmlCommonPrefix `xml:"CommonPrefixes,omitempty"`
		KeyCount              int               `xml:"KeyCount"`
		ContinuationToken     string            `xml:"ContinuationToken,omitempty"`
		NextContinuationToken string            `xml:"NextContinuationToken,omitempty"`
		StartAfter            string            `xml:"StartAfter,omitempty"`
	}

	resp := xmlResponse{
		Xmlns:             "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:              bucket,
		Prefix:            prefix,
		Delimiter:         delimiter,
		MaxKeys:           maxKeys,
		IsTruncated:       truncated,
		ContinuationToken: contToken,
		StartAfter:        startAfter,
	}

	for _, m := range metas {
		resp.Contents = append(resp.Contents, xmlContent{
			Key:          m.Key,
			LastModified: time.Unix(m.LastModified, 0).UTC().Format(time.RFC3339),
			ETag:         m.ETag,
			Size:         m.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, cp := range commonPrefixes {
		xcp := xmlCommonPrefix{Prefix: cp.Prefix}
		if cp.LastModified > 0 {
			xcp.LastModified = time.Unix(cp.LastModified, 0).UTC().Format(time.RFC3339)
		}
		resp.CommonPrefixes = append(resp.CommonPrefixes, xcp)
	}
	resp.KeyCount = len(resp.Contents) + len(resp.CommonPrefixes)

	// When more entries remain, hand back an opaque token the client echoes as
	// continuation-token to fetch the next page.
	if truncated && nextCursor != "" {
		resp.NextContinuationToken = base64.StdEncoding.EncodeToString([]byte(nextCursor))
	}

	writeXML(w, http.StatusOK, resp)
}

// ListObjectsV1 handles GET /{bucket} (V1 with marker-based pagination).
func (h *ObjectHandler) ListObjectsV1(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	marker := r.URL.Query().Get("marker")
	maxKeysStr := r.URL.Query().Get("max-keys")
	maxKeys := 1000
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 && mk <= 1000 {
			maxKeys = mk
		}
	}

	// V1 uses marker as start-after
	objects, truncated, err := h.listObjects(bucket, prefix, marker, maxKeys)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	type xmlContent struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	}
	type xmlCommonPrefix struct {
		Prefix string `xml:"Prefix"`
	}
	type xmlV1Response struct {
		XMLName        xml.Name          `xml:"ListBucketResult"`
		Xmlns          string            `xml:"xmlns,attr"`
		Name           string            `xml:"Name"`
		Prefix         string            `xml:"Prefix"`
		Marker         string            `xml:"Marker"`
		Delimiter      string            `xml:"Delimiter,omitempty"`
		MaxKeys        int               `xml:"MaxKeys"`
		IsTruncated    bool              `xml:"IsTruncated"`
		Contents       []xmlContent      `xml:"Contents"`
		CommonPrefixes []xmlCommonPrefix `xml:"CommonPrefixes,omitempty"`
		NextMarker     string            `xml:"NextMarker,omitempty"`
	}

	resp := xmlV1Response{
		Xmlns:       "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:        bucket,
		Prefix:      prefix,
		Marker:      marker,
		Delimiter:   delimiter,
		MaxKeys:     maxKeys,
		IsTruncated: truncated,
	}

	if delimiter != "" {
		seen := make(map[string]bool)
		for _, obj := range objects {
			rel := strings.TrimPrefix(obj.Key, prefix)
			if idx := strings.Index(rel, delimiter); idx >= 0 {
				cp := prefix + rel[:idx+len(delimiter)]
				if !seen[cp] {
					seen[cp] = true
					resp.CommonPrefixes = append(resp.CommonPrefixes, xmlCommonPrefix{Prefix: cp})
				}
			} else {
				resp.Contents = append(resp.Contents, xmlContent{
					Key:          obj.Key,
					LastModified: time.Unix(obj.LastModified, 0).UTC().Format(time.RFC3339),
					ETag:         obj.ETag,
					Size:         obj.Size,
					StorageClass: "STANDARD",
				})
			}
		}
	} else {
		for _, obj := range objects {
			resp.Contents = append(resp.Contents, xmlContent{
				Key:          obj.Key,
				LastModified: time.Unix(obj.LastModified, 0).UTC().Format(time.RFC3339),
				ETag:         obj.ETag,
				Size:         obj.Size,
				StorageClass: "STANDARD",
			})
		}
	}

	if truncated && len(resp.Contents) > 0 {
		resp.NextMarker = resp.Contents[len(resp.Contents)-1].Key
	}

	writeXML(w, http.StatusOK, resp)
}

// GetObjectAttributes handles GET /{bucket}/{key}?attributes.
func (h *ObjectHandler) GetObjectAttributes(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	versionID := r.URL.Query().Get("versionId")
	var meta *metadata.ObjectMeta
	var err error
	if versionID != "" {
		meta, err = h.store.GetObjectVersion(bucket, key, versionID)
	} else {
		meta, err = h.store.GetObjectMeta(bucket, key)
	}
	if err != nil || meta == nil {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}
	if meta.DeleteMarker {
		w.Header().Set("X-Amz-Delete-Marker", "true")
		writeS3Error(w, "NoSuchKey", "Object is a delete marker", http.StatusNotFound)
		return
	}

	type xmlObjectParts struct {
		TotalPartsCount int `xml:"TotalPartsCount"`
	}
	type xmlChecksum struct {
		ChecksumSHA256 string `xml:"ChecksumSHA256,omitempty"`
		ChecksumCRC32  string `xml:"ChecksumCRC32,omitempty"`
		ChecksumCRC32C string `xml:"ChecksumCRC32C,omitempty"`
		ChecksumSHA1   string `xml:"ChecksumSHA1,omitempty"`
	}
	type xmlObjectAttributes struct {
		XMLName      xml.Name        `xml:"GetObjectAttributesResponse"`
		ETag         string          `xml:"ETag,omitempty"`
		ObjectSize   int64           `xml:"ObjectSize"`
		StorageClass string          `xml:"StorageClass"`
		Checksum     *xmlChecksum    `xml:"Checksum,omitempty"`
		ObjectParts  *xmlObjectParts `xml:"ObjectParts,omitempty"`
	}

	resp := xmlObjectAttributes{
		ETag:         meta.ETag,
		ObjectSize:   meta.Size,
		StorageClass: "STANDARD",
	}

	if meta.ChecksumSHA256 != "" || meta.ChecksumCRC32 != "" || meta.ChecksumCRC32C != "" || meta.ChecksumSHA1 != "" {
		resp.Checksum = &xmlChecksum{
			ChecksumSHA256: meta.ChecksumSHA256,
			ChecksumCRC32:  meta.ChecksumCRC32,
			ChecksumCRC32C: meta.ChecksumCRC32C,
			ChecksumSHA1:   meta.ChecksumSHA1,
		}
	}

	if meta.PartsCount > 0 {
		resp.ObjectParts = &xmlObjectParts{TotalPartsCount: meta.PartsCount}
	}

	if meta.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", meta.VersionID)
	}

	writeXML(w, http.StatusOK, resp)
}

// PutObjectACL handles PUT /{bucket}/{key}?acl — accepts but is a no-op (VaultS3 uses policies).
func (h *ObjectHandler) PutObjectACL(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	_, err := h.store.GetObjectMeta(bucket, key)
	if err != nil {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}
	io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusOK)
}

// GetObjectACL handles GET /{bucket}/{key}?acl — returns default private ACL.
func (h *ObjectHandler) GetObjectACL(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	_, err := h.store.GetObjectMeta(bucket, key)
	if err != nil {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}
	type grantee struct {
		XMLName     xml.Name `xml:"Grantee"`
		XMLNS       string   `xml:"xmlns:xsi,attr"`
		Type        string   `xml:"xsi:type,attr"`
		ID          string   `xml:"ID"`
		DisplayName string   `xml:"DisplayName"`
	}
	type grant struct {
		Grantee    grantee `xml:"Grantee"`
		Permission string  `xml:"Permission"`
	}
	type aclResult struct {
		XMLName xml.Name `xml:"AccessControlPolicy"`
		Xmlns   string   `xml:"xmlns,attr"`
		Owner   xmlOwner `xml:"Owner"`
		ACL     []grant  `xml:"AccessControlList>Grant"`
	}
	writeXML(w, http.StatusOK, aclResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner: xmlOwner{ID: "vaults3", DisplayName: "VaultS3"},
		ACL: []grant{{
			Grantee:    grantee{XMLNS: "http://www.w3.org/2001/XMLSchema-instance", Type: "CanonicalUser", ID: "vaults3", DisplayName: "VaultS3"},
			Permission: "FULL_CONTROL",
		}},
	})
}
