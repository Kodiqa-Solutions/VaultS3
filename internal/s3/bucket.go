package s3

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// validateEndpointURL prevents SSRF by blocking private/internal URLs.
func validateEndpointURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" {
		return fmt.Errorf("URL must not point to localhost")
	}
	if strings.HasPrefix(host, "169.254.") || host == "metadata.google.internal" {
		return fmt.Errorf("URL must not point to metadata service")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("URL must not point to loopback or link-local address")
		}
		privateRanges := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}
		for _, cidr := range privateRanges {
			_, network, _ := net.ParseCIDR(cidr)
			if network.Contains(ip) {
				return fmt.Errorf("URL must not point to private network")
			}
		}
	}
	return nil
}

type BucketHandler struct {
	store  *metadata.Store
	engine storage.Engine
}

// ListBuckets responds to GET / with a list of all buckets.
func (h *BucketHandler) ListBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := h.store.ListBuckets()
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	type xmlBucket struct {
		Name         string `xml:"Name"`
		CreationDate string `xml:"CreationDate"`
	}
	type xmlResponse struct {
		XMLName xml.Name    `xml:"ListAllMyBucketsResult"`
		Xmlns   string      `xml:"xmlns,attr"`
		Owner   xmlOwner    `xml:"Owner"`
		Buckets []xmlBucket `xml:"Buckets>Bucket"`
	}

	// Filter by prefix if specified
	prefix := r.URL.Query().Get("prefix")

	resp := xmlResponse{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner: xmlOwner{ID: "vaults3", DisplayName: "VaultS3"},
	}
	for _, b := range buckets {
		if prefix != "" && !strings.HasPrefix(b.Name, prefix) {
			continue
		}
		resp.Buckets = append(resp.Buckets, xmlBucket{
			Name:         b.Name,
			CreationDate: b.CreatedAt.Format(time.RFC3339),
		})
	}

	writeXML(w, http.StatusOK, resp)
}

// CreateBucket handles PUT /{bucket}.
func (h *BucketHandler) CreateBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if !isValidBucketName(bucket) {
		writeS3Error(w, "InvalidBucketName", "Invalid bucket name", http.StatusBadRequest)
		return
	}

	if err := h.store.CreateBucket(bucket); err != nil {
		writeS3Error(w, "BucketAlreadyExists", err.Error(), http.StatusConflict)
		return
	}

	if err := h.engine.CreateBucketDir(bucket); err != nil {
		h.store.DeleteBucket(bucket) // rollback
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Location", "/"+bucket)
	w.WriteHeader(http.StatusOK)
}

// DeleteBucket handles DELETE /{bucket}.
func (h *BucketHandler) DeleteBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	// Check if bucket is empty
	objects, _, err := h.engine.ListObjects(bucket, "", "", 1)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	if len(objects) > 0 {
		writeS3Error(w, "BucketNotEmpty", "Bucket is not empty", http.StatusConflict)
		return
	}

	if err := h.engine.DeleteBucketDir(bucket); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	if err := h.store.DeleteBucket(bucket); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HeadBucket handles HEAD /{bucket}.
func (h *BucketHandler) HeadBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// PutBucketPolicy handles PUT /{bucket}?policy.
func (h *BucketHandler) PutBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 20*1024)) // 20KB limit
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	// Validate JSON
	var js json.RawMessage
	if err := json.Unmarshal(body, &js); err != nil {
		writeS3Error(w, "MalformedPolicy", "Policy is not valid JSON", http.StatusBadRequest)
		return
	}

	if err := h.store.PutBucketPolicy(bucket, body); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// GetBucketPolicy handles GET /{bucket}?policy.
func (h *BucketHandler) GetBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	policy, err := h.store.GetBucketPolicy(bucket)
	if err != nil {
		writeS3Error(w, "NoSuchBucketPolicy", "Bucket policy does not exist", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(policy)
}

// DeleteBucketPolicy handles DELETE /{bucket}?policy.
func (h *BucketHandler) DeleteBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	h.store.DeleteBucketPolicy(bucket)
	w.WriteHeader(http.StatusNoContent)
}

