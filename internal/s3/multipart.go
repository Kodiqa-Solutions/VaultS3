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

	uploadID := generateUploadID()

	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}

	upload := metadata.MultipartUpload{
		UploadID:    uploadID,
		Bucket:      bucket,
		Key:         key,
		ContentType: ct,
		CreatedAt:   time.Now().UTC().Unix(),
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
func (h *ObjectHandler) requireUpload(w http.ResponseWriter, r *http.Request, uploadID string) (*metadata.MultipartUpload, bool) {
	upload, err := h.multipartStore().GetMultipartUpload(uploadID)
	if err == nil {
		return upload, true
	}
	if h.multipartHolder != nil && h.multipartHolder(w, r, uploadID) {
		return nil, false
	}
	writeS3Error(w, "NoSuchUpload", "Upload not found", http.StatusNotFound)
	return nil, false
}

// UploadPart handles PUT /{bucket}/{key}?partNumber=N&uploadId=X.
func (h *ObjectHandler) UploadPart(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	if _, ok := h.requireUpload(w, r, uploadID); !ok {
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

	written, etag, ok := h.writePart(w, r.Body, uploadID, partNum)
	if !ok {
		return
	}

	h.multipartStore().PutPart(uploadID, metadata.PartInfo{
		PartNumber: partNum,
		ETag:       etag,
		Size:       written,
	})

	w.Header().Set("ETag", etag)
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
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
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

// CompleteMultipartUpload handles POST /{bucket}/{key}?uploadId=X.
func (h *ObjectHandler) CompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	upload, ok := h.requireUpload(w, r, uploadID)
	if !ok {
		return
	}

	// Check quota (estimate size from parts)
	parts, _ := h.multipartStore().ListParts(uploadID)
	var estimatedSize int64
	for _, p := range parts {
		estimatedSize += p.Size
	}
	if !h.checkQuota(w, bucket, estimatedSize) {
		return
	}

	type completePart struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
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

	sort.Slice(req.Parts, func(i, j int) bool {
		return req.Parts[i].PartNumber < req.Parts[j].PartNumber
	})

	// Assemble the parts. When encryption is enabled we assemble into a temp file
	// and write the object through engine.PutObject so it is encrypted at rest
	// (per-bucket or SSE); otherwise we assemble straight to the final path.
	// Assemble the parts into a temp file, then write the object through
	// engine.PutObject. This keeps completion atomic (the engine does temp+rename),
	// routes through the packed/compressed/encrypted wrappers, and never touches a
	// pre-existing object at the target key until the new object is fully assembled,
	// so a failed complete (e.g. a missing part) cannot truncate or delete it.
	assemblePath := filepath.Join(h.multipartDir(uploadID), "assembled.tmp")
	outFile, err := os.Create(assemblePath)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

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
			os.Remove(assemblePath)
			slog.Error("internal error", "error", err)
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
			return
		}

		totalSize += written
		partBoundaries = append(partBoundaries, totalSize)
		combinedHash.Write(partHash.Sum(nil))
	}
	outFile.Close()
	if missingPart != 0 {
		os.Remove(assemblePath)
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

	// Write the assembled object through the engine (atomic temp+rename; applies
	// compression / per-bucket or SSE encryption / packing as configured), then
	// drop the temp file.
	af, err := os.Open(assemblePath)
	if err != nil {
		os.Remove(assemblePath)
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	if _, _, perr := h.engine.PutObject(bucket, key, af, totalSize); perr != nil {
		af.Close()
		os.Remove(assemblePath)
		slog.Error("internal error", "error", perr)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	af.Close()
	os.Remove(assemblePath)

	// S3 multipart ETag: md5(md5(part1) + md5(part2) + ...)-N
	etag := fmt.Sprintf("\"%s-%d\"", hex.EncodeToString(combinedHash.Sum(nil)), len(req.Parts))

	now := time.Now().UTC()

	if err := h.store.PutObjectMeta(metadata.ObjectMeta{
		Bucket:         bucket,
		Key:            key,
		ContentType:    upload.ContentType,
		ETag:           etag,
		Size:           totalSize,
		LastModified:   now.Unix(),
		PartsCount:     len(req.Parts),
		PartBoundaries: partBoundaries,
	}); err != nil {
		// The parts are assembled but the object is not recorded. Failing here
		// leaves the upload completable on retry, which is far better than
		// acknowledging an object that will never list.
		metaWriteFailed(w, err, "CompleteMultipartUpload", bucket, key)
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

	writeXML(w, http.StatusOK, completeResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Location: fmt.Sprintf("/%s/%s", bucket, key),
		Bucket:   bucket,
		Key:      key,
		ETag:     etag,
	})
	if h.onNotification != nil {
		h.onNotification("s3:ObjectCreated:CompleteMultipartUpload", bucket, key, totalSize, etag, "")
	}
	if h.onReplication != nil {
		h.onReplication("s3:ObjectCreated:CompleteMultipartUpload", bucket, key, totalSize, etag, "")
	}
	if h.onLambda != nil {
		h.onLambda("s3:ObjectCreated:CompleteMultipartUpload", bucket, key, totalSize, etag, "")
	}
	if h.onScan != nil {
		h.onScan(bucket, key, totalSize)
	}
	if h.onSearchUpdate != nil {
		h.onSearchUpdate("put", bucket, key)
	}
}

// AbortMultipartUpload handles DELETE /{bucket}/{key}?uploadId=X.
func (h *ObjectHandler) AbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	if _, ok := h.requireUpload(w, r, uploadID); !ok {
		return
	}

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
	if _, ok := h.requireUpload(w, r, uploadID); !ok {
		return
	}

	partNumStr := r.URL.Query().Get("partNumber")
	partNum, err := strconv.Atoi(partNumStr)
	if err != nil || partNum < 1 || partNum > 10000 {
		writeS3Error(w, "InvalidArgument", "Invalid part number", http.StatusBadRequest)
		return
	}

	// Parse copy source
	copySource := r.Header.Get("X-Amz-Copy-Source")
	copySource = strings.TrimPrefix(copySource, "/")
	srcBucket, srcKey := parseCopySource(copySource)
	if srcBucket == "" || srcKey == "" {
		writeS3Error(w, "InvalidArgument", "Invalid x-amz-copy-source", http.StatusBadRequest)
		return
	}
	// Validate source key against path traversal
	for _, segment := range strings.Split(srcKey, "/") {
		if segment == ".." {
			writeS3Error(w, "InvalidArgument", "Invalid x-amz-copy-source key", http.StatusBadRequest)
			return
		}
	}

	if !h.store.BucketExists(srcBucket) {
		writeS3Error(w, "NoSuchBucket", "Source bucket does not exist", http.StatusNotFound)
		return
	}

	// Read source object
	reader, srcSize, err := h.engine.GetObject(srcBucket, srcKey)
	if err != nil {
		writeS3Error(w, "NoSuchKey", "Source object not found", http.StatusNotFound)
		return
	}
	defer reader.Close()

	var dataReader io.Reader = reader
	var copySize int64 = srcSize

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
		// Skip to start
		if start > 0 {
			if _, err := io.CopyN(io.Discard, reader, start); err != nil {
				writeS3Error(w, "InternalError", "Failed to seek source", http.StatusInternalServerError)
				return
			}
		}
		copySize = end - start + 1
		dataReader = io.LimitReader(reader, copySize)
	}

	// Write to the part file through the same temp-then-rename path as a normal
	// part upload, so a failed copy cannot destroy a part that already succeeded
	// (issue #48).
	written, etag, ok := h.writePart(w, dataReader, uploadID, partNum)
	if !ok {
		return
	}

	h.multipartStore().PutPart(uploadID, metadata.PartInfo{
		PartNumber: partNum,
		ETag:       etag,
		Size:       written,
	})

	now := time.Now().UTC()

	type copyPartResult struct {
		XMLName      xml.Name `xml:"CopyPartResult"`
		ETag         string   `xml:"ETag"`
		LastModified string   `xml:"LastModified"`
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
	if _, ok := h.requireUpload(w, r, uploadID); !ok {
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
