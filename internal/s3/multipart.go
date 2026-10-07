package s3

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// validUploadID ensures uploadID is hex-only to prevent path traversal.
var validUploadID = regexp.MustCompile(`^[a-f0-9]+$`)

// CreateMultipartUpload handles POST /{bucket}/{key}?uploads.
func (h *ObjectHandler) CreateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	// Refuse a bad tag set here rather than at completion, so the client learns
	// about it before uploading the parts.
	tags, tagErr := parseInlineTags(r)
	if tagErr != nil {
		writeTagError(w, tagErr)
		return
	}

	uploadID := generateUploadID()

	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}

	// Everything the object should end up with has to be recorded now: completion
	// runs from this record, not from the request that started the upload.
	upload := metadata.MultipartUpload{
		UploadID:           uploadID,
		Bucket:             bucket,
		Key:                key,
		ContentType:        ct,
		CreatedAt:          time.Now().UTC().Unix(),
		Tags:               tags,
		UserMetadata:       parseUserMetadata(r),
		ContentEncoding:    r.Header.Get("Content-Encoding"),
		ContentDisposition: r.Header.Get("Content-Disposition"),
		CacheControl:       r.Header.Get("Cache-Control"),
		ContentLanguage:    r.Header.Get("Content-Language"),
		WebsiteRedirect:    r.Header.Get("X-Amz-Website-Redirect-Location"),
		LockMode:           r.Header.Get("X-Amz-Object-Lock-Mode"),
		LockRetainUntil:    r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"),
		LockLegalHold:      r.Header.Get("X-Amz-Object-Lock-Legal-Hold"),
	}

	if err := h.multipartStore().CreateMultipartUpload(upload); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	partsDir := h.multipartDir(uploadID)
	if err := os.MkdirAll(partsDir, 0755); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	type initResult struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Xmlns    string   `xml:"xmlns,attr"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadId string   `xml:"UploadId"`
	}

	writeXML(w, http.StatusOK, initResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:   bucket,
		Key:      key,
		UploadId: uploadID,
	})
}

// requireUpload resolves an upload ID for a request that names one. It returns
// the record and true when this node holds it and should serve the request
// locally. It returns false when the caller must stop, either because a peer
// holding the upload has already answered, or because the upload genuinely does
// not exist anywhere and a NoSuchUpload has been written.
//
// Every multipart handler goes through here so none of them can reintroduce the
// bug where a node that lacks the record answers NoSuchUpload for an upload that
// is alive on a different node (issue #47 bug B).
//
// The upload must also belong to the bucket and key in the URL. That was never
// compared, so an upload id was a capability for whatever object it was started
// for: a caller authorized only on its own key could complete, abort or add
// parts to someone else's upload by naming it under that key, and the object
// was then written under the upload's key, not the authorized one.
func (h *ObjectHandler) requireUpload(w http.ResponseWriter, r *http.Request, uploadID, bucket, key string) (*metadata.MultipartUpload, bool) {
	upload, err := h.multipartStore().GetMultipartUpload(uploadID)
	if err == nil {
		// A record written before uploads carried their bucket and key cannot
		// be checked, and is honoured as it always was.
		if (upload.Bucket != "" || upload.Key != "") && (upload.Bucket != bucket || upload.Key != key) {
			writeS3Error(w, "NoSuchUpload", "Upload not found", http.StatusNotFound)
			return nil, false
		}
		return upload, true
	}
	if h.multipartHolder != nil && h.multipartHolder(w, r, uploadID) {
		return nil, false
	}
	writeS3Error(w, "NoSuchUpload", "Upload not found", http.StatusNotFound)
	return nil, false
}

// lockUpload serializes the requests that end an upload. Two concurrent
// CompleteMultipartUpload calls for one upload used to assemble into the same
// fixed temp file at once and interleave their bytes, and an abort could remove
// the parts from under a completion halfway through assembling them.
func lockUpload(uploadID string) func() {
	return lockObjectKey("\x00multipart", uploadID)
}

// UploadPart handles PUT /{bucket}/{key}?partNumber=N&uploadId=X.
func (h *ObjectHandler) UploadPart(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	if _, ok := h.requireUpload(w, r, uploadID, bucket, key); !ok {
		return
	}

	partNumStr := r.URL.Query().Get("partNumber")
	partNum, err := strconv.Atoi(partNumStr)
	if err != nil || partNum < 1 || partNum > 10000 {
		writeS3Error(w, "InvalidArgument", "Invalid part number", http.StatusBadRequest)
		return
	}

	// Enforce max part size (5GB per S3 spec)
	const maxPartSize int64 = 5 * 1024 * 1024 * 1024
	if r.ContentLength > maxPartSize {
		writeS3Error(w, "EntityTooLarge", "Part size exceeds 5GB limit", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPartSize)

	// A part is checked against its Content-MD5 and x-amz-checksum-* the same
	// way a whole object is, inside the reader, so a part that arrives damaged
	// never replaces a good copy of itself. Neither was checked at all.
	digests := newPutDigests(r, r.Body)
	digests.checkInline(r, -1)

	written, etag, ok := h.writePart(w, digests, uploadID, partNum)
	if !ok {
		return
	}

	// The part is only usable once it is recorded, so a failed record must not
	// be answered with 200. It used to be ignored, and the completion then
	// failed with InvalidPart for a part the client had been told was stored.
	if err := h.multipartStore().PutPart(uploadID, metadata.PartInfo{
		PartNumber: partNum,
		ETag:       etag,
		Size:       written,
	}); err != nil {
		metaWriteFailed(w, err, "PutPart", bucket, key)
		return
	}

	w.Header().Set("ETag", etag)
	setChecksumHeaders(w, &metadata.ObjectMeta{ChecksumSHA256: digests.sums.SHA256, ChecksumCRC32: digests.sums.CRC32,
		ChecksumCRC32C: digests.sums.CRC32C, ChecksumSHA1: digests.sums.SHA1})
	w.WriteHeader(http.StatusOK)
}

// writePart streams one part to disk and returns its size and ETag.
//
// The write goes to a temp file that is renamed into place only once it is
// complete, so re-uploading a part is non-destructive. Writing straight to the
// part path meant os.Create truncated whatever was already there and the error
// path deleted it outright, so a RETRY of a part that had already succeeded
// destroyed the good data while the earlier success's metadata survived. The
// upload was then permanently un-completable: ListParts still advertised the
// part, CompleteMultipartUpload could not open it and answered InvalidPart, and
// no number of retries recovered (issue #48). Any failed transfer was enough to
// trigger it, which is why it tracked dropped connections and memory pressure
// rather than data, and a retrying client or proxy made it routine.
//
// Returns ok=false when it has already written an error response.
func (h *ObjectHandler) writePart(w http.ResponseWriter, body io.Reader, uploadID string, partNum int) (int64, string, bool) {
	dir := h.multipartDir(uploadID)
	partPath := filepath.Join(dir, fmt.Sprintf("part-%05d", partNum))

	tmp, err := os.CreateTemp(dir, fmt.Sprintf(".part-%05d-*", partNum))
	if err != nil {
		slog.Error("multipart: could not create a temp file for a part",
			"upload", uploadID, "part", partNum, "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return 0, "", false
	}
	tmpPath := tmp.Name()

	hash := md5.New()
	written, err := io.Copy(tmp, io.TeeReader(body, hash))
	if err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		// Only the temp file goes; any previously uploaded copy of this part is
		// left exactly as it was.
		slog.Warn("multipart: part upload failed mid-transfer, any previously uploaded copy of this part is untouched",
			"upload", uploadID, "part", partNum, "error", err)
		if !writeReqError(w, err) {
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		}
		return 0, "", false
	}
	// Close before renaming, and check it: a deferred close would discard a
	// write error that only surfaces on flush, leaving a short part on disk that
	// the client believes was stored.
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		slog.Error("multipart: part could not be flushed to disk",
			"upload", uploadID, "part", partNum, "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return 0, "", false
	}
	if err := os.Rename(tmpPath, partPath); err != nil {
		os.Remove(tmpPath)
		slog.Error("multipart: part could not be moved into place",
			"upload", uploadID, "part", partNum, "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return 0, "", false
	}

	return written, fmt.Sprintf("\"%s\"", hex.EncodeToString(hash.Sum(nil))), true
}

// completePart is one entry of a CompleteMultipartUpload part list.
type completePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// normalizeETag compares ETags the way S3 does: quotes are optional and hex is
// case-insensitive.
func normalizeETag(e string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(e), `"`))
}

