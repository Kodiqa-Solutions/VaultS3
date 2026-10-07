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
	h.catchUp()
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

	var werr error
	if meta.VersionID != "" {
		werr = h.store.UpdateObjectVersionMeta(*meta)
	} else {
		werr = h.store.PutObjectMeta(*meta)
	}
	// A legal hold that reports success without being recorded is a compliance
	// failure, not a performance detail.
	if metaWriteFailed(w, werr, "PutObjectLegalHold", bucket, key) {
		return
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
	h.catchUp()
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

	// An empty Retention removes it, which only a GOVERNANCE lock with an
	// authorized bypass, or an expired lock, allows.
	clearing := req.Mode == "" && req.RetainUntilDate == ""
	var retainUntil time.Time
	if !clearing {
		if req.Mode != "GOVERNANCE" && req.Mode != "COMPLIANCE" {
			writeS3Error(w, "InvalidArgument", "Retention mode must be GOVERNANCE or COMPLIANCE", http.StatusBadRequest)
			return
		}
		var err error
		retainUntil, err = time.Parse(time.RFC3339, req.RetainUntilDate)
		if err != nil {
			writeS3Error(w, "InvalidArgument", "RetainUntilDate must be RFC3339 format", http.StatusBadRequest)
			return
		}
	}

	meta, err := h.getVersionMeta(bucket, key, versionID)
	if err != nil {
		if metadataUnavailable(w, err) {
			return
		}
		writeS3Error(w, "NoSuchKey", "Object not found", http.StatusNotFound)
		return
	}

	if msg := retentionChangeRefused(meta, req.Mode, retainUntil, clearing, h.governanceBypass(r, bucket, key)); msg != "" {
		writeS3Error(w, "AccessDenied", msg, http.StatusForbidden)
		return
	}

	if clearing {
		meta.RetentionMode = ""
		meta.RetentionUntil = 0
	} else {
		meta.RetentionMode = req.Mode
		meta.RetentionUntil = retainUntil.Unix()
	}

	var werr error
	if meta.VersionID != "" {
		werr = h.store.UpdateObjectVersionMeta(*meta)
	} else {
		werr = h.store.PutObjectMeta(*meta)
	}
	if metaWriteFailed(w, werr, "PutObjectRetention", bucket, key) {
		return
	}

	w.WriteHeader(http.StatusOK)
}

// retentionChangeRefused says why a retention change is not allowed, or "" when
// it is. These are the S3 rules:
//
// An active COMPLIANCE lock can only be extended. Nobody can shorten it, remove
// it or turn it into GOVERNANCE, bypass or not.
//
// An active GOVERNANCE lock can be extended, or raised to COMPLIANCE, freely.
// Shortening or removing it needs x-amz-bypass-governance-retention: true and
// the s3:BypassGovernanceRetention permission.
//
// Only shortening COMPLIANCE used to be refused. A COMPLIANCE lock could be
// downgraded to GOVERNANCE with the same date and then removed, and a GOVERNANCE
// lock could be shortened or removed by anyone allowed PutObjectRetention.
func retentionChangeRefused(meta *metadata.ObjectMeta, mode string, until time.Time, clearing, bypass bool) string {
	if meta.RetentionMode == "" || meta.RetentionUntil == 0 || time.Now().UTC().Unix() >= meta.RetentionUntil {
		return "" // nothing active to protect
	}
	extends := !clearing && until.Unix() >= meta.RetentionUntil
	switch meta.RetentionMode {
	case "COMPLIANCE":
		if clearing || mode != "COMPLIANCE" {
			return "An object under COMPLIANCE retention cannot have its retention mode changed or removed"
		}
		if !extends {
			return "Cannot shorten COMPLIANCE retention period"
		}
	case "GOVERNANCE":
		if extends {
			return ""
		}
		if !bypass {
			return "Shortening or removing GOVERNANCE retention requires x-amz-bypass-governance-retention and the s3:BypassGovernanceRetention permission"
		}
	}
	return ""
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
