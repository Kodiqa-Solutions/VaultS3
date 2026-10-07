package s3

import (
	"crypto/md5"
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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
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
	// perBucketMode is encryption.per_bucket: only then is encryption a
	// per-bucket question rather than a server-wide one.
	perBucketMode bool
	// keyMgr is the per-bucket key manager, non-nil only in per-bucket mode. It
	// answers whether a given bucket actually opted into encryption.
	keyMgr *bucketcrypto.Manager
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

// writePutError turns a failed object write into a response. A bucket key that
// has not reached this node yet is transient, not an internal fault: the entry
// is moments away and every S3 SDK retries a 503 on its own, so the caller's own
// retry lands after it arrives. Refusing beats the alternative, which was
// storing the object in the clear in a bucket that asked for encryption.
func writePutError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrBucketKeyUnavailable) {
		slog.Warn("refusing a write until this node has the bucket encryption key")
		writeS3Error(w, "SlowDown", "Bucket encryption key is still replicating, please retry", http.StatusServiceUnavailable)
		return
	}
	slog.Error("internal error", "error", err)
	writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
}

// sseHeaderApplies reports whether this object really is encrypted at rest, and
// so whether the response may claim `x-amz-server-side-encryption: AES256`.
//
// It used to be the global encryption flag alone. In per-bucket mode that is not
// the same question: a bucket that never opted in stores plaintext, and the
// server was telling every client its objects were encrypted when they were not.
// A false claim of encryption is worse than no claim, because it is the answer a
// compliance check reads (issue #53).
func (h *ObjectHandler) sseHeaderApplies(bucket string) bool {
	if !h.encryptionEnabled {
		return false
	}
	// Only per-bucket mode makes this a per-bucket question. A server-wide key
	// and SSE-KMS both encrypt every object, so the answer is simply yes.
	//
	// Asking the key manager instead of the mode was wrong: the manager is built
	// whenever a valid `encryption.key` is set, and SSE-KMS requires one even
	// though it never uses it. In KMS mode it therefore answered "this bucket has
	// no per-bucket key", meaning "not encrypted", for objects the KMS engine had
	// encrypted. That understated the SSE response header and, once the clustered
	// read used this to decide whether to compare content, made every SSE-KMS
	// object on a cluster fail that comparison and answer 503.
	if !h.perBucketMode {
		return true
	}
	if h.keyMgr == nil {
		return true
	}
	return h.keyMgr.IsEncrypted(bucket)
}

// localCopyIsStale reports that the bytes this node holds are not the bytes the
// metadata describes, which in a cluster means an overwrite landed elsewhere and
// has not replicated here yet.
//
// Metadata is Raft-replicated and synchronous, object data is copied
// asynchronously, so the two are briefly out of step after an overwrite. The read
// path used to hand back whatever the local engine could open and only consulted
// a peer when opening FAILED, so a node holding the PREVIOUS bytes served them
// under the NEW object's ETag and Last-Modified: a silent wrong answer rather
// than a miss, which is the worst shape a storage bug can take.
//
// Size is the signal because it is already in hand: the engine returns it from
// the open, and after decompression and decryption it is the plaintext length the
// metadata records. Comparing it costs nothing and never gives a false positive.
// An overwrite that keeps the byte count identical still slips through, so this
// narrows the window rather than closing it completely.
//
// Single-node servers skip the check: there is no second copy to be behind, and
// no peer to ask if the check were to fire.
func (h *ObjectHandler) localCopyIsStale(meta *metadata.ObjectMeta, opened int64) bool {
	if h.dataHolderFallback == nil || meta == nil {
		return false
	}
	if meta.DeleteMarker || meta.Size < 0 || opened < 0 {
		return false
	}
	return meta.Size != opened
}

// staleVerifyWindow is how long after a write a clustered node still checks the
// CONTENT of its local copy instead of trusting it. Data replication settles in
// about two seconds, so a minute is generous. Outside the window the local copy
// has certainly caught up and reads cost nothing extra, which is the case for
// almost every read a real workload makes.
const staleVerifyWindow = 60 * time.Second

// staleVerifyMaxSize bounds the work: verifying means hashing the local copy, so
// it is only worth doing while the object is small enough that the hash is
// cheap next to serving it. A larger object inside the window falls back to the
// size check alone.
const staleVerifyMaxSize = 64 << 20

