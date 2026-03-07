package api

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/Kodiqa-Solutions/VaultS3/internal/backup"
	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/lambda"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metrics"
	"github.com/Kodiqa-Solutions/VaultS3/internal/ratelimit"
	s3auth "github.com/Kodiqa-Solutions/VaultS3/internal/s3"
	"github.com/Kodiqa-Solutions/VaultS3/internal/scanner"
	"github.com/Kodiqa-Solutions/VaultS3/internal/search"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
	"github.com/Kodiqa-Solutions/VaultS3/internal/tiering"
)

// APIHandler serves the dashboard REST API at /api/v1/.
type APIHandler struct {
	store            *metadata.Store
	engine           storage.Engine
	metrics          *metrics.Collector
	cfg              *config.Config
	jwt              *JWTService
	activity         *ActivityLog
	searchIndex      *search.Index
	scanner          *scanner.Scanner
	tieringMgr       *tiering.Manager
	backupSched      *backup.Scheduler
	rateLimiter      *ratelimit.Limiter
	oidc             *OIDCValidator
	lambdaMgr        *lambda.TriggerManager
	eventBus         *EventBus
	logBroadcaster   *LogBroadcaster
	traceBroadcaster *TraceBroadcaster
	s3Auth           *s3auth.Authenticator
}

func NewAPIHandler(store *metadata.Store, engine storage.Engine, mc *metrics.Collector, cfg *config.Config, activity *ActivityLog) *APIHandler {
	return &APIHandler{
		store:    store,
		engine:   engine,
		metrics:  mc,
		cfg:      cfg,
		jwt:      NewJWTService(cfg.Auth.AdminSecretKey),
		activity: activity,
	}
}

// SetSearchIndex sets the search index for the API handler.
func (h *APIHandler) SetSearchIndex(idx *search.Index) {
	h.searchIndex = idx
}

// SetOIDCValidator sets the OIDC validator for the API handler.
func (h *APIHandler) SetOIDCValidator(v *OIDCValidator) {
	h.oidc = v
}

// SetEventBus sets the event bus for real-time event streaming.
func (h *APIHandler) SetEventBus(eb *EventBus) {
	h.eventBus = eb
}

// SetLogBroadcaster sets the log broadcaster for real-time log streaming.
func (h *APIHandler) SetLogBroadcaster(lb *LogBroadcaster) {
	h.logBroadcaster = lb
}

// SetTraceBroadcaster sets the trace broadcaster for request tracing.
func (h *APIHandler) SetTraceBroadcaster(tb *TraceBroadcaster) {
	h.traceBroadcaster = tb
}

// SetS3Authenticator sets the S3 authenticator reference for credential updates.
func (h *APIHandler) SetS3Authenticator(auth *s3auth.Authenticator) {
	h.s3Auth = auth
}