// PutBucketQuota handles PUT /{bucket}?quota.
func (h *BucketHandler) PutBucketQuota(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	var req struct {
		MaxSizeBytes int64 `json:"max_size_bytes"`
		MaxObjects   int64 `json:"max_objects"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedJSON", "Could not parse request body", http.StatusBadRequest)
		return
	}

	if err := h.store.UpdateBucketQuota(bucket, req.MaxSizeBytes, req.MaxObjects); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// GetBucketQuota handles GET /{bucket}?quota.
func (h *BucketHandler) GetBucketQuota(w http.ResponseWriter, r *http.Request, bucket string) {
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

	currentSize, currentCount, _ := h.engine.BucketSize(bucket)

	resp := struct {
		MaxSizeBytes int64 `json:"max_size_bytes"`
		MaxObjects   int64 `json:"max_objects"`
		CurrentSize  int64 `json:"current_size_bytes"`
		CurrentCount int64 `json:"current_object_count"`
	}{
		MaxSizeBytes: info.MaxSizeBytes,
		MaxObjects:   info.MaxObjects,
		CurrentSize:  currentSize,
		CurrentCount: currentCount,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

// PutBucketLifecycle handles PUT /{bucket}?lifecycle.
func (h *BucketHandler) PutBucketLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	var req struct {
		XMLName xml.Name `xml:"LifecycleConfiguration"`
		Rules   []struct {
			Expiration struct {
				Days int `xml:"Days"`
			} `xml:"Expiration"`
			Filter struct {
				Prefix string `xml:"Prefix"`
			} `xml:"Filter"`
			Status string `xml:"Status"`
		} `xml:"Rule"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse lifecycle XML", http.StatusBadRequest)
		return
	}

	if len(req.Rules) == 0 {
		writeS3Error(w, "InvalidArgument", "At least one rule is required", http.StatusBadRequest)
		return
	}

	// Store the first rule (simplified — one rule per bucket)
	rule := req.Rules[0]
	if rule.Expiration.Days <= 0 {
		writeS3Error(w, "InvalidArgument", "Expiration days must be positive", http.StatusBadRequest)
		return
	}

	if err := h.store.PutLifecycleRule(bucket, metadata.LifecycleRule{
		ExpirationDays: rule.Expiration.Days,
		Prefix:         rule.Filter.Prefix,
		Status:         rule.Status,
	}); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// GetBucketLifecycle handles GET /{bucket}?lifecycle.
func (h *BucketHandler) GetBucketLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	rule, err := h.store.GetLifecycleRule(bucket)
	if err != nil {
		writeS3Error(w, "NoSuchLifecycleConfiguration", "No lifecycle configuration", http.StatusNotFound)
		return
	}

	type xmlExpiration struct {
		Days int `xml:"Days"`
	}
	type xmlFilter struct {
		Prefix string `xml:"Prefix,omitempty"`
	}
	type xmlRule struct {
		Expiration xmlExpiration `xml:"Expiration"`
		Filter     xmlFilter     `xml:"Filter"`
		Status     string        `xml:"Status"`
	}
	type xmlLifecycleConfig struct {
		XMLName xml.Name  `xml:"LifecycleConfiguration"`
		Xmlns   string    `xml:"xmlns,attr"`
		Rules   []xmlRule `xml:"Rule"`
	}

	writeXML(w, http.StatusOK, xmlLifecycleConfig{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Rules: []xmlRule{{
			Expiration: xmlExpiration{Days: rule.ExpirationDays},
			Filter:     xmlFilter{Prefix: rule.Prefix},
			Status:     rule.Status,
		}},
	})
}

// DeleteBucketLifecycle handles DELETE /{bucket}?lifecycle.
func (h *BucketHandler) DeleteBucketLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	h.store.DeleteLifecycleRule(bucket)
	w.WriteHeader(http.StatusNoContent)
}