// localCopyNeedsContentCheck reports whether a same-size local copy is recent
// enough to be worth verifying byte for byte.
//
// The size check alone catches an overwrite that changed the byte count, which
// is most of them, but an overwrite that keeps the size identical slips through
// and is served as a silent wrong answer. Content is the only signal that
// catches that, and hashing every read would undo the streaming work from issue
// #38, so it is limited to objects that were written moments ago: exactly the
// window in which replication can still be in flight.
func (h *ObjectHandler) localCopyNeedsContentCheck(bucket string, meta *metadata.ObjectMeta, opened int64) bool {
	if h.dataHolderFallback == nil || meta == nil || meta.DeleteMarker {
		return false
	}
	// An encrypted object's ETag is the MD5 of the stored CIPHERTEXT while the
	// reader by now yields plaintext, so the two can never agree and the check
	// would call every such copy corrupt. The size check alone has to do.
	//
	// This was written for SSE-C alone, where the customer key makes it obvious.
	// Server-side encryption has exactly the same shape and was missed: on a
	// cluster every read of a per-bucket encrypted object failed its content
	// check, found no holder that could pass it either, and answered 503
	// SlowDown. A bucket with encryption switched on was effectively unreadable
	// for the first minute after each write, which is the whole window this
	// check covers.
	if meta.SSECustomerKeyMD5 != "" || h.sseHeaderApplies(bucket) {
		return false
	}
	if opened <= 0 || opened > staleVerifyMaxSize {
		return false
	}
	// A multipart ETag is not the MD5 of the object, so it cannot be recomputed
	// from the bytes here.
	if meta.ETag == "" || strings.Contains(meta.ETag, "-") {
		return false
	}
	if meta.LastModified == 0 {
		return false
	}
	return time.Since(time.Unix(meta.LastModified, 0)) < staleVerifyWindow
}

// localCopyContentMatches hashes what the engine opened and compares it with the
// ETag the metadata records, then rewinds so the caller can still serve it.
// Returns true when it cannot tell, so an unreadable or unseekable copy is not
// mistaken for a stale one.
func (h *ObjectHandler) localCopyContentMatches(reader storage.ReadSeekCloser, meta *metadata.ObjectMeta) bool {
	sum := md5.New()
	if _, err := io.Copy(sum, reader); err != nil {
		return true
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return true
	}
	return strings.Trim(meta.ETag, `"`) == hex.EncodeToString(sum.Sum(nil))
}

// openSSEC swaps the stored SSE-C blob for a plaintext reader, requiring and
// verifying the customer key from the request. It runs BEFORE the stale-copy
// checks because those compare the opened size with meta.Size, and meta.Size is
// the plaintext length: compared against the ciphertext length every SSE-C GET
// on a cluster node looked like a copy that had not caught up and was routed to
// a peer or refused with SlowDown. For objects in the streaming format the
// returned reader decrypts a chunk at a time and seeks without buffering; only
// pre-streaming objects are still read in full. On failure the response has
// been written, reader is closed, and ok is false.
func (h *ObjectHandler) openSSEC(w http.ResponseWriter, r *http.Request, meta *metadata.ObjectMeta,
	reader storage.ReadSeekCloser, size int64,
) (storage.ReadSeekCloser, int64, bool) {
	if meta == nil || meta.SSECustomerKeyMD5 == "" {
		return reader, size, true
	}
	ssecKey, perr := parseSSECHeaders(r)
	if perr != nil || ssecKey == nil {
		reader.Close()
		writeS3Error(w, "InvalidArgument", "object is SSE-C encrypted; a customer key is required", http.StatusBadRequest)
		return nil, 0, false
	}
	if ssecKey.keyMD5 != meta.SSECustomerKeyMD5 {
		reader.Close()
		writeS3Error(w, "AccessDenied", "SSE-C key does not match the one used to encrypt this object", http.StatusForbidden)
		return nil, 0, false
	}
	plain, plainSize, derr := ssecOpenStored(ssecKey, reader, size)
	if derr != nil {
		// A wrong key and a corrupt blob both surface as an authentication
		// failure, which is the one answer that does not leak which it was.
		reader.Close()
		writeS3Error(w, "AccessDenied", "SSE-C decryption failed", http.StatusForbidden)
		return nil, 0, false
	}
	// The streaming reader closes the engine reader it wraps; the legacy one has
	// nothing to close, so closing plain is always enough for the caller.
	return plain, plainSize, true
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
		metas = append(metas, objMeta{key: obj.Key, size: meta.Size, mod: time.Unix(meta.LastModified, 0)})
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

		// Eviction is a delete like any other, so object lock applies to it.
		// It used to remove objects under legal hold and COMPLIANCE retention,
		// and to count a failed delete as freed space.
		if err := h.checkObjectLock(bucket, m.key, ""); err != nil {
			continue
		}
		if err := h.engine.DeleteObject(bucket, m.key); err != nil {
			slog.Warn("fifo quota: could not evict an object", "bucket", bucket, "key", m.key, "error", err)
			continue
		}
		if err := h.store.DeleteObjectMeta(bucket, m.key); err != nil {
			slog.Error("fifo quota: evicted an object's data but could not remove its metadata",
				"bucket", bucket, "key", m.key, "error", err)
			continue
		}
		h.reapElsewhere(bucket, m.key, "")
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

	ct := detectContentType(r, key)

	// Parse extended metadata from headers
	userMeta := parseUserMetadata(r)
	tags, tagErr := parseInlineTags(r)
	if tagErr != nil {
		writeTagError(w, tagErr)
		return
	}

	// SSE-C (customer-provided keys). Supported on the non-versioned path for now.
	ssecKey, ssecErr := parseSSECHeaders(r)
	if ssecErr != nil {
		writeS3Error(w, "InvalidArgument", ssecErr.Error(), http.StatusBadRequest)
		return
	}

	// The body streams to the engine while its digests are computed in passing,
	// so a large upload costs a copy buffer rather than its whole size in memory
	// (issue #46). The digests and the quota are checked INSIDE the reader, so a
	// rejected upload fails the engine's own write and the object already at the
	// key is left exactly as it was.
	digests := newPutDigests(r, r.Body)
	digests.checkInline(r, h.quotaByteLimit(bucket))

	meta, err := h.writeObject(newObject{
		bucket: bucket,
		key:    key,
		body:   digests,
		size:   r.ContentLength,
		meta: metadata.ObjectMeta{
			ContentType:        ct,
			Tags:               tags,
			UserMetadata:       userMeta,
			ContentEncoding:    r.Header.Get("Content-Encoding"),
			ContentDisposition: r.Header.Get("Content-Disposition"),
			CacheControl:       r.Header.Get("Cache-Control"),
			ContentLanguage:    r.Header.Get("Content-Language"),
			WebsiteRedirect:    r.Header.Get("X-Amz-Website-Redirect-Location"),
		},
		ssec:             ssecKey,
		digests:          digests,
		bypassGovernance: h.governanceBypass(r, bucket, key),
		lockFrom:         r,
	})
	if err != nil {
		answerWriteError(w, err, bucket, key)
		return
	}

	w.Header().Set("ETag", meta.ETag)
	if meta.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", meta.VersionID)
	}
	if ssecKey != nil {
		w.Header().Set(hdrSSECAlgo, "AES256")
		w.Header().Set(hdrSSECKeyMD5, ssecKey.keyMD5)
	} else if h.sseHeaderApplies(bucket) {
		w.Header().Set("X-Amz-Server-Side-Encryption", "AES256")
	}
	setChecksumHeaders(w, &meta)
	w.WriteHeader(http.StatusOK)
	h.notifyCreated("s3:ObjectCreated:Put", meta)
}

