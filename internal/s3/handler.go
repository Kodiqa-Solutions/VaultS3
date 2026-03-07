package s3

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metrics"
	"github.com/Kodiqa-Solutions/VaultS3/internal/ratelimit"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// ActivityFunc is a callback for recording S3 activity.
type ActivityFunc func(method, bucket, key string, status int, size int64, clientIP string)

// AuditFunc is a callback for recording audit trail entries.
type AuditFunc func(principal, userID, action, resource, effect, sourceIP string, statusCode int)

// NotificationFunc is called after object mutations to trigger event notifications.
type NotificationFunc func(eventType, bucket, key string, size int64, etag, versionID string)

// ReplicationFunc is called after object mutations to enqueue replication events.
type ReplicationFunc func(eventType, bucket, key string, size int64, etag, versionID string)

// ScanFunc is called after object uploads to trigger virus scanning.
type ScanFunc func(bucket, key string, size int64)

// SearchUpdateFunc is called after object mutations to update the search index.
type SearchUpdateFunc func(eventType, bucket, key string)

// LambdaFunc is called after object mutations to trigger lambda functions.
type LambdaFunc func(eventType, bucket, key string, size int64, etag, versionID string)

// ClusterProxyFunc checks if a request should be forwarded to another node.
// Returns true if the request was proxied (caller should return immediately).
type ClusterProxyFunc func(w http.ResponseWriter, r *http.Request, bucket, key string) bool

// Handler routes incoming S3 API requests to the appropriate handler.
type Handler struct {
	store               *metadata.Store
	engine              storage.Engine
	auth                *Authenticator
	buckets             *BucketHandler
	objects             *ObjectHandler
	encryptionEnabled   bool
	domain              string // base domain for virtual-hosted style URLs
	metrics             *metrics.Collector
	onActivity          ActivityFunc
	onAudit             AuditFunc
	onNotification      NotificationFunc
	onReplication       ReplicationFunc
	onScan              ScanFunc
	onSearchUpdate      SearchUpdateFunc
	onLambda            LambdaFunc
	rateLimiter         *ratelimit.Limiter
	accessUpdater       *metadata.AccessUpdater
	replicationPeerKeys map[string]bool
	clusterProxy        ClusterProxyFunc
}

func NewHandler(store *metadata.Store, engine storage.Engine, auth *Authenticator, encryptionEnabled bool, domain string, mc *metrics.Collector) *Handler {
	h := &Handler{
		store:             store,
		engine:            engine,
		auth:              auth,
		encryptionEnabled: encryptionEnabled,
		domain:            domain,
		metrics:           mc,
	}
	h.buckets = &BucketHandler{store: store, engine: engine}
	h.objects = &ObjectHandler{store: store, engine: engine, encryptionEnabled: encryptionEnabled}
	return h
}

// SetActivityFunc sets the callback for recording S3 activity.
func (h *Handler) SetActivityFunc(fn ActivityFunc) {
	h.onActivity = fn
}

// SetAuditFunc sets the callback for recording audit trail entries.
func (h *Handler) SetAuditFunc(fn AuditFunc) {
	h.onAudit = fn
}

// SetNotificationFunc sets the callback for S3 event notifications.
func (h *Handler) SetNotificationFunc(fn NotificationFunc) {
	h.onNotification = fn
	h.objects.onNotification = fn
}

// SetReplicationFunc sets the callback for replication event enqueueing.
func (h *Handler) SetReplicationFunc(fn ReplicationFunc) {
	h.onReplication = fn
	h.objects.onReplication = fn
}

// SetScanFunc sets the callback for virus scanning after uploads.
func (h *Handler) SetScanFunc(fn ScanFunc) {
	h.onScan = fn
	h.objects.onScan = fn
}

// SetRateLimiter sets the rate limiter for the S3 handler.
func (h *Handler) SetRateLimiter(rl *ratelimit.Limiter) {
	h.rateLimiter = rl
}

// SetSearchUpdateFunc sets the callback for search index updates.
func (h *Handler) SetSearchUpdateFunc(fn SearchUpdateFunc) {
	h.onSearchUpdate = fn
	h.objects.onSearchUpdate = fn
}

