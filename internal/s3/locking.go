package s3

import (
	"encoding/xml"
	"io"
	"net/http"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// PutObjectLegalHold handles PUT /{bucket}/{key}?legal-hold.
func (h *ObjectHandler) PutObjectLegalHold(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	versionID := r.URL.Query().Get("versionId")

	var req struct {
		XMLName xml.Name `xml:"LegalHold"`
		Status  string   `xml:"Status"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse legal hold XML", http.StatusBadRequest)
		return
	}

	if req.Status != "ON" && req.Status != "OFF" {
		writeS3Error(w, "InvalidArgument", "Legal hold status must be ON or OFF", http.StatusBadRequest)
		return
	}

	// Get the version metadata
	meta, err := h.getVersionMeta(bucket, key, versionID)
	if err != nil {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}

	meta.LegalHold = req.Status == "ON"

	if meta.VersionID != "" {
		h.store.UpdateObjectVersionMeta(*meta)
	} else {
		h.store.PutObjectMeta(*meta)
	}

	w.WriteHeader(http.StatusOK)
}

// GetObjectLegalHold handles GET /{bucket}/{key}?legal-hold.
func (h *ObjectHandler) GetObjectLegalHold(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	versionID := r.URL.Query().Get("versionId")

	meta, err := h.getVersionMeta(bucket, key, versionID)
	if err != nil {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}

	status := "OFF"
	if meta.LegalHold {
		status = "ON"
	}

	type legalHoldResp struct {
		XMLName xml.Name `xml:"LegalHold"`
		Xmlns   string   `xml:"xmlns,attr"`
		Status  string   `xml:"Status"`
	}

	writeXML(w, http.StatusOK, legalHoldResp{
		Xmlns:  "http://s3.amazonaws.com/doc/2006-03-01/",
		Status: status,
	})
}

// PutObjectRetention handles PUT /{bucket}/{key}?retention.
func (h *ObjectHandler) PutObjectRetention(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	versionID := r.URL.Query().Get("versionId")

	var req struct {
		XMLName         xml.Name `xml:"Retention"`
		Mode            string   `xml:"Mode"`
		RetainUntilDate string   `xml:"RetainUntilDate"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse retention XML", http.StatusBadRequest)
		return
	}

	if req.Mode != "GOVERNANCE" && req.Mode != "COMPLIANCE" {
		writeS3Error(w, "InvalidArgument", "Retention mode must be GOVERNANCE or COMPLIANCE", http.StatusBadRequest)
		return
	}

	retainUntil, err := time.Parse(time.RFC3339, req.RetainUntilDate)
	if err != nil {
		writeS3Error(w, "InvalidArgument", "RetainUntilDate must be RFC3339 format", http.StatusBadRequest)
		return
	}

	meta, err := h.getVersionMeta(bucket, key, versionID)
	if err != nil {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}

	// Check if existing COMPLIANCE retention is still active (cannot be shortened)
	if meta.RetentionMode == "COMPLIANCE" && meta.RetentionUntil > 0 {
		if time.Now().UTC().Unix() < meta.RetentionUntil && retainUntil.Unix() < meta.RetentionUntil {
			writeS3Error(w, "AccessDenied", "Cannot shorten COMPLIANCE retention period", http.StatusForbidden)
			return
		}
	}

	meta.RetentionMode = req.Mode
	meta.RetentionUntil = retainUntil.Unix()

	if meta.VersionID != "" {
		h.store.UpdateObjectVersionMeta(*meta)
	} else {
		h.store.PutObjectMeta(*meta)
	}

	w.WriteHeader(http.StatusOK)
}

// GetObjectRetention handles GET /{bucket}/{key}?retention.
func (h *ObjectHandler) GetObjectRetention(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	versionID := r.URL.Query().Get("versionId")

	meta, err := h.getVersionMeta(bucket, key, versionID)
	if err != nil {
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}

	if meta.RetentionMode == "" {
		writeS3Error(w, "NoSuchObjectLockConfiguration", "No retention configured", http.StatusNotFound)
		return
	}

	type retentionResp struct {
		XMLName         xml.Name `xml:"Retention"`
		Xmlns           string   `xml:"xmlns,attr"`
		Mode            string   `xml:"Mode"`
		RetainUntilDate string   `xml:"RetainUntilDate"`
	}

	writeXML(w, http.StatusOK, retentionResp{
		Xmlns:           "http://s3.amazonaws.com/doc/2006-03-01/",
		Mode:            meta.RetentionMode,
		RetainUntilDate: time.Unix(meta.RetentionUntil, 0).UTC().Format(time.RFC3339),
	})
}

// getVersionMeta retrieves metadata for a specific version or the latest.
func (h *ObjectHandler) getVersionMeta(bucket, key, versionID string) (*metadata.ObjectMeta, error) {
	if versionID != "" {
		return h.store.GetObjectVersion(bucket, key, versionID)
	}
	return h.store.GetObjectMeta(bucket, key)
}