// checkCompleteParts validates a completion's part list against the parts that
// were uploaded. It returns the S3 error to answer, or nil.
//
// The list used to be trusted as given: a part named twice was concatenated
// twice, an empty list produced a 0-byte object, a part's ETag was never
// compared with the part that was stored, and an unsorted list was quietly
// sorted. Every one of those stores an object the client did not describe.
func checkCompleteParts(parts []completePart, stored []metadata.PartInfo) *reqError {
	if len(parts) == 0 {
		return &reqError{"MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema", http.StatusBadRequest}
	}
	byNumber := make(map[int]metadata.PartInfo, len(stored))
	for _, p := range stored {
		byNumber[p.PartNumber] = p
	}
	prev := 0
	for _, p := range parts {
		if p.PartNumber < 1 || p.PartNumber > 10000 {
			return &reqError{"InvalidPart", fmt.Sprintf("Part number %d is out of range", p.PartNumber), http.StatusBadRequest}
		}
		if p.PartNumber <= prev {
			return &reqError{"InvalidPartOrder", "The list of parts was not in ascending order. Parts must be ordered by part number.", http.StatusBadRequest}
		}
		prev = p.PartNumber
		got, ok := byNumber[p.PartNumber]
		if !ok || normalizeETag(p.ETag) == "" || normalizeETag(got.ETag) != normalizeETag(p.ETag) {
			return &reqError{"InvalidPart", fmt.Sprintf("Part %d not found, or its ETag does not match the part that was uploaded", p.PartNumber), http.StatusBadRequest}
		}
	}
	return nil
}

