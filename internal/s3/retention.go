package s3

import (
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
)

// PutBucketObjectLockConfig handles PUT /{bucket}?object-lock.
func (h *ObjectHandler) PutBucketObjectLockConfig(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	// Require versioning to be enabled
	versioning, _ := h.store.GetBucketVersioning(bucket)
	if versioning != "Enabled" {
		writeS3Error(w, "InvalidBucketState", "Object Lock requires versioning to be enabled", http.StatusConflict)
		return
	}

	var req struct {
		XMLName xml.Name `xml:"ObjectLockConfiguration"`
		Rule    struct {
			DefaultRetention struct {
				Mode string `xml:"Mode"`
				Days int    `xml:"Days"`
			} `xml:"DefaultRetention"`
		} `xml:"Rule"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse object lock XML", http.StatusBadRequest)
		return
	}

	mode := req.Rule.DefaultRetention.Mode
	days := req.Rule.DefaultRetention.Days

	if mode != "" && mode != "GOVERNANCE" && mode != "COMPLIANCE" {
		writeS3Error(w, "InvalidArgument", "Mode must be GOVERNANCE or COMPLIANCE", http.StatusBadRequest)
		return
	}

	if mode != "" && days <= 0 {
		writeS3Error(w, "InvalidArgument", "Days must be positive when Mode is set", http.StatusBadRequest)
		return
	}

	if err := h.store.SetBucketDefaultRetention(bucket, mode, days); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	// Configuring object lock marks the bucket object-lock enabled.
	if err := h.store.SetBucketObjectLockEnabled(bucket, true); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// GetBucketObjectLockConfig handles GET /{bucket}?object-lock.
func (h *ObjectHandler) GetBucketObjectLockConfig(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	info, err := h.store.GetBucket(bucket)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	// Report the true state instead of always claiming "Enabled".
	if !info.ObjectLockEnabled {
		writeS3Error(w, "ObjectLockConfigurationNotFoundError",
			"Object Lock configuration does not exist for this bucket", http.StatusNotFound)
		return
	}

	type defaultRetention struct {
		Mode string `xml:"Mode,omitempty"`
		Days int    `xml:"Days,omitempty"`
	}
	type rule struct {
		DefaultRetention defaultRetention `xml:"DefaultRetention"`
	}
	type objectLockConfig struct {
		XMLName           xml.Name `xml:"ObjectLockConfiguration"`
		Xmlns             string   `xml:"xmlns,attr"`
		ObjectLockEnabled string   `xml:"ObjectLockEnabled"`
		Rule              *rule    `xml:"Rule,omitempty"`
	}

	resp := objectLockConfig{
		Xmlns:             "http://s3.amazonaws.com/doc/2006-03-01/",
		ObjectLockEnabled: "Enabled",
	}

	if info.DefaultRetentionMode != "" && info.DefaultRetentionDays > 0 {
		resp.Rule = &rule{
			DefaultRetention: defaultRetention{
				Mode: info.DefaultRetentionMode,
				Days: info.DefaultRetentionDays,
			},
		}
	}

	writeXML(w, http.StatusOK, resp)
}
