package s3

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

type ObjectHandler struct {
	store             *metadata.Store
	engine            storage.Engine
	encryptionEnabled bool
	onNotification    NotificationFunc
	onReplication     ReplicationFunc
	onScan            ScanFunc
	onSearchUpdate    SearchUpdateFunc
	onLambda          LambdaFunc
	accessUpdater     *metadata.AccessUpdater
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

	// Conditional PUT: check If-Match / If-None-Match
	if checkPutPreconditions(w, r, h.store, bucket, key) {
		return
	}

	// Read body for Content-MD5 and checksum validation
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeS3Error(w, "InternalError", "Failed to read request body", http.StatusInternalServerError)
		return
	}

	// Validate Content-MD5 if present
	if validateContentMD5(w, r.Header.Get("Content-MD5"), body) {
		return
	}

	// Validate and compute S3 checksums
	csha256, ccrc32, ccrc32c, csha1, checksumErr := checksumFromRequest(r, body)
	if checksumErr != nil {
		writeS3Error(w, "BadDigest", checksumErr.Error(), http.StatusBadRequest)
		return
	}

	versioning, _ := h.store.GetBucketVersioning(bucket)
	ct := detectContentType(r, key)
	now := time.Now().UTC()

	// Parse extended metadata from headers
	userMeta := parseUserMetadata(r)
	tags := parseInlineTags(r)

	if versioning == "Enabled" {
		versionID := generateVersionID()

		written, etag, err := h.engine.PutObjectVersion(bucket, key, versionID, bytes.NewReader(body), int64(len(body)))
		if err != nil {
			slog.Error("internal error", "error", err)
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
			return
		}

		// Mark previous latest as not latest
		if oldMeta, err := h.store.GetObjectMeta(bucket, key); err == nil && oldMeta.VersionID != "" {
			oldMeta.IsLatest = false
			h.store.PutObjectVersion(*oldMeta)
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

		// Apply inline retention
		if mode := r.Header.Get("X-Amz-Object-Lock-Mode"); mode != "" {
			meta.RetentionMode = mode
			if until := r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"); until != "" {
				if t, err := time.Parse(time.RFC3339, until); err == nil {
					meta.RetentionUntil = t.Unix()
				}
			}
		}

		// Apply bucket default retention if configured and no inline retention
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

		h.store.PutObjectVersion(meta)
		h.store.PutObjectMeta(meta) // update "latest pointer"

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
		written, etag, err := h.engine.PutObjectVersion(bucket, key, "null", bytes.NewReader(body), int64(len(body)))
		if err != nil {
			slog.Error("internal error", "error", err)
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
			return
		}

		// Remove any existing null version
		if oldMeta, err := h.store.GetObjectVersion(bucket, key, "null"); err == nil {
			oldMeta.IsLatest = false
			h.store.PutObjectVersion(*oldMeta)
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

		h.store.PutObjectVersion(meta)
		h.store.PutObjectMeta(meta)

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

	// Non-versioned path
	written, etag, err := h.engine.PutObject(bucket, key, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
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

	h.store.PutObjectMeta(meta)

	w.Header().Set("ETag", etag)
	if h.encryptionEnabled {
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
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Amz-Version-Id", versionID)
	} else {
		// Get latest version
		meta, _ = h.store.GetObjectMeta(bucket, key)
		if meta != nil && meta.DeleteMarker {
			w.Header().Set("X-Amz-Delete-Marker", "true")
			if meta.VersionID != "" {
				w.Header().Set("X-Amz-Version-Id", meta.VersionID)
			}
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}

		if meta != nil && meta.VersionID != "" {
			// Versioned bucket — read from version storage
			reader, size, err = h.engine.GetObjectVersion(bucket, key, meta.VersionID)
			w.Header().Set("X-Amz-Version-Id", meta.VersionID)
		} else {
			reader, size, err = h.engine.GetObject(bucket, key)
		}
		if err != nil {
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}
	}
	defer reader.Close()

	// Conditional GET: check preconditions before sending body
	if checkGetPreconditions(w, r, meta) {
		return
	}

	if meta != nil {
		w.Header().Set("Content-Type", meta.ContentType)
		w.Header().Set("ETag", meta.ETag)
		w.Header().Set("Last-Modified", time.Unix(meta.LastModified, 0).UTC().Format(http.TimeFormat))
		setHTTPMetadataHeaders(w, meta)
		setUserMetadataHeaders(w, meta)
		setChecksumHeaders(w, meta)
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

	if versionID != "" {
		// Delete specific version permanently
		// Check object lock first (with governance bypass if header present)
		bypassGov := strings.EqualFold(r.Header.Get("X-Amz-Bypass-Governance-Retention"), "true")
		if err := h.checkObjectLock(bucket, key, versionID, bypassGov); err != nil {
			writeS3Error(w, "AccessDenied", err.Error(), http.StatusForbidden)
			return
		}

		h.engine.DeleteObjectVersion(bucket, key, versionID)
		h.store.DeleteObjectVersion(bucket, key, versionID)

		// If we deleted the latest, find the new latest
		versions, _, _ := h.store.ListObjectVersions(bucket, key, "", "", 1)
		if len(versions) > 0 {
			// There's still a version — make it latest
			versions[0].IsLatest = true
			h.store.UpdateObjectVersionMeta(versions[0])
		} else {
			// No versions left — remove from objects bucket
			h.store.DeleteObjectMeta(bucket, key)
		}

		w.Header().Set("X-Amz-Version-Id", versionID)
		w.WriteHeader(http.StatusNoContent)
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
		return
	}

	if versioning == "Enabled" {
		// Create a delete marker instead of actually deleting
		dmVersionID := generateVersionID()

		// Mark previous latest as not latest
		if oldMeta, err := h.store.GetObjectMeta(bucket, key); err == nil && oldMeta.VersionID != "" {
			oldMeta.IsLatest = false
			h.store.PutObjectVersion(*oldMeta)
		}

		dm := metadata.ObjectMeta{
			Bucket:       bucket,
			Key:          key,
			VersionID:    dmVersionID,
			IsLatest:     true,
			DeleteMarker: true,
			LastModified: time.Now().UTC().Unix(),
		}
		h.store.PutObjectVersion(dm)
		h.store.PutObjectMeta(dm) // latest pointer now points to delete marker

		w.Header().Set("X-Amz-Delete-Marker", "true")
		w.Header().Set("X-Amz-Version-Id", dmVersionID)
		w.WriteHeader(http.StatusNoContent)
		if h.onNotification != nil {
			h.onNotification("s3:ObjectRemoved:Delete", bucket, key, 0, "", dmVersionID)
		}
		if h.onReplication != nil {
			h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", dmVersionID)
		}
		if h.onLambda != nil {
			h.onLambda("s3:ObjectRemoved:Delete", bucket, key, 0, "", dmVersionID)
		}
		if h.onSearchUpdate != nil {
			h.onSearchUpdate("delete", bucket, key)
		}
		return
	}

	if versioning == "Suspended" {
		// Suspended versioning: create a null-version delete marker
		// Remove existing null version if any
		h.engine.DeleteObjectVersion(bucket, key, "null")
		h.store.DeleteObjectVersion(bucket, key, "null")

		dm := metadata.ObjectMeta{
			Bucket:       bucket,
			Key:          key,
			VersionID:    "null",
			IsLatest:     true,
			DeleteMarker: true,
			LastModified: time.Now().UTC().Unix(),
		}
		h.store.PutObjectVersion(dm)
		h.store.PutObjectMeta(dm)

		w.Header().Set("X-Amz-Delete-Marker", "true")
		w.Header().Set("X-Amz-Version-Id", "null")
		w.WriteHeader(http.StatusNoContent)
		if h.onNotification != nil {
			h.onNotification("s3:ObjectRemoved:Delete", bucket, key, 0, "", "null")
		}
		if h.onReplication != nil {
			h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", "null")
		}
		if h.onLambda != nil {
			h.onLambda("s3:ObjectRemoved:Delete", bucket, key, 0, "", "null")
		}
		if h.onSearchUpdate != nil {
			h.onSearchUpdate("delete", bucket, key)
		}
		return
	}

	// Non-versioned: delete normally
	if err := h.engine.DeleteObject(bucket, key); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	h.store.DeleteObjectMeta(bucket, key)
	w.WriteHeader(http.StatusNoContent)
	if h.onNotification != nil {
		h.onNotification("s3:ObjectRemoved:Delete", bucket, key, 0, "", "")
	}
	if h.onReplication != nil {
		h.onReplication("s3:ObjectRemoved:Delete", bucket, key, 0, "", "")
	}
	if h.onLambda != nil {
		h.onLambda("s3:ObjectRemoved:Delete", bucket, key, 0, "", "")
	}
	if h.onSearchUpdate != nil {
		h.onSearchUpdate("delete", bucket, key)
	}
}

// checkObjectLock checks if an object version is locked (legal hold or retention).
// If bypassGovernance is true, GOVERNANCE retention is skipped (requires s3:BypassGovernanceRetention).
func (h *ObjectHandler) checkObjectLock(bucket, key, versionID string, bypassGovernance ...bool) error {
	meta, err := h.store.GetObjectVersion(bucket, key, versionID)
	if err != nil {
		return nil // version doesn't exist in metadata, allow delete
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
		meta, _ = h.store.GetObjectMeta(bucket, key)
		if meta != nil && meta.DeleteMarker {
			w.Header().Set("X-Amz-Delete-Marker", "true")
			if meta.VersionID != "" {
				w.Header().Set("X-Amz-Version-Id", meta.VersionID)
			}
			writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
			return
		}
		if meta == nil {
			if !h.engine.ObjectExists(bucket, key) {
				writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
				return
			}
			size, err := h.engine.ObjectSize(bucket, key)
			if err != nil {
				slog.Error("internal error", "error", err)
				writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
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
	if h.encryptionEnabled {
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

	h.store.PutObjectMeta(meta)

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

// BatchDelete handles POST /{bucket}?delete.
func (h *ObjectHandler) BatchDelete(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	var req deleteRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse request body", http.StatusBadRequest)
		return
	}

	versioning, _ := h.store.GetBucketVersioning(bucket)

	var result deleteResult
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

		// Check object lock for versioned objects
		if versioning == "Enabled" {
			if meta, err := h.store.GetObjectMeta(bucket, obj.Key); err == nil && meta.VersionID != "" {
				if lockErr := h.checkObjectLock(bucket, obj.Key, meta.VersionID); lockErr != nil {
					result.Errors = append(result.Errors, deleteError{
						Key:     obj.Key,
						Code:    "AccessDenied",
						Message: lockErr.Error(),
					})
					continue
				}
			}
		}

		err := h.engine.DeleteObject(bucket, obj.Key)
		if err != nil {
			result.Errors = append(result.Errors, deleteError{
				Key:     obj.Key,
				Code:    "InternalError",
				Message: err.Error(),
			})
		} else {
			h.store.DeleteObjectMeta(bucket, obj.Key)
			if !req.Quiet {
				result.Deleted = append(result.Deleted, deletedObject{Key: obj.Key})
			}
			if h.onNotification != nil {
				h.onNotification("s3:ObjectRemoved:Delete", bucket, obj.Key, 0, "", "")
			}
			if h.onReplication != nil {
				h.onReplication("s3:ObjectRemoved:Delete", bucket, obj.Key, 0, "", "")
			}
			if h.onLambda != nil {
				h.onLambda("s3:ObjectRemoved:Delete", bucket, obj.Key, 0, "", "")
			}
			if h.onSearchUpdate != nil {
				h.onSearchUpdate("delete", bucket, obj.Key)
			}
		}
	}

	writeXML(w, http.StatusOK, result)
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
	h.store.PutObjectMeta(*meta)
	w.WriteHeader(http.StatusNoContent)
	if h.onSearchUpdate != nil {
		h.onSearchUpdate("put", bucket, key)
	}
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
func (h *ObjectHandler) ListObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	prefix := r.URL.Query().Get("prefix")
	startAfter := r.URL.Query().Get("start-after")
	maxKeysStr := r.URL.Query().Get("max-keys")
	maxKeys := 1000
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 && mk <= 1000 {
			maxKeys = mk
		}
	}

	objects, truncated, err := h.engine.ListObjects(bucket, prefix, startAfter, maxKeys)
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
	type xmlResponse struct {
		XMLName     xml.Name     `xml:"ListBucketResult"`
		Xmlns       string       `xml:"xmlns,attr"`
		Name        string       `xml:"Name"`
		Prefix      string       `xml:"Prefix"`
		MaxKeys     int          `xml:"MaxKeys"`
		IsTruncated bool         `xml:"IsTruncated"`
		Contents    []xmlContent `xml:"Contents"`
		KeyCount    int          `xml:"KeyCount"`
	}

	resp := xmlResponse{
		Xmlns:       "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:        bucket,
		Prefix:      prefix,
		MaxKeys:     maxKeys,
		IsTruncated: truncated,
		KeyCount:    len(objects),
	}

	for _, obj := range objects {
		resp.Contents = append(resp.Contents, xmlContent{
			Key:          obj.Key,
			LastModified: time.Unix(obj.LastModified, 0).UTC().Format(time.RFC3339),
			ETag:         obj.ETag,
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
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
	objects, truncated, err := h.engine.ListObjects(bucket, prefix, marker, maxKeys)
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