// PutBucketVersioning handles PUT /{bucket}?versioning.
func (h *BucketHandler) PutBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	var req struct {
		XMLName xml.Name `xml:"VersioningConfiguration"`
		Status  string   `xml:"Status"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse versioning XML", http.StatusBadRequest)
		return
	}

	if req.Status != "Enabled" && req.Status != "Suspended" {
		writeS3Error(w, "IllegalVersioningConfigurationException", "Status must be Enabled or Suspended", http.StatusBadRequest)
		return
	}

	if err := h.store.SetBucketVersioning(bucket, req.Status); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// GetBucketVersioning handles GET /{bucket}?versioning.
func (h *BucketHandler) GetBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	status, _ := h.store.GetBucketVersioning(bucket)

	type versioningConfig struct {
		XMLName xml.Name `xml:"VersioningConfiguration"`
		Xmlns   string   `xml:"xmlns,attr"`
		Status  string   `xml:"Status,omitempty"`
	}

	writeXML(w, http.StatusOK, versioningConfig{
		Xmlns:  "http://s3.amazonaws.com/doc/2006-03-01/",
		Status: status,
	})
}

// PutBucketWebsite handles PUT /{bucket}?website.
func (h *BucketHandler) PutBucketWebsite(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	var req struct {
		XMLName       xml.Name `xml:"WebsiteConfiguration"`
		IndexDocument struct {
			Suffix string `xml:"Suffix"`
		} `xml:"IndexDocument"`
		ErrorDocument struct {
			Key string `xml:"Key"`
		} `xml:"ErrorDocument"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse website XML", http.StatusBadRequest)
		return
	}

	if req.IndexDocument.Suffix == "" {
		writeS3Error(w, "InvalidArgument", "IndexDocument Suffix is required", http.StatusBadRequest)
		return
	}

	// Validate IndexDocument and ErrorDocument against path traversal
	for _, segment := range strings.Split(req.IndexDocument.Suffix, "/") {
		if segment == ".." {
			writeS3Error(w, "InvalidArgument", "IndexDocument must not contain '..' segments", http.StatusBadRequest)
			return
		}
	}
	if req.ErrorDocument.Key != "" {
		for _, segment := range strings.Split(req.ErrorDocument.Key, "/") {
			if segment == ".." {
				writeS3Error(w, "InvalidArgument", "ErrorDocument must not contain '..' segments", http.StatusBadRequest)
				return
			}
		}
	}

	if err := h.store.PutWebsiteConfig(bucket, metadata.WebsiteConfig{
		IndexDocument: req.IndexDocument.Suffix,
		ErrorDocument: req.ErrorDocument.Key,
	}); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// GetBucketWebsite handles GET /{bucket}?website.
func (h *BucketHandler) GetBucketWebsite(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	cfg, err := h.store.GetWebsiteConfig(bucket)
	if err != nil {
		writeS3Error(w, "NoSuchWebsiteConfiguration", "No website configuration", http.StatusNotFound)
		return
	}

	type xmlIndex struct {
		Suffix string `xml:"Suffix"`
	}
	type xmlError struct {
		Key string `xml:"Key,omitempty"`
	}
	type xmlWebsiteConfig struct {
		XMLName       xml.Name  `xml:"WebsiteConfiguration"`
		Xmlns         string    `xml:"xmlns,attr"`
		IndexDocument xmlIndex  `xml:"IndexDocument"`
		ErrorDocument *xmlError `xml:"ErrorDocument,omitempty"`
	}

	resp := xmlWebsiteConfig{
		Xmlns:         "http://s3.amazonaws.com/doc/2006-03-01/",
		IndexDocument: xmlIndex{Suffix: cfg.IndexDocument},
	}
	if cfg.ErrorDocument != "" {
		resp.ErrorDocument = &xmlError{Key: cfg.ErrorDocument}
	}

	writeXML(w, http.StatusOK, resp)
}

