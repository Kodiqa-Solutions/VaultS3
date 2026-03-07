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

	if err := h.store.CreateMultipartUpload(upload); err != nil {
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

// UploadPart handles PUT /{bucket}/{key}?partNumber=N&uploadId=X.
func (h *ObjectHandler) UploadPart(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	_, err := h.store.GetMultipartUpload(uploadID)
	if err != nil {
		writeS3Error(w, "NoSuchUpload", "Upload not found", http.StatusNotFound)
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

	partPath := filepath.Join(h.multipartDir(uploadID), fmt.Sprintf("part-%05d", partNum))
	f, err := os.Create(partPath)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	hash := md5.New()
	written, err := io.Copy(f, io.TeeReader(r.Body, hash))
	if err != nil {
		os.Remove(partPath)
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	etag := fmt.Sprintf("\"%s\"", hex.EncodeToString(hash.Sum(nil)))

	h.store.PutPart(uploadID, metadata.PartInfo{
		PartNumber: partNum,
		ETag:       etag,
		Size:       written,
	})

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// CompleteMultipartUpload handles POST /{bucket}/{key}?uploadId=X.
func (h *ObjectHandler) CompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	upload, err := h.store.GetMultipartUpload(uploadID)
	if err != nil {
		writeS3Error(w, "NoSuchUpload", "Upload not found", http.StatusNotFound)
		return
	}

	// Check quota (estimate size from parts)
	parts, _ := h.store.ListParts(uploadID)
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

	var req completeRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse request body", http.StatusBadRequest)
		return
	}

	sort.Slice(req.Parts, func(i, j int) bool {
		return req.Parts[i].PartNumber < req.Parts[j].PartNumber
	})

	// Assemble the final object
	objPath := h.engine.ObjectPath(bucket, key)
	if err := os.MkdirAll(filepath.Dir(objPath), 0755); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	outFile, err := os.Create(objPath)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	defer outFile.Close()

	// Concatenate parts and compute multipart ETag
	var totalSize int64
	combinedHash := md5.New()
	var partBoundaries []int64

	for _, part := range req.Parts {
		partPath := filepath.Join(h.multipartDir(uploadID), fmt.Sprintf("part-%05d", part.PartNumber))
		pf, err := os.Open(partPath)
		if err != nil {
			os.Remove(objPath)
			writeS3Error(w, "InvalidPart", fmt.Sprintf("Part %d not found", part.PartNumber), http.StatusBadRequest)
			return
		}

		partHash := md5.New()
		written, err := io.Copy(outFile, io.TeeReader(pf, partHash))
		pf.Close()
		if err != nil {
			os.Remove(objPath)
			slog.Error("internal error", "error", err)
			writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
			return
		}

		totalSize += written
		partBoundaries = append(partBoundaries, totalSize)
		combinedHash.Write(partHash.Sum(nil))
	}

	// S3 multipart ETag: md5(md5(part1) + md5(part2) + ...)-N
	etag := fmt.Sprintf("\"%s-%d\"", hex.EncodeToString(combinedHash.Sum(nil)), len(req.Parts))

	now := time.Now().UTC()

	h.store.PutObjectMeta(metadata.ObjectMeta{
		Bucket:         bucket,
		Key:            key,
		ContentType:    upload.ContentType,
		ETag:           etag,
		Size:           totalSize,
		LastModified:   now.Unix(),
		PartsCount:     len(req.Parts),
		PartBoundaries: partBoundaries,
	})

	// Clean up
	os.RemoveAll(h.multipartDir(uploadID))
	h.store.DeleteMultipartUpload(uploadID)

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
	_, err := h.store.GetMultipartUpload(uploadID)
	if err != nil {
		writeS3Error(w, "NoSuchUpload", "Upload not found", http.StatusNotFound)
		return
	}

	os.RemoveAll(h.multipartDir(uploadID))
	h.store.DeleteMultipartUpload(uploadID)

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
	_, err := h.store.GetMultipartUpload(uploadID)
	if err != nil {
		writeS3Error(w, "NoSuchUpload", "Upload not found", http.StatusNotFound)
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

	// Write to part file
	partPath := filepath.Join(h.multipartDir(uploadID), fmt.Sprintf("part-%05d", partNum))
	f, err := os.Create(partPath)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	hash := md5.New()
	written, err := io.Copy(f, io.TeeReader(dataReader, hash))
	if err != nil {
		os.Remove(partPath)
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	etag := fmt.Sprintf("\"%s\"", hex.EncodeToString(hash.Sum(nil)))

	h.store.PutPart(uploadID, metadata.PartInfo{
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
	uploads, err := h.store.ListMultipartUploads(bucket)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

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
	_, err := h.store.GetMultipartUpload(uploadID)
	if err != nil {
		writeS3Error(w, "NoSuchUpload", "Upload not found", http.StatusNotFound)
		return
	}

	parts, err := h.store.ListParts(uploadID)
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