// CompleteMultipartUpload handles POST /{bucket}/{key}?uploadId=X.
func (h *ObjectHandler) CompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	if _, ok := h.requireUpload(w, r, uploadID, bucket, key); !ok {
		return
	}
	unlock := lockUpload(uploadID)
	defer unlock()
	// A completion or abort that held the lock before this one may have ended
	// the upload, so read it again now that nothing else can.
	upload, err := h.multipartStore().GetMultipartUpload(uploadID)
	if err != nil {
		writeS3Error(w, "NoSuchUpload", "Upload not found", http.StatusNotFound)
		return
	}

	type completeRequest struct {
		XMLName xml.Name       `xml:"CompleteMultipartUpload"`
		Parts   []completePart `xml:"Part"`
	}

	// S3 allows up to 10,000 parts, so the CompleteMultipartUpload part list can be
	// a few MB (each <Part> carries a number, an ETag, and optional checksum
	// fields). The old 256KB cap silently truncated the body for large uploads
	// (~2,000+ parts), so xml.Decode failed with "MalformedXML" — the exact error
	// aws-cli hit uploading a multi-GB object (issue #26). 8MiB fits 10,000 parts
	// with room to spare while staying bounded.
	const maxCompleteBodySize = 8 << 20
	var req completeRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, maxCompleteBodySize)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse request body", http.StatusBadRequest)
		return
	}

	stored, err := h.multipartStore().ListParts(uploadID)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	if rerr := checkCompleteParts(req.Parts, stored); rerr != nil {
		writeS3Error(w, rerr.code, rerr.msg, rerr.status)
		return
	}

	// Check quota against the parts actually being assembled
	sizes := make(map[int]int64, len(stored))
	for _, p := range stored {
		sizes[p.PartNumber] = p.Size
	}
	var estimatedSize int64
	for _, p := range req.Parts {
		estimatedSize += sizes[p.PartNumber]
	}
	if !h.checkQuota(w, bucket, estimatedSize) {
		return
	}

	// Assemble the parts into a temp file of this request's own, then write the
	// object through writeObject. That keeps completion atomic (the engine does
	// temp+rename), routes through the packed/compressed/encrypted wrappers and
	// the versioning and object-lock rules, and never touches a pre-existing
	// object at the target key until the new object is fully assembled.
	outFile, err := os.CreateTemp(h.multipartDir(uploadID), "assembled-*.tmp")
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	assemblePath := outFile.Name()
	defer os.Remove(assemblePath)

	// Concatenate parts and compute multipart ETag
	var totalSize int64
	combinedHash := md5.New()
	var partBoundaries []int64
	missingPart := 0

	var openErr error
	for _, part := range req.Parts {
		partPath := filepath.Join(h.multipartDir(uploadID), fmt.Sprintf("part-%05d", part.PartNumber))
		pf, err := os.Open(partPath)
		if err != nil {
			// Only a genuinely absent part is the client's problem. Anything else
			// (out of file descriptors, an I/O error, a permission problem) is the
			// server's, and reporting it as InvalidPart told the client its request
			// was malformed, so every SDK correctly refused to retry a condition
			// that a retry would have survived (issue #48).
			if !os.IsNotExist(err) {
				openErr = err
			}
			missingPart = part.PartNumber
			break
		}

		partHash := md5.New()
		written, err := io.Copy(outFile, io.TeeReader(pf, partHash))
		pf.Close()
		if err != nil {
			outFile.Close()
			slog.Error("internal error", "error", err)
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
			return
		}
		// The bytes on disk must be the part the client named. A part re-uploaded
		// while this completion was being prepared has a different digest.
		if hex.EncodeToString(partHash.Sum(nil)) != normalizeETag(part.ETag) {
			outFile.Close()
			writeS3Error(w, "InvalidPart", fmt.Sprintf("Part %d does not match its ETag", part.PartNumber), http.StatusBadRequest)
			return
		}

		totalSize += written
		partBoundaries = append(partBoundaries, totalSize)
		combinedHash.Write(partHash.Sum(nil))
	}
	if cerr := outFile.Close(); cerr != nil && missingPart == 0 {
		slog.Error("internal error", "error", cerr)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	if missingPart != 0 {
		if openErr != nil {
			slog.Error("multipart: could not read a part that is present, failing the completion",
				"bucket", bucket, "key", key, "upload", uploadID,
				"part", missingPart, "error", openErr)
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
			return
		}
		// The part's data really is gone while its metadata still lists it, so the
		// upload can never complete and the client cannot tell why. Say so in the
		// log: silence here is what left issue #48 undiagnosable from the server side.
		slog.Error("multipart: a part listed by this upload has no data on disk, so the upload cannot complete",
			"bucket", bucket, "key", key, "upload", uploadID, "part", missingPart,
			"hint", "re-upload the part, or abort the upload and start again")
		writeS3Error(w, "InvalidPart", fmt.Sprintf("Part %d not found", missingPart), http.StatusBadRequest)
		return
	}

	af, err := os.Open(assemblePath)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	defer af.Close()

	// S3 multipart ETag: md5(md5(part1) + md5(part2) + ...)-N
	etag := fmt.Sprintf("\"%s-%d\"", hex.EncodeToString(combinedHash.Sum(nil)), len(req.Parts))

	meta, err := h.writeObject(newObject{
		bucket: bucket,
		key:    key,
		body:   af,
		size:   totalSize,
		meta: metadata.ObjectMeta{
			ContentType:        upload.ContentType,
			PartsCount:         len(req.Parts),
			PartBoundaries:     partBoundaries,
			Tags:               upload.Tags,
			UserMetadata:       upload.UserMetadata,
			ContentEncoding:    upload.ContentEncoding,
			ContentDisposition: upload.ContentDisposition,
			CacheControl:       upload.CacheControl,
			ContentLanguage:    upload.ContentLanguage,
			WebsiteRedirect:    upload.WebsiteRedirect,
		},
		etag:             etag,
		bypassGovernance: h.governanceBypass(r, bucket, key),
		lockFrom:         uploadLockRequest(upload),
	})
	if err != nil {
		// The parts are assembled but the object is not recorded. Failing here
		// leaves the upload completable on retry, which is far better than
		// acknowledging an object that will never list.
		answerWriteError(w, err, bucket, key)
		return
	}

	// Clean up. The record goes FIRST: stopping between the two steps (an OOM
	// kill, an eviction) used to leave the record advertising parts whose files
	// were already gone, so every later attempt hit InvalidPart forever. This
	// order fails the other way instead, leaving part files with no record, which
	// is both harmless (they are unreachable) and reclaimable with
	// `vaults3-cli storage reclaim` (issue #47).
	h.multipartStore().DeleteMultipartUpload(uploadID)
	os.RemoveAll(h.multipartDir(uploadID))

	type completeResult struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		Xmlns    string   `xml:"xmlns,attr"`
		Location string   `xml:"Location"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		ETag     string   `xml:"ETag"`
	}

	if meta.VersionID != "" {
		w.Header().Set("X-Amz-Version-Id", meta.VersionID)
	}
	writeXML(w, http.StatusOK, completeResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Location: fmt.Sprintf("/%s/%s", bucket, key),
		Bucket:   bucket,
		Key:      key,
		ETag:     meta.ETag,
	})
	h.notifyCreated("s3:ObjectCreated:CompleteMultipartUpload", meta)
}

// AbortMultipartUpload handles DELETE /{bucket}/{key}?uploadId=X.
func (h *ObjectHandler) AbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	if _, ok := h.requireUpload(w, r, uploadID, bucket, key); !ok {
		return
	}
	unlock := lockUpload(uploadID)
	defer unlock()

	os.RemoveAll(h.multipartDir(uploadID))
	h.multipartStore().DeleteMultipartUpload(uploadID)

	w.WriteHeader(http.StatusNoContent)
}

func (h *ObjectHandler) multipartDir(uploadID string) string {
	if !validUploadID.MatchString(uploadID) {
		// Return a safe path that won't exist, callers check for errors
		return filepath.Join(h.engine.DataDir(), ".multipart", "invalid")
	}
	return filepath.Join(h.engine.DataDir(), ".multipart", uploadID)
}

// UploadPartCopy handles PUT /{bucket}/{key}?partNumber=N&uploadId=X with X-Amz-Copy-Source.
func (h *ObjectHandler) UploadPartCopy(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	if _, ok := h.requireUpload(w, r, uploadID, bucket, key); !ok {
		return
	}

	partNumStr := r.URL.Query().Get("partNumber")
	partNum, err := strconv.Atoi(partNumStr)
	if err != nil || partNum < 1 || partNum > 10000 {
		writeS3Error(w, "InvalidArgument", "Invalid part number", http.StatusBadRequest)
		return
	}

	// Parsed and opened exactly as CopyObject does. This used to take the header
	// without unescaping it, so an encoded key named a different object from the
	// one the router authorized, and it read the plain path, which misses every
	// object in a versioned bucket.
	src, perr := parseCopySourceHeader(r.Header.Get("X-Amz-Copy-Source"))
	if perr != nil {
		writeS3Error(w, "InvalidArgument", "Invalid x-amz-copy-source", http.StatusBadRequest)
		return
	}
	srcMeta, reader, srcSize, ok := h.openCopySource(w, r, src)
	if !ok {
		return
	}
	defer reader.Close()
	if checkCopyPreconditions(w, r, srcMeta) {
		return
	}

	var dataReader io.Reader = reader

	// Parse optional range header
	if rangeHeader := r.Header.Get("X-Amz-Copy-Source-Range"); rangeHeader != "" {
		// Format: bytes=START-END
		rangeHeader = strings.TrimPrefix(rangeHeader, "bytes=")
		parts := strings.SplitN(rangeHeader, "-", 2)
		if len(parts) != 2 {
			writeS3Error(w, "InvalidArgument", "Invalid copy source range", http.StatusBadRequest)
			return
		}
		start, err1 := strconv.ParseInt(parts[0], 10, 64)
		end, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil || start < 0 || end < start || start >= srcSize {
			writeS3Error(w, "InvalidArgument", "Invalid copy source range", http.StatusBadRequest)
			return
		}
		if end >= srcSize {
			end = srcSize - 1
		}
		if _, err := reader.Seek(start, io.SeekStart); err != nil {
			writeS3Error(w, "InternalError", "Failed to seek source", http.StatusInternalServerError)
			return
		}
		dataReader = io.LimitReader(reader, end-start+1)
	}

	// Write to the part file through the same temp-then-rename path as a normal
	// part upload, so a failed copy cannot destroy a part that already succeeded
	// (issue #48).
	written, etag, ok := h.writePart(w, dataReader, uploadID, partNum)
	if !ok {
		return
	}

	if err := h.multipartStore().PutPart(uploadID, metadata.PartInfo{
		PartNumber: partNum,
		ETag:       etag,
		Size:       written,
	}); err != nil {
		metaWriteFailed(w, err, "PutPart", bucket, key)
		return
	}

	now := time.Now().UTC()

	type copyPartResult struct {
		XMLName      xml.Name `xml:"CopyPartResult"`
		ETag         string   `xml:"ETag"`
		LastModified string   `xml:"LastModified"`
	}

	if srcMeta.VersionID != "" {
		w.Header().Set("X-Amz-Copy-Source-Version-Id", srcMeta.VersionID)
	}
	writeXML(w, http.StatusOK, copyPartResult{
		ETag:         etag,
		LastModified: now.Format(time.RFC3339),
	})
}

func generateUploadID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ListMultipartUploads handles GET /{bucket}?uploads.
func (h *ObjectHandler) ListMultipartUploads(w http.ResponseWriter, r *http.Request, bucket string) {
	uploads, err := h.multipartStore().ListMultipartUploads(bucket)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	// This request is bucket-level, so a cluster routes it to one node by
	// hash(bucket, ""), but uploads are stored on the node that owns each object
	// KEY. Listing only what is local therefore showed roughly 1/N of the uploads
	// and hid the rest completely: no ListParts, no abort, and no lifecycle rule
	// could reach them, so their parts sat on disk forever (issue #47 bug B).
	if h.multipartPeers != nil {
		seen := make(map[string]bool, len(uploads))
		for _, u := range uploads {
			seen[u.UploadID] = true
		}
		for _, u := range h.multipartPeers(bucket) {
			if !seen[u.UploadID] {
				seen[u.UploadID] = true
				uploads = append(uploads, u)
			}
		}
	}
	// A stable order across nodes: the merge order depends on map iteration and
	// peer response timing, which would otherwise reshuffle the listing every call.
	sort.Slice(uploads, func(i, j int) bool {
		if uploads[i].Key != uploads[j].Key {
			return uploads[i].Key < uploads[j].Key
		}
		return uploads[i].UploadID < uploads[j].UploadID
	})

	type xmlUpload struct {
		Key       string `xml:"Key"`
		UploadID  string `xml:"UploadId"`
		Initiated string `xml:"Initiated"`
	}
	type xmlResult struct {
		XMLName xml.Name    `xml:"ListMultipartUploadsResult"`
		Xmlns   string      `xml:"xmlns,attr"`
		Bucket  string      `xml:"Bucket"`
		Uploads []xmlUpload `xml:"Upload"`
	}
	resp := xmlResult{
		Xmlns:  "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket: bucket,
	}
	for _, u := range uploads {
		resp.Uploads = append(resp.Uploads, xmlUpload{
			Key:       u.Key,
			UploadID:  u.UploadID,
			Initiated: time.Unix(u.CreatedAt, 0).UTC().Format(time.RFC3339),
		})
	}
	writeXML(w, http.StatusOK, resp)
}

// ListParts handles GET /{bucket}/{key}?uploadId=X.
func (h *ObjectHandler) ListParts(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	if _, ok := h.requireUpload(w, r, uploadID, bucket, key); !ok {
		return
	}

	parts, err := h.multipartStore().ListParts(uploadID)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	type xmlPart struct {
		PartNumber   int    `xml:"PartNumber"`
		Size         int64  `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	}
	type xmlResult struct {
		XMLName  xml.Name  `xml:"ListPartsResult"`
		Xmlns    string    `xml:"xmlns,attr"`
		Bucket   string    `xml:"Bucket"`
		Key      string    `xml:"Key"`
		UploadID string    `xml:"UploadId"`
		Parts    []xmlPart `xml:"Part"`
	}
	resp := xmlResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:   bucket,
		Key:      key,
		UploadID: uploadID,
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, p := range parts {
		resp.Parts = append(resp.Parts, xmlPart{
			PartNumber:   p.PartNumber,
			Size:         p.Size,
			ETag:         p.ETag,
			LastModified: now,
		})
	}
	writeXML(w, http.StatusOK, resp)
}

// uploadLockRequest carries an upload's object-lock headers to completion. The
// lock is asked for when the upload starts, not when it completes, so applying
// it from the completing request lost it.
func uploadLockRequest(u *metadata.MultipartUpload) *http.Request {
	h := http.Header{}
	for name, v := range map[string]string{
		"X-Amz-Object-Lock-Mode":              u.LockMode,
		"X-Amz-Object-Lock-Retain-Until-Date": u.LockRetainUntil,
		"X-Amz-Object-Lock-Legal-Hold":        u.LockLegalHold,
	} {
		if v != "" {
			h.Set(name, v)
		}
	}
	return &http.Request{Header: h}
}