// DeleteBucketWebsite handles DELETE /{bucket}?website.
func (h *BucketHandler) DeleteBucketWebsite(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	h.store.DeleteWebsiteConfig(bucket)
	w.WriteHeader(http.StatusNoContent)
}

// PutBucketCORS handles PUT /{bucket}?cors.
func (h *BucketHandler) PutBucketCORS(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	var req struct {
		XMLName xml.Name `xml:"CORSConfiguration"`
		Rules   []struct {
			AllowedOrigin []string `xml:"AllowedOrigin"`
			AllowedMethod []string `xml:"AllowedMethod"`
			AllowedHeader []string `xml:"AllowedHeader"`
			MaxAgeSeconds int      `xml:"MaxAgeSeconds"`
		} `xml:"CORSRule"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse CORS XML", http.StatusBadRequest)
		return
	}

	if len(req.Rules) == 0 {
		writeS3Error(w, "InvalidArgument", "At least one CORSRule is required", http.StatusBadRequest)
		return
	}

	var rules []metadata.CORSRule
	for _, r := range req.Rules {
		rules = append(rules, metadata.CORSRule{
			AllowedOrigins: r.AllowedOrigin,
			AllowedMethods: r.AllowedMethod,
			AllowedHeaders: r.AllowedHeader,
			MaxAgeSecs:     r.MaxAgeSeconds,
		})
	}

	if err := h.store.PutCORSConfig(bucket, metadata.CORSConfig{Rules: rules}); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// GetBucketCORS handles GET /{bucket}?cors.
func (h *BucketHandler) GetBucketCORS(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	cfg, err := h.store.GetCORSConfig(bucket)
	if err != nil {
		writeS3Error(w, "NoSuchCORSConfiguration", "No CORS configuration", http.StatusNotFound)
		return
	}

	type xmlCORSRule struct {
		AllowedOrigin []string `xml:"AllowedOrigin"`
		AllowedMethod []string `xml:"AllowedMethod"`
		AllowedHeader []string `xml:"AllowedHeader,omitempty"`
		MaxAgeSeconds int      `xml:"MaxAgeSeconds,omitempty"`
	}
	type xmlCORSConfig struct {
		XMLName xml.Name      `xml:"CORSConfiguration"`
		Xmlns   string        `xml:"xmlns,attr"`
		Rules   []xmlCORSRule `xml:"CORSRule"`
	}

	var rules []xmlCORSRule
	for _, r := range cfg.Rules {
		rules = append(rules, xmlCORSRule{
			AllowedOrigin: r.AllowedOrigins,
			AllowedMethod: r.AllowedMethods,
			AllowedHeader: r.AllowedHeaders,
			MaxAgeSeconds: r.MaxAgeSecs,
		})
	}

	writeXML(w, http.StatusOK, xmlCORSConfig{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Rules: rules,
	})
}

// DeleteBucketCORS handles DELETE /{bucket}?cors.
func (h *BucketHandler) DeleteBucketCORS(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	h.store.DeleteCORSConfig(bucket)
	w.WriteHeader(http.StatusNoContent)
}

// PutBucketNotification handles PUT /{bucket}?notification.
func (h *BucketHandler) PutBucketNotification(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	type xmlFilterRule struct {
		Name  string `xml:"Name"`
		Value string `xml:"Value"`
	}
	type xmlS3Key struct {
		FilterRules []xmlFilterRule `xml:"FilterRule"`
	}
	type xmlFilter struct {
		S3Key xmlS3Key `xml:"S3Key"`
	}
	type xmlTopicConfig struct {
		ID     string    `xml:"Id"`
		Topic  string    `xml:"Topic"`
		Events []string  `xml:"Event"`
		Filter xmlFilter `xml:"Filter"`
	}
	type xmlNotificationConfig struct {
		XMLName xml.Name         `xml:"NotificationConfiguration"`
		Topics  []xmlTopicConfig `xml:"TopicConfiguration"`
	}

	var req xmlNotificationConfig
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse notification configuration", http.StatusBadRequest)
		return
	}

	cfg := metadata.BucketNotificationConfig{}
	for _, tc := range req.Topics {
		if tc.Topic == "" {
			writeS3Error(w, "InvalidArgument", "Topic (endpoint URL) is required", http.StatusBadRequest)
			return
		}
		if err := validateEndpointURL(tc.Topic); err != nil {
			writeS3Error(w, "InvalidArgument", fmt.Sprintf("Invalid endpoint URL: %v", err), http.StatusBadRequest)
			return
		}
		var filters []metadata.NotificationFilterRule
		for _, fr := range tc.Filter.S3Key.FilterRules {
			filters = append(filters, metadata.NotificationFilterRule{Name: fr.Name, Value: fr.Value})
		}
		id := tc.ID
		if id == "" {
			id = generateVersionID()[:8]
		}
		cfg.Webhooks = append(cfg.Webhooks, metadata.NotificationEndpointConfig{
			ID:       id,
			Endpoint: tc.Topic,
			Events:   tc.Events,
			Filters:  filters,
		})
	}

	if err := h.store.PutNotificationConfig(bucket, cfg); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetBucketNotification handles GET /{bucket}?notification.
func (h *BucketHandler) GetBucketNotification(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	cfg, err := h.store.GetNotificationConfig(bucket)
	if err != nil {
		// No config — return empty notification configuration
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></NotificationConfiguration>`))
		return
	}

	type xmlFilterRule struct {
		XMLName xml.Name `xml:"FilterRule"`
		Name    string   `xml:"Name"`
		Value   string   `xml:"Value"`
	}
	type xmlS3Key struct {
		XMLName     xml.Name        `xml:"S3Key"`
		FilterRules []xmlFilterRule `xml:"FilterRule"`
	}
	type xmlFilter struct {
		XMLName xml.Name `xml:"Filter"`
		S3Key   xmlS3Key `xml:"S3Key"`
	}
	type xmlTopicConfig struct {
		XMLName xml.Name   `xml:"TopicConfiguration"`
		ID      string     `xml:"Id"`
		Topic   string     `xml:"Topic"`
		Events  []string   `xml:"Event"`
		Filter  *xmlFilter `xml:"Filter,omitempty"`
	}
	type xmlNotificationConfig struct {
		XMLName xml.Name         `xml:"NotificationConfiguration"`
		Xmlns   string           `xml:"xmlns,attr"`
		Topics  []xmlTopicConfig `xml:"TopicConfiguration"`
	}

	resp := xmlNotificationConfig{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	for _, wh := range cfg.Webhooks {
		tc := xmlTopicConfig{
			ID:     wh.ID,
			Topic:  wh.Endpoint,
			Events: wh.Events,
		}
		if len(wh.Filters) > 0 {
			filter := &xmlFilter{}
			for _, f := range wh.Filters {
				filter.S3Key.FilterRules = append(filter.S3Key.FilterRules, xmlFilterRule{Name: f.Name, Value: f.Value})
			}
			tc.Filter = filter
		}
		resp.Topics = append(resp.Topics, tc)
	}

	writeXML(w, http.StatusOK, resp)
}