// notifyCreated fires the hooks for a new object.
func (h *ObjectHandler) notifyCreated(event string, meta metadata.ObjectMeta) {
	if h.onNotification != nil {
		h.onNotification(event, meta.Bucket, meta.Key, meta.Size, meta.ETag, meta.VersionID)
	}
	if h.onReplication != nil {
		h.onReplication(event, meta.Bucket, meta.Key, meta.Size, meta.ETag, meta.VersionID)
	}
	if h.onLambda != nil {
		h.onLambda(event, meta.Bucket, meta.Key, meta.Size, meta.ETag, meta.VersionID)
	}
	if h.onScan != nil {
		h.onScan(meta.Bucket, meta.Key, meta.Size)
	}
	if h.onSearchUpdate != nil {
		h.onSearchUpdate("put", meta.Bucket, meta.Key)
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
		reader, size, err = h.readObjectData(bucket, key, versionID)
		if err != nil {
			// The version's metadata is here but its bytes may live on another
			// holder that has not been copied to yet (issue #42).
			if h.serveFromDataHolder(w, r, bucket, key) {
				return
			}
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}
		var ok bool
		if reader, size, ok = h.openSSEC(w, r, meta, reader, size); !ok {
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
			reader, size, err = h.readObjectData(bucket, key, meta.VersionID)
			w.Header().Set("X-Amz-Version-Id", meta.VersionID)
		} else {
			reader, size, err = h.engine.GetObject(bucket, key)
		}
		if err == nil {
			// Plaintext size and reader from here on, which is what the stale-copy
			// checks below compare against the metadata.
			var ok bool
			if reader, size, ok = h.openSSEC(w, r, meta, reader, size); !ok {
				return
			}
		}
		if err == nil && !h.localCopyIsStale(meta, size) && h.localCopyNeedsContentCheck(bucket, meta, size) &&
			!h.localCopyContentMatches(reader, meta) {
			// Same byte count, different bytes: an overwrite that kept the size
			// landed on another holder and has not replicated here yet.
			reader.Close()
			slog.Info("local object copy has the right size but the wrong content, routing the read to a holder",
				"bucket", bucket, "key", key)
			if h.serveFromDataHolder(w, r, bucket, key) {
				return
			}
			writeS3Error(w, "SlowDown", "Object data is being replicated, please retry", http.StatusServiceUnavailable)
			return
		}
		if err == nil && h.localCopyIsStale(meta, size) {
			// The bytes here are not the bytes the metadata describes, so serving
			// them would answer with the previous object under the new object's
			// ETag. Ask a holder that has the current data instead.
			reader.Close()
			slog.Info("local object copy is behind its metadata, routing the read to a holder",
				"bucket", bucket, "key", key, "meta_size", meta.Size, "local_size", size)
			if h.serveFromDataHolder(w, r, bucket, key) {
				return
			}
			// No peer could serve the current bytes either. Replication is still in
			// flight, so ask the client to retry rather than hand back data known to
			// be wrong. Every S3 SDK retries this on its own (issue #42).
			writeS3Error(w, "SlowDown", "Object data is being replicated, please retry", http.StatusServiceUnavailable)
			return
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

	// Conditional GET: check preconditions before sending body
	if checkGetPreconditions(w, r, meta) {
		return
	}

	// The Range is read before any header is set, because an ignored one means
	// a full 200 response, checksums included.
	rangeHeader := r.Header.Get("Range")
	rangeStart, rangeEnd, rangeKind := parseRange(rangeHeader, size)
	if rangeHeader != "" && rangeKind == rangeUnsatisfiable && r.URL.Query().Get("partNumber") == "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		writeS3Error(w, "InvalidRange", "The requested range is not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// A whole-object checksum must not be sent on a partial (206) response: modern
	// SDKs (boto3 >= 1.36, aws-cli v2) validate x-amz-checksum-* against the bytes
	// they actually receive, and a whole-object checksum never matches a range or a
	// single part, so range downloads would fail with a checksum mismatch.
	isPartial := rangeKind == rangeSatisfiable || r.URL.Query().Get("partNumber") != ""
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
		setTaggingCountHeader(w, meta)
		setObjectLockHeaders(w, meta)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	if h.sseHeaderApplies(bucket) {
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

	if rangeKind == rangeSatisfiable {
		h.serveRange(w, reader, size, rangeStart, rangeEnd)
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}

// rangeOutcome says what a Range header asks for.
type rangeOutcome int

const (
	rangeIgnored       rangeOutcome = iota // no usable range: serve the whole object
	rangeSatisfiable                       // serve start..end
	rangeUnsatisfiable                     // well formed but outside the object: 416
)

// parseRange reads a Range header against an object of totalSize bytes.
//
// RFC 7233 says a Range that is not a valid single byte range is IGNORED and the
// whole object served with 200. "bytes=5-2", a multi-range list and an unknown
// unit used to be answered 416 instead, which a client asking for a range it
// did not need could not recover from. A valid range that starts past the end
// is still unsatisfiable.
func parseRange(rangeHeader string, totalSize int64) (start, end int64, outcome rangeOutcome) {
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, rangeIgnored
	}
	spec := strings.TrimSpace(strings.TrimPrefix(rangeHeader, "bytes="))
	if strings.Contains(spec, ",") {
		return 0, 0, rangeIgnored
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, rangeIgnored
	}
	if parts[0] == "" {
		// Suffix range: bytes=-500 (last 500 bytes)
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix < 0 {
			return 0, 0, rangeIgnored
		}
		if suffix == 0 || totalSize == 0 {
			return 0, 0, rangeUnsatisfiable
		}
		start = totalSize - suffix
		if start < 0 {
			start = 0
		}
		return start, totalSize - 1, rangeSatisfiable
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 {
		return 0, 0, rangeIgnored
	}
	if parts[1] == "" {
		end = totalSize - 1 // Open-ended: bytes=500-
	} else {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, rangeIgnored
		}
	}
	if start >= totalSize {
		return 0, 0, rangeUnsatisfiable
	}
	if end >= totalSize {
		end = totalSize - 1
	}
	return start, end, rangeSatisfiable
}

// serveRange handles partial content responses for a range parseRange accepted.
func (h *ObjectHandler) serveRange(w http.ResponseWriter, reader storage.ReadSeekCloser, totalSize, start, end int64) {
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
	bypassGov := h.governanceBypass(r, bucket, key)

	del, err := h.deleteOneObject(bucket, key, versionID, versioning, bypassGov)
	if err != nil {
		var refused *deleteRefused
		if errors.As(err, &refused) {
			if !writeReqError(w, refused.err) {
				writeS3Error(w, "AccessDenied", refused.Error(), http.StatusForbidden)
			}
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
		// marker, so the previous null version's data does go. That destroys it,
		// so its lock is checked first. It used to be removed with no lock check
		// at all.
		if err := h.checkReplaceLock(bucket, key, versioning, bypassGovernance); err != nil {
			return objectDeletion{}, &deleteRefused{err: err}
		}
		// A null version written before versioning was turned on has its bytes
		// at the plain object path, not under .vs/.
		preVersioning := false
		if cur, err := h.store.GetObjectMeta(bucket, key); err == nil && cur != nil && cur.VersionID == "" && !cur.DeleteMarker {
			preVersioning = true
		}
		h.engine.DeleteObjectVersion(bucket, key, "null")
		if preVersioning {
			if err := h.engine.DeleteObject(bucket, key); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("could not remove the data of a pre-versioning null version",
					"bucket", bucket, "key", key, "error", err)
			}
		}
		if err := h.store.DeleteObjectVersion(bucket, key, "null"); err != nil {
			return objectDeletion{}, &deleteNotRecorded{op: "DeleteObjectVersion", err: err}
		}
		del, err := h.writeDeleteMarker(bucket, key, "null")
		if err != nil {
			return del, err
		}
		if preVersioning {
			h.reapElsewhere(bucket, key, "")
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
	// The lock is checked before ANY bytes go. The null branch below used to
	// delete first and check after, so a locked null version lost its data
	// while the refusal left its metadata in place.
	if err := h.checkObjectLock(bucket, key, versionID, bypassGovernance); err != nil {
		return objectDeletion{}, &deleteRefused{err: err}
	}
	if versionID == nullVersionID {
		// The null version can also be hidden behind a delete marker, in which
		// case the latest pointer names the marker rather than the object and the
		// branch above does not fire. Its bytes are still at the ordinary object
		// path, so the version-aware delete below would remove a .vs/ file that
		// was never written and leave the real data orphaned. Remove it here.
		if err := h.engine.DeleteObject(bucket, key); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not remove the data of a null version hidden by a delete marker",
				"bucket", bucket, "key", key, "error", err)
		}
	}
	h.engine.DeleteObjectVersion(bucket, key, versionID)
	if err := h.store.DeleteObjectVersion(bucket, key, versionID); err != nil {
		return objectDeletion{}, &deleteNotRecorded{op: "DeleteObjectVersion", err: err}
	}

	// The store makes the newest surviving version current, both the version
	// entry and the latest pointer, in the same transaction as the delete (see
	// metadata.Store.DeleteObjectVersion). Doing it here, after the delete, raced
	// with concurrent deletes of the same key and brought a deleted version back.
	return objectDeletion{VersionID: versionID, Reap: true, ReapVersion: versionID}, nil
}

// writeDeleteMarker hides the object behind a marker, keeping every version.
// No object-lock check: nothing is destroyed, which is also why S3 allows it on
// a locked object.
func (h *ObjectHandler) writeDeleteMarker(bucket, key, markerVersionID string) (objectDeletion, error) {
	if old, err := h.store.GetObjectMeta(bucket, key); err == nil && old != nil {
		// An object written BEFORE versioning was enabled on its bucket carries no
		// version id, and the guard here used to be `old.VersionID != ""`, so such
		// an object was never demoted into a version record. The marker then
		// overwrote the latest pointer, which was the only thing naming it: taking
		// the marker away could not bring the object back, and its bytes sat on
		// disk with nothing referring to them. That is silent data loss on a
		// bucket the user had just asked to keep every version.
		//
		// S3 calls an object that predates versioning the "null" version, so adopt
		// it as one before hiding it. Its bytes stay at the ordinary object path
		// rather than under .vs/, which readObjectData and deleteObjectVersion
		// both account for.
		if old.VersionID == "" {
			if old.DeleteMarker {
				// A marker with no version id is not an object to preserve.
				old = nil
			} else {
				old.VersionID = nullVersionID
			}
		}
		if old != nil {
			old.IsLatest = false
			if err := h.store.PutObjectVersion(*old); err != nil {
				return objectDeletion{}, &deleteNotRecorded{op: "demote previous version", err: err}
			}
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
	if r == nil {
		r = &http.Request{Header: http.Header{}}
	}
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
// If bypassGovernance is true, GOVERNANCE retention is skipped. Callers pass the
// result of governanceBypass, which requires s3:BypassGovernanceRetention.
//
// It refuses with AccessDenied for a lock, and with 503 when the lock cannot be
// read. Any metadata error used to count as "no such object, allow", so a
// metadata store that was briefly unreachable let a locked object be destroyed.
// freshReader is a store that can bring this node up to the cluster leader
// before a read whose answer decides a protection.
type freshReader interface {
	ReadBarrier() error
}

// catchUp makes the next metadata read on this node reflect every write the
// cluster has committed. On a follower the local copy can lag by a moment, and
// a lock decision made on it allowed, for example, a COMPLIANCE retention set
// a moment earlier through another request to be downgraded.
func (h *ObjectHandler) catchUp() {
	if f, ok := h.store.(freshReader); ok {
		if err := f.ReadBarrier(); err != nil {
			slog.Warn("s3: could not catch up with the cluster before a lock decision", "error", err)
		}
	}
}

func (h *ObjectHandler) checkObjectLock(bucket, key, versionID string, bypassGovernance ...bool) error {
	if info, err := h.store.GetBucket(bucket); err == nil && info != nil && info.ObjectLockEnabled {
		h.catchUp()
	}
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
		if isMetaNotFound(err) {
			return nil // nothing recorded under this name, so nothing is locked
		}
		slog.Error("object lock: could not read the lock state, refusing", "bucket", bucket, "key", key,
			"version", versionID, "error", err)
		return &reqError{"ServiceUnavailable", "The object lock state could not be read, please retry", http.StatusServiceUnavailable}
	}
	if meta == nil {
		return nil
	}

	if meta.LegalHold {
		return &reqError{"AccessDenied", "Access Denied because object protected by object lock (legal hold)", http.StatusForbidden}
	}

	if meta.RetentionMode != "" && meta.RetentionUntil > 0 {
		if time.Now().UTC().Unix() < meta.RetentionUntil {
			// Allow governance bypass if requested
			if meta.RetentionMode == "GOVERNANCE" && len(bypassGovernance) > 0 && bypassGovernance[0] {
				return nil
			}
			return &reqError{"AccessDenied", fmt.Sprintf("Access Denied because object protected by object lock (%s retention until %s)",
				meta.RetentionMode, time.Unix(meta.RetentionUntil, 0).UTC().Format(time.RFC3339)), http.StatusForbidden}
		}
	}

	return nil
}

// governanceBypass reports whether this request may bypass GOVERNANCE
// retention. It has to ask, with x-amz-bypass-governance-retention: true, and
// the caller has to hold s3:BypassGovernanceRetention on the object. The header
// used to be honoured on its own, so anyone allowed to delete an object could
// delete it through a GOVERNANCE lock, which is the one thing that mode exists
// to require a separate permission for.
func (h *ObjectHandler) governanceBypass(r *http.Request, bucket, key string) bool {
	if r == nil || !strings.EqualFold(r.Header.Get("X-Amz-Bypass-Governance-Retention"), "true") {
		return false
	}
	return h.authorizeEntry(r, "s3:BypassGovernanceRetention", formatResource(bucket, key)) == nil
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
	setTaggingCountHeader(w, meta)
	setObjectLockHeaders(w, meta)
	if meta.SSECustomerKeyMD5 != "" {
		w.Header().Set(hdrSSECAlgo, "AES256")
		w.Header().Set(hdrSSECKeyMD5, meta.SSECustomerKeyMD5)
	} else if h.sseHeaderApplies(bucket) {
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

	// The same parser the router authorized the source with, so the object read
	// here is the one that was authorized.
	src, perr := parseCopySourceHeader(r.Header.Get("X-Amz-Copy-Source"))
	if perr != nil {
		writeS3Error(w, "InvalidArgument", "Invalid x-amz-copy-source", http.StatusBadRequest)
		return
	}

	// The destination's own SSE-C key, if it is to be sealed with one.
	destKey, derr := parseSSECHeaders(r)
	if derr != nil {
		writeS3Error(w, "InvalidArgument", derr.Error(), http.StatusBadRequest)
		return
	}

	srcMeta, reader, size, ok := h.openCopySource(w, r, src)
	if !ok {
		return
	}
	defer reader.Close()

	// Check conditional copy preconditions
	if checkCopyPreconditions(w, r, srcMeta) {
		return
	}

	// The tag set is governed by its own directive, independently of the metadata
	// one: S3 lets a copy replace the metadata while keeping the source's tags, and
	// the other way round. Deciding tags inside the metadata branch got both halves
	// wrong. "Replace the metadata" with no tagging header silently DISCARDED the
	// source's tags, which is data loss on an ordinary copy, and a tagging directive
	// of REPLACE did nothing at all unless the metadata directive happened to say
	// REPLACE too.
	var copyTags map[string]string
	if strings.EqualFold(r.Header.Get("X-Amz-Tagging-Directive"), "REPLACE") {
		headerTags, tagErr := parseInlineTags(r)
		if tagErr != nil {
			writeTagError(w, tagErr)
			return
		}
		copyTags = headerTags
	} else {
		copyTags = srcMeta.Tags
	}

	// Determine metadata: REPLACE uses request headers, COPY (default) uses source
	var meta metadata.ObjectMeta
	if strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE") {
		meta.ContentType = detectContentType(r, key)
		meta.UserMetadata = parseUserMetadata(r)
		meta.ContentEncoding = r.Header.Get("Content-Encoding")
		meta.ContentDisposition = r.Header.Get("Content-Disposition")
		meta.CacheControl = r.Header.Get("Cache-Control")
		meta.ContentLanguage = r.Header.Get("Content-Language")
		meta.WebsiteRedirect = r.Header.Get("X-Amz-Website-Redirect-Location")
	} else {
		meta.ContentType = srcMeta.ContentType
		meta.UserMetadata = srcMeta.UserMetadata
		meta.ContentEncoding = srcMeta.ContentEncoding
		meta.ContentDisposition = srcMeta.ContentDisposition
		meta.CacheControl = srcMeta.CacheControl
		meta.ContentLanguage = srcMeta.ContentLanguage
		meta.WebsiteRedirect = srcMeta.WebsiteRedirect
	}
	// The checksums describe the bytes, which a copy keeps whatever the
	// directive says.
	meta.ChecksumSHA256 = srcMeta.ChecksumSHA256
	meta.ChecksumCRC32 = srcMeta.ChecksumCRC32
	meta.ChecksumCRC32C = srcMeta.ChecksumCRC32C
	meta.ChecksumSHA1 = srcMeta.ChecksumSHA1
	meta.Tags = copyTags

	written, err := h.writeObject(newObject{
		bucket:           bucket,
		key:              key,
		body:             reader,
		size:             size,
		meta:             meta,
		ssec:             destKey,
		bypassGovernance: h.governanceBypass(r, bucket, key),
		lockFrom:         r,
	})
	if err != nil {
		answerWriteError(w, err, bucket, key)
		return
	}

	type copyResult struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		ETag         string   `xml:"ETag"`
		LastModified string   `xml:"LastModified"`
	}

	if srcMeta.VersionID != "" {
		w.Header().Set("X-Amz-Copy-Source-Version-Id", srcMeta.VersionID)
	}
	if written.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", written.VersionID)
	}
	if destKey != nil {
		w.Header().Set(hdrSSECAlgo, "AES256")
		w.Header().Set(hdrSSECKeyMD5, destKey.keyMD5)
	}
	writeXML(w, http.StatusOK, copyResult{
		ETag:         written.ETag,
		LastModified: time.Unix(written.LastModified, 0).UTC().Format(time.RFC3339),
	})
	h.notifyCreated("s3:ObjectCreated:Copy", written)
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

	var result deleteResult
	// Keys whose data must also be dropped on the other nodes. This request was
	// routed by hash(bucket, "") to a single node, so the engine delete below only
	// frees the keys that happen to live here; the rest sit on the other nodes and
	// become unreachable orphans the moment the Raft-replicated metadata is gone.
	// That is how a delete-heavy workload grew to ~9x its logical size (issue #47).
	var reaped []string
	for _, obj := range req.Objects {
		// The keys come from the body, so the router never checked them. An
		// empty <Key></Key> used to reach the engine as a delete of "".
		if obj.Key == "" {
			result.Errors = append(result.Errors, deleteError{
				Key:     obj.Key,
				Code:    "UserKeyMustBeSpecified",
				Message: "The request was missing a required key",
			})
			continue
		}
		if msg := objectKeyProblem(obj.Key); msg != "" {
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
			slog.Info("s3: multi-object delete entry refused by policy", "key", obj.Key, "reason", err.Error())
			result.Errors = append(result.Errors, deleteError{
				Key:     obj.Key,
				Code:    "AccessDenied",
				Message: "Access Denied",
			})
			continue
		}

		del, err := h.deleteOneObject(bucket, obj.Key, obj.VersionID, versioning, h.governanceBypass(r, bucket, obj.Key))
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
		var re *reqError
		if errors.As(refused.err, &re) {
			entry.Code, entry.Message = re.code, re.msg
		}
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

// taggingTarget resolves the object version a tagging request acts on, through
// the metadata store, which is where tags live.
//
// Tagging used to gate on the local engine having a file at the plain object
// path. An object in a versioned bucket keeps its bytes under .vs/, so every
// one of them answered NoSuchKey, and on a cluster so did every node that does
// not hold the bytes, even though HEAD and GET answered on all of them.
//
// On failure the response has been written and ok is false.
func (h *ObjectHandler) taggingTarget(w http.ResponseWriter, r *http.Request, bucket, key string) (*metadata.ObjectMeta, bool) {
	versionID := r.URL.Query().Get("versionId")
	if versionID != "" {
		meta, err := h.store.GetObjectVersion(bucket, key, versionID)
		if err != nil && versionID == nullVersionID {
			// An object written before versioning was turned on is the null
			// version, recorded only in the latest pointer.
			if cur, cerr := h.store.GetObjectMeta(bucket, key); cerr == nil && cur != nil && cur.VersionID == "" {
				meta, err = cur, nil
			}
		}
		if err != nil || meta == nil {
			if metadataUnavailable(w, err) {
				return nil, false
			}
			writeS3Error(w, "NoSuchVersion", "The specified version does not exist", http.StatusNotFound)
			return nil, false
		}
		if meta.DeleteMarker {
			writeS3Error(w, "MethodNotAllowed", "The specified method is not allowed against a delete marker", http.StatusMethodNotAllowed)
			return nil, false
		}
		return meta, true
	}
	meta, err := h.store.GetObjectMetaConsistent(bucket, key)
	if meta == nil && metadataUnavailable(w, err) {
		return nil, false
	}
	if meta == nil || meta.DeleteMarker {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return nil, false
	}
	return meta, true
}

// saveTagging records a version's changed tags. A versioned record is updated
// in the version index, which also moves the latest pointer when it is the
// current version.
func (h *ObjectHandler) saveTagging(meta *metadata.ObjectMeta) error {
	if meta.VersionID == "" {
		return h.store.PutObjectMeta(*meta)
	}
	if !meta.IsLatest {
		// The latest pointer carries its own copy of the record, so a request
		// that named the current version by id still has to move it.
		if cur, err := h.store.GetObjectMeta(meta.Bucket, meta.Key); err == nil && cur != nil && cur.VersionID == meta.VersionID {
			meta.IsLatest = true
		}
	}
	return h.store.UpdateObjectVersionMeta(*meta)
}

// PutObjectTagging handles PUT /{bucket}/{key}?tagging.
func (h *ObjectHandler) PutObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	h.catchUp()
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	var req taggingRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse tagging XML", http.StatusBadRequest)
		return
	}

	if err := checkTagSet(req.TagSet.Tags); err != nil {
		writeTagError(w, err)
		return
	}

	meta, ok := h.taggingTarget(w, r, bucket, key)
	if !ok {
		return
	}

	meta.Tags = make(map[string]string, len(req.TagSet.Tags))
	for _, tag := range req.TagSet.Tags {
		meta.Tags[tag.Key] = tag.Value
	}

	if err := h.saveTagging(meta); err != nil {
		metaWriteFailed(w, err, "PutObjectTagging", bucket, key)
		return
	}

	if meta.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", meta.VersionID)
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

	meta, ok := h.taggingTarget(w, r, bucket, key)
	if !ok {
		return
	}

	resp := taggingResponse{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	resp.TagSet.Tags = sortedTags(meta.Tags)

	if meta.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", meta.VersionID)
	}
	writeXML(w, http.StatusOK, resp)
}

// DeleteObjectTagging handles DELETE /{bucket}/{key}?tagging.
func (h *ObjectHandler) DeleteObjectTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	h.catchUp()
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	meta, ok := h.taggingTarget(w, r, bucket, key)
	if !ok {
		return
	}

	meta.Tags = nil
	if err := h.saveTagging(meta); err != nil {
		metaWriteFailed(w, err, "DeleteObjectTagging", bucket, key)
		return
	}
	if meta.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", meta.VersionID)
	}
	w.WriteHeader(http.StatusNoContent)
	if h.onSearchUpdate != nil {
		h.onSearchUpdate("put", bucket, key)
	}
}

// readObjectData opens the bytes for the version the metadata names.
//
// The "null" version is the special case. It names an object stored before its
// bucket was versioned, whose data lives at the ordinary object path and NOT
// under .vs/<key>/<version>, so asking the engine for it as a version finds
// nothing. Falling back keeps a restored pre-versioning object readable.
func (h *ObjectHandler) readObjectData(bucket, key, versionID string) (storage.ReadSeekCloser, int64, error) {
	if versionID == "" {
		return h.engine.GetObject(bucket, key)
	}
	reader, size, err := h.engine.GetObjectVersion(bucket, key, versionID)
	if err != nil && versionID == nullVersionID {
		return h.engine.GetObject(bucket, key)
	}
	return reader, size, err
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
	// The store resumes from a (key, version) position in its own index order,
	// so the next page has to start after the last entry IT returned. Within a
	// key that order is not the newest-first order the response is sorted
	// into, and the last entry shown can be one the store has already gone past.
	var storeLast *metadata.ObjectMeta
	if truncated && len(versions) > 0 {
		last := versions[len(versions)-1]
		storeLast = &last
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
		XMLName         xml.Name `xml:"ListVersionsResult"`
		Xmlns           string   `xml:"xmlns,attr"`
		Name            string   `xml:"Name"`
		Prefix          string   `xml:"Prefix,omitempty"`
		KeyMarker       string   `xml:"KeyMarker"`
		VersionIdMarker string   `xml:"VersionIdMarker"`
		MaxKeys         int      `xml:"MaxKeys"`
		IsTruncated     bool     `xml:"IsTruncated"`
		// The two Next markers are where the following page starts. They were
		// never sent, so an SDK paginator stopped after the first page and
		// every version past it was invisible to it.
		NextKeyMarker       string            `xml:"NextKeyMarker,omitempty"`
		NextVersionIdMarker string            `xml:"NextVersionIdMarker,omitempty"`
		Versions            []xmlVersion      `xml:"Version,omitempty"`
		DeleteMarkers       []xmlDeleteMarker `xml:"DeleteMarker,omitempty"`
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
	if truncated && len(versions) > 0 {
		last := versions[len(versions)-1]
		if storeLast != nil {
			for _, v := range versions {
				if v.Key == storeLast.Key && v.VersionID == storeLast.VersionID {
					last = *storeLast
					break
				}
			}
		}
		resp.NextKeyMarker = last.Key
		resp.NextVersionIdMarker = last.VersionID
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