func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS: allow same-origin and configured origins only
	origin := r.Header.Get("Origin")
	if origin != "" && h.isAllowedOrigin(origin, r) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "3600")
		w.Header().Set("Vary", "Origin")
	}

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Rate limit check (before auth to protect against brute force)
	if h.rateLimiter != nil {
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		if clientIP == "" {
			clientIP = r.RemoteAddr
		}
		if !h.rateLimiter.Allow(clientIP, "") {
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	path = strings.TrimSuffix(path, "/")

	// Login does not require auth
	if path == "/auth/login" && r.Method == http.MethodPost {
		h.handleLogin(w, r)
		return
	}
	if path == "/auth/oidc" && r.Method == http.MethodPost {
		h.handleOIDCLogin(w, r)
		return
	}
	if path == "/auth/oidc/config" && r.Method == http.MethodGet {
		h.handleOIDCConfig(w, r)
		return
	}

	// All other routes require JWT
	if err := h.authenticate(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Admin-only routes: IAM, keys, STS, audit, backups, settings, lambda, presign, replication, scanner, tiering
	adminPaths := strings.HasPrefix(path, "/keys") ||
		strings.HasPrefix(path, "/iam/") ||
		strings.HasPrefix(path, "/sts/") ||
		path == "/audit" ||
		strings.HasPrefix(path, "/backups") ||
		strings.HasPrefix(path, "/lambda/") ||
		strings.HasPrefix(path, "/replication/") ||
		strings.HasPrefix(path, "/scanner/") ||
		strings.HasPrefix(path, "/tiering/") ||
		strings.HasPrefix(path, "/settings") ||
		path == "/presign"

	if adminPaths && !h.isAdminUser(r) {
		writeError(w, http.StatusForbidden, "admin access required")
		return
	}

	switch {
	case path == "/auth/me" && r.Method == http.MethodGet:
		h.handleMe(w, r)

	// Bucket routes
	case path == "/buckets" && r.Method == http.MethodGet:
		h.handleListBuckets(w, r)

	case path == "/buckets" && r.Method == http.MethodPost:
		h.handleCreateBucket(w, r)

	case strings.HasPrefix(path, "/buckets/"):
		h.routeBucket(w, r, strings.TrimPrefix(path, "/buckets/"))

	// Key management routes (admin only)
	case path == "/keys" && r.Method == http.MethodGet:
		h.handleListKeys(w, r)

	case path == "/keys" && r.Method == http.MethodPost:
		h.handleCreateKey(w, r)

	case strings.HasPrefix(path, "/keys/") && r.Method == http.MethodDelete:
		accessKey := strings.TrimPrefix(path, "/keys/")
		h.handleDeleteKey(w, r, accessKey)

	// STS routes (admin only)
	case path == "/sts/session-token" && r.Method == http.MethodPost:
		h.handleCreateSessionToken(w, r)

	// Audit trail route (admin only)
	case path == "/audit" && r.Method == http.MethodGet:
		h.handleListAudit(w, r)

	// IAM User routes (admin only)
	case path == "/iam/users" && r.Method == http.MethodGet:
		h.handleListIAMUsers(w, r)
	case path == "/iam/users" && r.Method == http.MethodPost:
		h.handleCreateIAMUser(w, r)
	case strings.HasPrefix(path, "/iam/users/"):
		h.routeIAMUser(w, r, strings.TrimPrefix(path, "/iam/users/"))

	// IAM Group routes (admin only)
	case path == "/iam/groups" && r.Method == http.MethodGet:
		h.handleListIAMGroups(w, r)
	case path == "/iam/groups" && r.Method == http.MethodPost:
		h.handleCreateIAMGroup(w, r)
	case strings.HasPrefix(path, "/iam/groups/"):
		h.routeIAMGroup(w, r, strings.TrimPrefix(path, "/iam/groups/"))

	// IAM Policy routes (admin only)
	case path == "/iam/policies" && r.Method == http.MethodGet:
		h.handleListIAMPolicies(w, r)
	case path == "/iam/policies" && r.Method == http.MethodPost:
		h.handleCreateIAMPolicy(w, r)
	case strings.HasPrefix(path, "/iam/policies/"):
		policyName := strings.TrimPrefix(path, "/iam/policies/")
		if r.Method == http.MethodDelete {
			h.handleDeleteIAMPolicy(w, r, policyName)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}

	// Notification configs
	case path == "/notifications" && r.Method == http.MethodGet:
		h.handleListNotifications(w, r)

	// Search
	case path == "/search" && r.Method == http.MethodGet:
		h.handleSearch(w, r)

	// Presigned URL generation
	case path == "/presign" && r.Method == http.MethodPost:
		h.handleGeneratePresign(w, r)

	// Scanner routes
	case path == "/scanner/status" && r.Method == http.MethodGet:
		h.handleScannerStatus(w, r)
	case path == "/scanner/quarantine" && r.Method == http.MethodGet:
		h.handleQuarantineList(w, r)

	// Versioning routes
	case path == "/versions" && r.Method == http.MethodGet:
		h.handleListVersions(w, r)
	case path == "/versions/diff" && r.Method == http.MethodGet:
		h.handleVersionDiff(w, r)
	case path == "/versions/tags" && r.Method == http.MethodGet:
		h.handleVersionTags(w, r)
	case path == "/versions/tags" && r.Method == http.MethodPost:
		h.handleCreateTag(w, r)
	case path == "/versions/tags" && r.Method == http.MethodDelete:
		h.handleDeleteTag(w, r)
	case path == "/versions/rollback" && r.Method == http.MethodPost:
		h.handleRollback(w, r)

	// Tiering routes
	case path == "/tiering/status" && r.Method == http.MethodGet:
		h.handleTieringStatus(w, r)
	case path == "/tiering/migrate" && r.Method == http.MethodPost:
		h.handleTieringMigrate(w, r)

	// Rate limit route
	case path == "/ratelimit/status" && r.Method == http.MethodGet:
		h.handleRateLimitStatus(w, r)

	// Backup routes
	case path == "/backups" && r.Method == http.MethodGet:
		h.handleBackupList(w, r)
	case path == "/backups/trigger" && r.Method == http.MethodPost:
		h.handleBackupTrigger(w, r)
	case path == "/backups/status" && r.Method == http.MethodGet:
		h.handleBackupStatus(w, r)

	// Lambda trigger routes (admin only)
	case strings.HasPrefix(path, "/lambda/"):
		h.routeLambda(w, r, strings.TrimPrefix(path, "/lambda/"))

	// Replication routes
	case path == "/replication/status" && r.Method == http.MethodGet:
		h.handleReplicationStatus(w, r)
	case path == "/replication/queue" && r.Method == http.MethodGet:
		h.handleReplicationQueue(w, r)

	// Settings route (admin only)
	case path == "/settings" && r.Method == http.MethodGet:
		h.handleSettings(w, r)
	case path == "/settings/credentials" && r.Method == http.MethodPut:
		h.handleChangeCredentials(w, r)

	// Stats route
	case path == "/stats" && r.Method == http.MethodGet:
		h.handleStats(w, r)

	// Activity log route
	case path == "/activity" && r.Method == http.MethodGet:
		h.handleActivity(w, r)

	// Operations: heal
	case path == "/heal" && r.Method == http.MethodPost:
		h.handleHeal(w, r)

	// Operations: speedtest
	case path == "/speedtest" && r.Method == http.MethodPost:
		h.handleSpeedtest(w, r)

	// Observability: real-time event streaming (SSE)
	case path == "/events" && r.Method == http.MethodGet:
		h.handleEvents(w, r)

	// Observability: real-time log streaming (SSE)
	case path == "/logs" && r.Method == http.MethodGet:
		h.handleLogStream(w, r)

	// Observability: request tracing (SSE)
	case path == "/trace" && r.Method == http.MethodGet:
		h.handleTrace(w, r)

	// Observability: health diagnostics
	case path == "/diagnostics" && r.Method == http.MethodGet:
		h.handleDiagnostics(w, r)

	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *APIHandler) routeIAMUser(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 2)
	userName := parts[0]

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.handleGetIAMUser(w, r, userName)
		case http.MethodDelete:
			h.handleDeleteIAMUser(w, r, userName)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	sub := parts[1]
	switch {
	case sub == "policies" && r.Method == http.MethodPost:
		h.handleAttachUserPolicy(w, r, userName)
	case strings.HasPrefix(sub, "policies/") && r.Method == http.MethodDelete:
		policyName := strings.TrimPrefix(sub, "policies/")
		h.handleDetachUserPolicy(w, r, userName, policyName)
	case sub == "groups" && r.Method == http.MethodPost:
		h.handleAddUserToGroup(w, r, userName)
	case strings.HasPrefix(sub, "groups/") && r.Method == http.MethodDelete:
		groupName := strings.TrimPrefix(sub, "groups/")
		h.handleRemoveUserFromGroup(w, r, userName, groupName)
	case sub == "ip-restrictions" && r.Method == http.MethodPut:
		h.handleSetIPRestrictions(w, r, userName)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *APIHandler) routeIAMGroup(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 2)
	groupName := parts[0]

	if len(parts) == 1 {
		if r.Method == http.MethodDelete {
			h.handleDeleteIAMGroup(w, r, groupName)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	sub := parts[1]
	switch {
	case sub == "policies" && r.Method == http.MethodPost:
		h.handleAttachGroupPolicy(w, r, groupName)
	case strings.HasPrefix(sub, "policies/") && r.Method == http.MethodDelete:
		policyName := strings.TrimPrefix(sub, "policies/")
		h.handleDetachGroupPolicy(w, r, groupName, policyName)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *APIHandler) routeBucket(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 3)
	name := parts[0]

	if len(parts) == 1 {
		// /buckets/{name}
		switch r.Method {
		case http.MethodGet:
			h.handleGetBucket(w, r, name)
		case http.MethodDelete:
			h.handleDeleteBucket(w, r, name)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	sub := parts[1]
	keyRest := ""
	if len(parts) == 3 {
		keyRest = parts[2]
	}

	switch sub {
	case "policy":
		if r.Method == http.MethodPut {
			h.handlePutBucketPolicy(w, r, name)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "quota":
		if r.Method == http.MethodPut {
			h.handlePutBucketQuota(w, r, name)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "objects":
		if keyRest == "" {
			// /buckets/{name}/objects — list
			if r.Method == http.MethodGet {
				h.handleListObjects(w, r, name)
			} else {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		} else {
			// /buckets/{name}/objects/{key...} — delete
			if r.Method == http.MethodDelete {
				h.handleDeleteObject(w, r, name, keyRest)
			} else {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		}
	case "download":
		if keyRest != "" && r.Method == http.MethodGet {
			h.handleDownload(w, r, name, keyRest)
		} else {
			writeError(w, http.StatusNotFound, "not found")
		}
	case "upload":
		if r.Method == http.MethodPost {
			h.handleUpload(w, r, name)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "bulk-delete":
		if r.Method == http.MethodPost {
			h.handleBulkDelete(w, r, name)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "download-zip":
		if r.Method == http.MethodGet {
			h.handleDownloadZip(w, r, name)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "versioning":
		switch r.Method {
		case http.MethodGet:
			h.handleGetBucketVersioning(w, r, name)
		case http.MethodPut:
			h.handlePutBucketVersioning(w, r, name)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "lifecycle":
		switch r.Method {
		case http.MethodGet:
			h.handleGetLifecycleRule(w, r, name)
		case http.MethodPut:
			h.handlePutLifecycleRule(w, r, name)
		case http.MethodDelete:
			h.handleDeleteLifecycleRule(w, r, name)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "cors":
		switch r.Method {
		case http.MethodGet:
			h.handleGetCORSConfig(w, r, name)
		case http.MethodPut:
			h.handlePutCORSConfig(w, r, name)
		case http.MethodDelete:
			h.handleDeleteCORSConfig(w, r, name)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// isAllowedOrigin checks if the request origin matches the server's configured address.
func (h *APIHandler) isAllowedOrigin(origin string, _ *http.Request) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	// Build expected host from server config (not from request Host header which is attacker-controlled)
	serverPort := fmt.Sprintf("%d", h.cfg.Server.Port)
	originHost := parsed.Hostname()
	originPort := parsed.Port()

	// Allow configured server address on server port
	configAddr := h.cfg.Server.Address
	if configAddr == "" || configAddr == "0.0.0.0" || configAddr == "::" {
		// When bound to all interfaces, allow localhost and 127.0.0.1
		if originHost == "localhost" || originHost == "127.0.0.1" {
			if originPort == serverPort || originPort == "" {
				return true
			}
		}
	} else {
		if originHost == configAddr && (originPort == serverPort || originPort == "") {
			return true
		}
	}
	// Also allow if server.domain is configured and matches
	if h.cfg.Server.Domain != "" && originHost == h.cfg.Server.Domain {
		if originPort == serverPort || originPort == "" {
			return true
		}
	}
	// Allow localhost on server port (always, for dev)
	if (originHost == "localhost" || originHost == "127.0.0.1") && originPort == serverPort {
		return true
	}
	return false
}