// DeleteBucketNotification handles DELETE /{bucket}?notification.
func (h *BucketHandler) DeleteBucketNotification(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	h.store.DeleteNotificationConfig(bucket)
	w.WriteHeader(http.StatusNoContent)
}

// PutBucketEncryption handles PUT /{bucket}?encryption.
func (h *BucketHandler) PutBucketEncryption(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	var req struct {
		XMLName xml.Name `xml:"ServerSideEncryptionConfiguration"`
		Rules   []struct {
			DefaultEncryption struct {
				SSEAlgorithm string `xml:"SSEAlgorithm"`
				KMSKeyID     string `xml:"KMSMasterKeyID"`
			} `xml:"ApplyServerSideEncryptionByDefault"`
		} `xml:"Rule"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse encryption XML", http.StatusBadRequest)
		return
	}
	if len(req.Rules) == 0 {
		writeS3Error(w, "InvalidArgument", "At least one rule is required", http.StatusBadRequest)
		return
	}
	rule := req.Rules[0]
	if err := h.store.PutEncryptionConfig(bucket, metadata.BucketEncryptionConfig{
		SSEAlgorithm: rule.DefaultEncryption.SSEAlgorithm,
		KMSKeyID:     rule.DefaultEncryption.KMSKeyID,
	}); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetBucketEncryption handles GET /{bucket}?encryption.
func (h *BucketHandler) GetBucketEncryption(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	cfg, err := h.store.GetEncryptionConfig(bucket)
	if err != nil {
		writeS3Error(w, "ServerSideEncryptionConfigurationNotFoundError", "No encryption configuration", http.StatusNotFound)
		return
	}
	type xmlDefault struct {
		SSEAlgorithm string `xml:"SSEAlgorithm"`
		KMSKeyID     string `xml:"KMSMasterKeyID,omitempty"`
	}
	type xmlRule struct {
		DefaultEncryption xmlDefault `xml:"ApplyServerSideEncryptionByDefault"`
	}
	type xmlSSEConfig struct {
		XMLName xml.Name  `xml:"ServerSideEncryptionConfiguration"`
		Xmlns   string    `xml:"xmlns,attr"`
		Rules   []xmlRule `xml:"Rule"`
	}
	writeXML(w, http.StatusOK, xmlSSEConfig{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Rules: []xmlRule{{
			DefaultEncryption: xmlDefault{SSEAlgorithm: cfg.SSEAlgorithm, KMSKeyID: cfg.KMSKeyID},
		}},
	})
}

// DeleteBucketEncryption handles DELETE /{bucket}?encryption.
func (h *BucketHandler) DeleteBucketEncryption(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	h.store.DeleteEncryptionConfig(bucket)
	w.WriteHeader(http.StatusNoContent)
}

// PutPublicAccessBlock handles PUT /{bucket}?publicAccessBlock.
func (h *BucketHandler) PutPublicAccessBlock(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	var req struct {
		XMLName               xml.Name `xml:"PublicAccessBlockConfiguration"`
		BlockPublicAcls       bool     `xml:"BlockPublicAcls"`
		IgnorePublicAcls      bool     `xml:"IgnorePublicAcls"`
		BlockPublicPolicy     bool     `xml:"BlockPublicPolicy"`
		RestrictPublicBuckets bool     `xml:"RestrictPublicBuckets"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse public access block XML", http.StatusBadRequest)
		return
	}
	if err := h.store.PutPublicAccessBlock(bucket, metadata.PublicAccessBlockConfig{
		BlockPublicAcls:       req.BlockPublicAcls,
		IgnorePublicAcls:      req.IgnorePublicAcls,
		BlockPublicPolicy:     req.BlockPublicPolicy,
		RestrictPublicBuckets: req.RestrictPublicBuckets,
	}); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetPublicAccessBlock handles GET /{bucket}?publicAccessBlock.
func (h *BucketHandler) GetPublicAccessBlock(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	cfg, err := h.store.GetPublicAccessBlock(bucket)
	if err != nil {
		writeS3Error(w, "NoSuchPublicAccessBlockConfiguration", "No public access block configuration", http.StatusNotFound)
		return
	}
	type xmlPAB struct {
		XMLName               xml.Name `xml:"PublicAccessBlockConfiguration"`
		Xmlns                 string   `xml:"xmlns,attr"`
		BlockPublicAcls       bool     `xml:"BlockPublicAcls"`
		IgnorePublicAcls      bool     `xml:"IgnorePublicAcls"`
		BlockPublicPolicy     bool     `xml:"BlockPublicPolicy"`
		RestrictPublicBuckets bool     `xml:"RestrictPublicBuckets"`
	}
	writeXML(w, http.StatusOK, xmlPAB{
		Xmlns:                 "http://s3.amazonaws.com/doc/2006-03-01/",
		BlockPublicAcls:       cfg.BlockPublicAcls,
		IgnorePublicAcls:      cfg.IgnorePublicAcls,
		BlockPublicPolicy:     cfg.BlockPublicPolicy,
		RestrictPublicBuckets: cfg.RestrictPublicBuckets,
	})
}

// DeletePublicAccessBlock handles DELETE /{bucket}?publicAccessBlock.
func (h *BucketHandler) DeletePublicAccessBlock(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	h.store.DeletePublicAccessBlock(bucket)
	w.WriteHeader(http.StatusNoContent)
}

// PutBucketLogging handles PUT /{bucket}?logging.
func (h *BucketHandler) PutBucketLogging(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	var req struct {
		XMLName        xml.Name `xml:"BucketLoggingStatus"`
		LoggingEnabled *struct {
			TargetBucket string `xml:"TargetBucket"`
			TargetPrefix string `xml:"TargetPrefix"`
		} `xml:"LoggingEnabled"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "Could not parse logging XML", http.StatusBadRequest)
		return
	}
	if req.LoggingEnabled == nil {
		// Disable logging
		h.store.DeleteLoggingConfig(bucket)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := h.store.PutLoggingConfig(bucket, metadata.BucketLoggingConfig{
		TargetBucket: req.LoggingEnabled.TargetBucket,
		TargetPrefix: req.LoggingEnabled.TargetPrefix,
	}); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetBucketLogging handles GET /{bucket}?logging.
func (h *BucketHandler) GetBucketLogging(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	cfg, err := h.store.GetLoggingConfig(bucket)
	if err != nil {
		// No logging config — return empty BucketLoggingStatus
		type xmlLoggingStatus struct {
			XMLName xml.Name `xml:"BucketLoggingStatus"`
			Xmlns   string   `xml:"xmlns,attr"`
		}
		writeXML(w, http.StatusOK, xmlLoggingStatus{
			Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		})
		return
	}
	type xmlLoggingEnabled struct {
		TargetBucket string `xml:"TargetBucket"`
		TargetPrefix string `xml:"TargetPrefix,omitempty"`
	}
	type xmlLoggingStatus struct {
		XMLName        xml.Name          `xml:"BucketLoggingStatus"`
		Xmlns          string            `xml:"xmlns,attr"`
		LoggingEnabled xmlLoggingEnabled `xml:"LoggingEnabled"`
	}
	writeXML(w, http.StatusOK, xmlLoggingStatus{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		LoggingEnabled: xmlLoggingEnabled{
			TargetBucket: cfg.TargetBucket,
			TargetPrefix: cfg.TargetPrefix,
		},
	})
}

func isValidBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	// Must start and end with letter or digit
	first, last := name[0], name[len(name)-1]
	if !isAlphaNum(first) || !isAlphaNum(last) {
		return false
	}
	// No consecutive dots, no ".." path traversal
	prev := byte(0)
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.') {
			return false
		}
		if c == '.' && prev == '.' {
			return false // no consecutive dots
		}
		prev = c
	}
	return true
}

func isAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// PutBucketLambda handles PUT /{bucket}?lambda — set lambda trigger configuration.
func (h *BucketHandler) PutBucketLambda(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	var cfg metadata.BucketLambdaConfig
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&cfg); err != nil {
		writeS3Error(w, "MalformedJSON", "Could not parse lambda configuration", http.StatusBadRequest)
		return
	}

	for i, t := range cfg.Triggers {
		if t.FunctionURL == "" {
			writeS3Error(w, "InvalidArgument", "function_url is required", http.StatusBadRequest)
			return
		}
		if err := validateEndpointURL(t.FunctionURL); err != nil {
			writeS3Error(w, "InvalidArgument", fmt.Sprintf("Invalid function URL: %v", err), http.StatusBadRequest)
			return
		}
		if len(t.Events) == 0 {
			writeS3Error(w, "InvalidArgument", "events is required", http.StatusBadRequest)
			return
		}
		if t.ID == "" {
			cfg.Triggers[i].ID = generateVersionID()[:8]
		}
	}

	if err := h.store.PutLambdaConfig(bucket, cfg); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetBucketLambda handles GET /{bucket}?lambda — get lambda trigger configuration.
func (h *BucketHandler) GetBucketLambda(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	cfg, err := h.store.GetLambdaConfig(bucket)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"triggers":[]}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

// DeleteBucketLambda handles DELETE /{bucket}?lambda — remove lambda trigger configuration.
func (h *BucketHandler) DeleteBucketLambda(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}

	h.store.DeleteLambdaConfig(bucket)
	w.WriteHeader(http.StatusNoContent)
}

// GetBucketLocation handles GET /{bucket}?location.
func (h *BucketHandler) GetBucketLocation(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	type locationResult struct {
		XMLName  xml.Name `xml:"LocationConstraint"`
		Xmlns    string   `xml:"xmlns,attr"`
		Location string   `xml:",chardata"`
	}
	writeXML(w, http.StatusOK, locationResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Location: "us-east-1",
	})
}

// PutBucketTagging handles PUT /{bucket}?tagging.
func (h *BucketHandler) PutBucketTagging(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeS3Error(w, "MalformedXML", "Could not read request body", http.StatusBadRequest)
		return
	}
	var req taggingRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeS3Error(w, "MalformedXML", "Invalid tagging XML", http.StatusBadRequest)
		return
	}
	tags := make(map[string]string)
	for _, t := range req.TagSet.Tags {
		tags[t.Key] = t.Value
	}
	if err := h.store.PutBucketTags(bucket, tags); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetBucketTagging handles GET /{bucket}?tagging.
func (h *BucketHandler) GetBucketTagging(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	tags, err := h.store.GetBucketTags(bucket)
	if err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	resp := taggingResponse{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	for k, v := range tags {
		resp.TagSet.Tags = append(resp.TagSet.Tags, xmlTag{Key: k, Value: v})
	}
	writeXML(w, http.StatusOK, resp)
}

// DeleteBucketTagging handles DELETE /{bucket}?tagging.
func (h *BucketHandler) DeleteBucketTagging(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	if err := h.store.DeleteBucketTags(bucket); err != nil {
		slog.Error("internal error", "error", err)
		writeS3Error(w, "InternalError", "An internal error occurred", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetBucketACL handles GET /{bucket}?acl.
func (h *BucketHandler) GetBucketACL(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
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
	resp := aclResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner: xmlOwner{ID: "vaults3", DisplayName: "VaultS3"},
		ACL: []grant{{
			Grantee:    grantee{XMLNS: "http://www.w3.org/2001/XMLSchema-instance", Type: "CanonicalUser", ID: "vaults3", DisplayName: "VaultS3"},
			Permission: "FULL_CONTROL",
		}},
	}
	// Add public read grant if bucket is public
	if h.store.IsBucketPublicRead(bucket) {
		resp.ACL = append(resp.ACL, grant{
			Grantee:    grantee{XMLNS: "http://www.w3.org/2001/XMLSchema-instance", Type: "Group", ID: "http://acs.amazonaws.com/groups/global/AllUsers"},
			Permission: "READ",
		})
	}
	writeXML(w, http.StatusOK, resp)
}

// PutBucketACL handles PUT /{bucket}?acl — accepts but is a no-op (VaultS3 uses policies).
func (h *BucketHandler) PutBucketACL(w http.ResponseWriter, r *http.Request, bucket string) {
	if !h.store.BucketExists(bucket) {
		writeS3Error(w, "NoSuchBucket", "Bucket does not exist", http.StatusNotFound)
		return
	}
	// Consume and discard the body
	io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusOK)
}