// SetLambdaFunc sets the callback for lambda function triggers.
func (h *Handler) SetLambdaFunc(fn LambdaFunc) {
	h.onLambda = fn
	h.objects.onLambda = fn
}

// SetReplicationPeerKeys sets the access keys of configured replication peers.
func (h *Handler) SetReplicationPeerKeys(keys []string) {
	h.replicationPeerKeys = make(map[string]bool)
	for _, k := range keys {
		h.replicationPeerKeys[k] = true
	}
}

// isReplicationPeer checks if an access key belongs to a configured replication peer.
func (h *Handler) isReplicationPeer(accessKey string) bool {
	if h.replicationPeerKeys == nil {
		return false
	}
	return h.replicationPeerKeys[accessKey]
}

// SetClusterProxy sets the function used to proxy requests to other cluster nodes.
func (h *Handler) SetClusterProxy(fn ClusterProxyFunc) {
	h.clusterProxy = fn
}

// SetAccessUpdater sets the batched access updater.
func (h *Handler) SetAccessUpdater(u *metadata.AccessUpdater) {
	h.accessUpdater = u
	h.objects.accessUpdater = u
}

// statusWriter wraps ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Parse bucket and key — support both path-style and virtual-hosted style
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key := h.parseRequest(r.Host, path)

	slog.Debug("S3 request", "method", r.Method, "bucket", bucket, "key", key)

	// Cluster proxy: forward to the correct node if this isn't the primary
	// Skip if already proxied (X-VaultS3-Proxy header present) to prevent loops
	if h.clusterProxy != nil && r.Header.Get("X-VaultS3-Proxy") == "" {
		if h.clusterProxy(w, r, bucket, key) {
			return
		}
	}

	// Reject path traversal in keys
	if key != "" {
		for _, seg := range strings.Split(key, "/") {
			if seg == ".." {
				writeS3Error(w, "InvalidArgument", "Key must not contain '..' path segments", http.StatusBadRequest)
				return
			}
		}
	}

	// Record request metrics
	if h.metrics != nil {
		h.metrics.RecordRequest(r.Method)
		if r.ContentLength > 0 {
			h.metrics.RecordBytesIn(r.ContentLength)
		}
		if bucket != "" {
			h.metrics.RecordBucketRequest(bucket, r.Method)
			if r.ContentLength > 0 {
				h.metrics.RecordBucketBytesIn(bucket, r.ContentLength)
			}
		}
	}

	// Wrap writer to capture status code for activity log
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	w = sw
	defer func() {
		if h.onActivity != nil && bucket != "" {
			clientIP := r.RemoteAddr
			if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
				clientIP = strings.SplitN(fwd, ",", 2)[0]
			}
			h.onActivity(r.Method, bucket, key, sw.status, r.ContentLength, strings.TrimSpace(clientIP))
		}
	}()

	// Handle CORS preflight
	if r.Method == http.MethodOptions && bucket != "" {
		h.handleCORSPreflight(w, r, bucket)
		return
	}

	// Add CORS response headers if configured
	if bucket != "" {
		h.addCORSHeaders(w, r, bucket)
	}

	// Check for public-read policy bypass on GET/HEAD object requests
	authRequired := true
	if bucket != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if key != "" && h.store.IsBucketPublicRead(bucket) {
			authRequired = false
		}
		if h.store.IsBucketWebsite(bucket) {
			authRequired = false
		}
	}

	// Extract client IP — use RemoteAddr for rate limiting (tamper-proof),
	// X-Forwarded-For only for audit logging (can be spoofed)
	rateLimitIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if rateLimitIP == "" {
		rateLimitIP = r.RemoteAddr
	}
	clientIP := rateLimitIP
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		clientIP = strings.TrimSpace(strings.SplitN(fwd, ",", 2)[0])
	}

	// Rate limit check (uses RemoteAddr, not X-Forwarded-For)
	if h.rateLimiter != nil {
		accessKeyID := extractAccessKeyFromAuth(r)
		if !h.rateLimiter.Allow(rateLimitIP, accessKeyID) {
			w.Header().Set("Retry-After", "1")
			writeS3Error(w, "SlowDown", "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	// Authenticate and authorize
	if authRequired {
		identity, err := h.auth.Authenticate(r)
		if err != nil {
			// Log authentication failure for security monitoring
			if h.onAudit != nil {
				action := mapMethodToAction(r.Method, bucket, key, r.URL.Query())
				resource := formatResource(bucket, key)
				accessKey := extractAccessKeyFromAuth(r)
				h.onAudit(accessKey, "", action, resource, "Deny", clientIP, http.StatusForbidden)
			}
			writeS3Error(w, "AccessDenied", err.Error(), http.StatusForbidden)
			return
		}

		// IP access check — use RemoteAddr (tamper-proof), not X-Forwarded-For
		if err := h.auth.CheckIPAccess(identity, rateLimitIP); err != nil {
			if h.onAudit != nil {
				action := mapMethodToAction(r.Method, bucket, key, r.URL.Query())
				resource := formatResource(bucket, key)
				h.onAudit(identity.AccessKey, identity.UserID, action, resource, "Deny", clientIP, http.StatusForbidden)
			}
			writeS3Error(w, "AccessDenied", err.Error(), http.StatusForbidden)
			return
		}

		// Authorize non-admin identities
		if !identity.IsAdmin {
			action := mapMethodToAction(r.Method, bucket, key, r.URL.Query())
			resource := formatResource(bucket, key)
			if err := h.auth.Authorize(identity, action, resource); err != nil {
				if h.onAudit != nil {
					h.onAudit(identity.AccessKey, identity.UserID, action, resource, "Deny", clientIP, http.StatusForbidden)
				}
				writeS3Error(w, "AccessDenied", err.Error(), http.StatusForbidden)
				return
			}
			// Record allowed access
			if h.onAudit != nil {
				h.onAudit(identity.AccessKey, identity.UserID, action, resource, "Allow", clientIP, 0)
			}
		} else if h.onAudit != nil {
			action := mapMethodToAction(r.Method, bucket, key, r.URL.Query())
			resource := formatResource(bucket, key)
			h.onAudit(identity.AccessKey, identity.UserID, action, resource, "Allow", clientIP, 0)
		}
	}

	// Replication loop prevention: use a per-request ObjectHandler copy with
	// notification/replication/lambda callbacks disabled for replication peers.
	// This avoids mutating shared state from concurrent goroutines.
	if r.Header.Get("X-VaultS3-Replication") != "" && authRequired {
		identity, _ := h.auth.Authenticate(r)
		if identity != nil && h.isReplicationPeer(identity.AccessKey) {
			replicaObjects := *h.objects
			replicaObjects.onNotification = nil
			replicaObjects.onReplication = nil
			replicaObjects.onLambda = nil
			h = &Handler{
				store:               h.store,
				engine:              h.engine,
				auth:                h.auth,
				buckets:             h.buckets,
				objects:             &replicaObjects,
				encryptionEnabled:   h.encryptionEnabled,
				domain:              h.domain,
				metrics:             h.metrics,
				onActivity:          h.onActivity,
				onAudit:             h.onAudit,
				onNotification:      h.onNotification,
				onReplication:       h.onReplication,
				onScan:              h.onScan,
				onSearchUpdate:      h.onSearchUpdate,
				onLambda:            h.onLambda,
				rateLimiter:         h.rateLimiter,
				accessUpdater:       h.accessUpdater,
				replicationPeerKeys: h.replicationPeerKeys,
			}
		}
	}

	// Static website serving — intercept before normal routing
	if bucket != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if h.store.IsBucketWebsite(bucket) {
			// Only serve website for non-API requests (no query params like ?policy, ?versioning, etc.)
			if len(r.URL.Query()) == 0 {
				h.serveWebsite(w, r, bucket, key)
				return
			}
		}
	}

	// Route based on path and method
	switch {
	case bucket == "":
		// Service-level operations (e.g., ListBuckets)
		if r.Method == http.MethodGet {
			h.buckets.ListBuckets(w, r)
			return
		}

	case key == "":
		// Bucket-level operations
		bq := r.URL.Query()

		// Policy operations
		if _, ok := bq["policy"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketPolicy(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketPolicy(w, r, bucket)
			case http.MethodDelete:
				h.buckets.DeleteBucketPolicy(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Notification configuration
		if _, ok := bq["notification"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketNotification(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketNotification(w, r, bucket)
			case http.MethodDelete:
				h.buckets.DeleteBucketNotification(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Lambda trigger configuration
		if _, ok := bq["lambda"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketLambda(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketLambda(w, r, bucket)
			case http.MethodDelete:
				h.buckets.DeleteBucketLambda(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// CORS operations
		if _, ok := bq["cors"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketCORS(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketCORS(w, r, bucket)
			case http.MethodDelete:
				h.buckets.DeleteBucketCORS(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Website operations
		if _, ok := bq["website"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketWebsite(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketWebsite(w, r, bucket)
			case http.MethodDelete:
				h.buckets.DeleteBucketWebsite(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Lifecycle operations
		if _, ok := bq["lifecycle"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketLifecycle(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketLifecycle(w, r, bucket)
			case http.MethodDelete:
				h.buckets.DeleteBucketLifecycle(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Versioning operations
		if _, ok := bq["versioning"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketVersioning(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketVersioning(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// List object versions
		if _, ok := bq["versions"]; ok {
			if r.Method == http.MethodGet {
				h.objects.ListObjectVersions(w, r, bucket)
			} else {
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Object Lock (default retention) operations
		if _, ok := bq["object-lock"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.objects.PutBucketObjectLockConfig(w, r, bucket)
			case http.MethodGet:
				h.objects.GetBucketObjectLockConfig(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Bucket location
		if _, ok := bq["location"]; ok {
			if r.Method == http.MethodGet {
				h.buckets.GetBucketLocation(w, r, bucket)
			} else {
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Bucket tagging
		if _, ok := bq["tagging"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketTagging(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketTagging(w, r, bucket)
			case http.MethodDelete:
				h.buckets.DeleteBucketTagging(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Bucket ACL
		if _, ok := bq["acl"]; ok {
			switch r.Method {
			case http.MethodGet:
				h.buckets.GetBucketACL(w, r, bucket)
			case http.MethodPut:
				h.buckets.PutBucketACL(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Bucket encryption
		if _, ok := bq["encryption"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketEncryption(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketEncryption(w, r, bucket)
			case http.MethodDelete:
				h.buckets.DeleteBucketEncryption(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Public access block
		if _, ok := bq["publicAccessBlock"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutPublicAccessBlock(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetPublicAccessBlock(w, r, bucket)
			case http.MethodDelete:
				h.buckets.DeletePublicAccessBlock(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Replication configuration
		if _, ok := bq["replication"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketReplication(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketReplication(w, r, bucket)
			case http.MethodDelete:
				h.buckets.DeleteBucketReplication(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Bucket logging
		if _, ok := bq["logging"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketLogging(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketLogging(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// List multipart uploads
		if _, ok := bq["uploads"]; ok {
			if r.Method == http.MethodGet {
				h.objects.ListMultipartUploads(w, r, bucket)
			} else {
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Quota operations
		if _, ok := bq["quota"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.buckets.PutBucketQuota(w, r, bucket)
			case http.MethodGet:
				h.buckets.GetBucketQuota(w, r, bucket)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		switch r.Method {
		case http.MethodPut:
			h.buckets.CreateBucket(w, r, bucket)
		case http.MethodDelete:
			h.buckets.DeleteBucket(w, r, bucket)
		case http.MethodHead:
			h.buckets.HeadBucket(w, r, bucket)
		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				h.objects.ListObjects(w, r, bucket)
			} else {
				h.objects.ListObjectsV1(w, r, bucket)
			}
		case http.MethodPost:
			if _, ok := bq["delete"]; ok {
				h.objects.BatchDelete(w, r, bucket)
			} else if r.Header.Get("Content-Type") != "" && strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
				h.objects.PostUpload(w, r, bucket)
			} else {
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
		default:
			writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
		}

	default:
		// Object-level operations
		q := r.URL.Query()

		// Legal hold operations
		if _, ok := q["legal-hold"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.objects.PutObjectLegalHold(w, r, bucket, key)
			case http.MethodGet:
				h.objects.GetObjectLegalHold(w, r, bucket, key)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Retention operations
		if _, ok := q["retention"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.objects.PutObjectRetention(w, r, bucket, key)
			case http.MethodGet:
				h.objects.GetObjectRetention(w, r, bucket, key)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Object attributes
		if _, ok := q["attributes"]; ok {
			if r.Method == http.MethodGet {
				h.objects.GetObjectAttributes(w, r, bucket, key)
			} else {
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Object ACL
		if _, ok := q["acl"]; ok {
			switch r.Method {
			case http.MethodGet:
				h.objects.GetObjectACL(w, r, bucket, key)
			case http.MethodPut:
				h.objects.PutObjectACL(w, r, bucket, key)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Check for tagging operations
		if _, ok := q["tagging"]; ok {
			switch r.Method {
			case http.MethodPut:
				h.objects.PutObjectTagging(w, r, bucket, key)
			case http.MethodGet:
				h.objects.GetObjectTagging(w, r, bucket, key)
			case http.MethodDelete:
				h.objects.DeleteObjectTagging(w, r, bucket, key)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// RestoreObject
		if _, ok := q["restore"]; ok {
			if r.Method == http.MethodPost {
				h.objects.RestoreObject(w, r, bucket, key)
				return
			}
		}

		// S3 Select
		if _, ok := q["select"]; ok {
			if r.Method == http.MethodPost {
				h.objects.SelectObjectContent(w, r, bucket, key)
				return
			}
		}

		// Check for multipart upload operations
		if _, ok := q["uploads"]; ok {
			// POST /{bucket}/{key}?uploads = CreateMultipartUpload
			if r.Method == http.MethodPost {
				h.objects.CreateMultipartUpload(w, r, bucket, key)
				return
			}
		}
		if uploadID := q.Get("uploadId"); uploadID != "" {
			// Validate uploadID is hex-only to prevent path traversal
			if !isValidUploadID(uploadID) {
				writeS3Error(w, "InvalidArgument", "Invalid uploadId", http.StatusBadRequest)
				return
			}
			switch r.Method {
			case http.MethodGet:
				h.objects.ListParts(w, r, bucket, key, uploadID)
			case http.MethodPut:
				if r.Header.Get("X-Amz-Copy-Source") != "" {
					h.objects.UploadPartCopy(w, r, bucket, key, uploadID)
				} else {
					h.objects.UploadPart(w, r, bucket, key, uploadID)
				}
			case http.MethodPost:
				// POST /{bucket}/{key}?uploadId=X = CompleteMultipartUpload
				h.objects.CompleteMultipartUpload(w, r, bucket, key, uploadID)
			case http.MethodDelete:
				// DELETE /{bucket}/{key}?uploadId=X = AbortMultipartUpload
				h.objects.AbortMultipartUpload(w, r, bucket, key, uploadID)
			default:
				writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		// Validate presigned upload restrictions before processing PUT
		if r.Method == http.MethodPut {
			if err := ValidatePresignedRestrictions(r, bucket, key); err != nil {
				writeS3Error(w, "AccessDenied", err.Error(), http.StatusForbidden)
				return
			}
		}

		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("X-Amz-Copy-Source") != "" {
				h.objects.CopyObject(w, r, bucket, key)
			} else {
				h.objects.PutObject(w, r, bucket, key)
			}
		case http.MethodGet:
			h.objects.GetObject(w, r, bucket, key)
		case http.MethodDelete:
			h.objects.DeleteObject(w, r, bucket, key)
		case http.MethodHead:
			h.objects.HeadObject(w, r, bucket, key)
		default:
			writeS3Error(w, "MethodNotAllowed", "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// parseRequest extracts bucket and key from the request.
// Supports both virtual-hosted style (bucket.domain/key) and path-style (domain/bucket/key).
func (h *Handler) parseRequest(host, path string) (bucket, key string) {
	// Strip port from host
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}

	// Try virtual-hosted style if domain is configured
	if h.domain != "" && strings.HasSuffix(host, "."+h.domain) {
		bucket = strings.TrimSuffix(host, "."+h.domain)
		key = path
		return
	}

	// Fall back to path-style
	return parsePath(path)
}

func parsePath(path string) (bucket, key string) {
	if path == "" {
		return "", ""
	}
	parts := strings.SplitN(path, "/", 2)
	bucket = parts[0]
	if len(parts) > 1 {
		key = parts[1]
	}
	return
}

// mapMethodToAction maps an HTTP method + context to an S3 IAM action.
func mapMethodToAction(method, bucket, key string, query map[string][]string) string {
	if key != "" {
		switch method {
		case http.MethodGet, http.MethodHead:
			return "s3:GetObject"
		case http.MethodPut:
			return "s3:PutObject"
		case http.MethodDelete:
			return "s3:DeleteObject"
		}
	}

	if bucket != "" && key == "" {
		// Bucket-level operations
		if _, ok := query["policy"]; ok {
			if method == http.MethodPut {
				return "s3:PutBucketPolicy"
			}
			return "s3:GetBucketPolicy"
		}
		switch method {
		case http.MethodPut:
			return "s3:CreateBucket"
		case http.MethodDelete:
			return "s3:DeleteBucket"
		case http.MethodGet:
			return "s3:ListBucket"
		case http.MethodHead:
			return "s3:ListBucket"
		}
	}

	if bucket == "" && method == http.MethodGet {
		return "s3:ListAllMyBuckets"
	}

	return "s3:*"
}

// formatResource creates an S3 ARN from bucket and key.
func formatResource(bucket, key string) string {
	if bucket == "" {
		return "*"
	}
	if key == "" {
		return "arn:aws:s3:::" + bucket
	}
	return "arn:aws:s3:::" + bucket + "/" + key
}

// handleCORSPreflight handles OPTIONS requests for CORS preflight.
func (h *Handler) handleCORSPreflight(w http.ResponseWriter, r *http.Request, bucket string) {
	cfg, err := h.store.GetCORSConfig(bucket)
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	origin := r.Header.Get("Origin")
	requestMethod := r.Header.Get("Access-Control-Request-Method")

	for _, rule := range cfg.Rules {
		if !matchOrigin(rule.AllowedOrigins, origin) {
			continue
		}
		if !matchMethod(rule.AllowedMethods, requestMethod) {
			continue
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))
		if len(rule.AllowedHeaders) > 0 {
			w.Header().Set("Access-Control-Allow-Headers", strings.Join(rule.AllowedHeaders, ", "))
		}
		if rule.MaxAgeSecs > 0 {
			w.Header().Set("Access-Control-Max-Age", fmt.Sprintf("%d", rule.MaxAgeSecs))
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusForbidden)
}

// addCORSHeaders adds CORS response headers if a matching rule exists.
func (h *Handler) addCORSHeaders(w http.ResponseWriter, r *http.Request, bucket string) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}

	cfg, err := h.store.GetCORSConfig(bucket)
	if err != nil {
		return
	}

	for _, rule := range cfg.Rules {
		if matchOrigin(rule.AllowedOrigins, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			return
		}
	}
}

func matchOrigin(allowed []string, origin string) bool {
	for _, a := range allowed {
		if a == "*" || a == origin {
			return true
		}
	}
	return false
}

func matchMethod(allowed []string, method string) bool {
	for _, a := range allowed {
		if strings.EqualFold(a, method) {
			return true
		}
	}
	return false
}

// isValidUploadID checks that an upload ID contains only hex characters.
func isValidUploadID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// extractAccessKeyFromAuth quickly extracts the access key from an Authorization header
// without performing full signature validation. Returns empty string if not found.
func extractAccessKeyFromAuth(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	// AWS4-HMAC-SHA256 Credential=ACCESS_KEY/date/region/s3/aws4_request, ...
	if idx := strings.Index(auth, "Credential="); idx != -1 {
		rest := auth[idx+len("Credential="):]
		if slash := strings.Index(rest, "/"); slash != -1 {
			return rest[:slash]
		}
	}
	// Check query string auth (presigned URLs)
	if key := r.URL.Query().Get("X-Amz-Credential"); key != "" {
		if slash := strings.Index(key, "/"); slash != -1 {
			return key[:slash]
		}
	}
	return ""
}
