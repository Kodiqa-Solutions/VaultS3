package api

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Kodiqa-Solutions/VaultS3/internal/backup"
	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/erasure"
	"github.com/Kodiqa-Solutions/VaultS3/internal/lambda"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metrics"
	"github.com/Kodiqa-Solutions/VaultS3/internal/migrate"
	"github.com/Kodiqa-Solutions/VaultS3/internal/ratelimit"
	s3auth "github.com/Kodiqa-Solutions/VaultS3/internal/s3"
	"github.com/Kodiqa-Solutions/VaultS3/internal/scanner"
	"github.com/Kodiqa-Solutions/VaultS3/internal/search"
	"github.com/Kodiqa-Solutions/VaultS3/internal/selfupdate"
	"github.com/Kodiqa-Solutions/VaultS3/internal/snapshot"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
	"github.com/Kodiqa-Solutions/VaultS3/internal/sysinfo"
	"github.com/Kodiqa-Solutions/VaultS3/internal/tiering"
	"github.com/Kodiqa-Solutions/VaultS3/internal/vector"
)

// APIHandler serves the dashboard REST API at /api/v1/.
type APIHandler struct {
	allowPrivateLambda bool
	store              metadata.StoreAPI
	engine             storage.Engine
	keyMgr             *bucketcrypto.Manager // per-bucket encryption keys (nil if unconfigured)
	metrics            *metrics.Collector
	cfg                *config.Config
	jwt                *JWTService
	activity           *ActivityLog
	searchIndex        *search.Index
	vectorMgr          *vector.Manager
	migrator           *migrate.Manager
	updater            *selfupdate.Updater
	snapshots          *snapshot.Manager
	scanner            *scanner.Scanner
	tieringMgr         *tiering.Manager
	ecHealer           *erasure.Healer
	peerScheme         string
	peerAddr           func(nodeID string) string
	bucketEncrypted    func(bucket string) bool
	metaBarrier        func() error
	backupSched        *backup.Scheduler
	rateLimiter        *ratelimit.Limiter
	oidc               *OIDCValidator
	lambdaMgr          *lambda.TriggerManager
	eventBus           *EventBus
	logBroadcaster     *LogBroadcaster
	traceBroadcaster   *TraceBroadcaster
	s3Auth             *s3auth.Authenticator
	onReplication      ReplicationFunc
	clusterProxy       ClusterProxyFunc                        // proxy a single-object request to its owner (download/delete)
	clusterOwner       func(bucket, key string) (string, bool) // owner API addr for placement (upload); ("",false) if local
	clusterSelfID      string                                  // this node's cluster ID ("" if single-node)
	clusterNodeAddrs   func() map[string]string                // nodeID -> peer addr for all cluster nodes (nil if single-node)
	clusterSecret      string                                  // shared secret for the inter-node /cluster/sysinfo call
	clusterCtl         ClusterController                       // membership ops (join/leave/status); nil if single-node
	usage              *sysinfo.UsageCache                     // measured on-disk footprint; built lazily, nil when disabled
	usageOnce          sync.Once                               // guards building usage
	writable           *atomic.Bool                            // node-local write gate (drain); nil ⇒ always writable
	triggerRebalance   func()                                  // kick a background rebalance pass (nil if single-node)
	triggerRepair      func()                                  // kick a background replica repair pass (nil if single-node)
	repairStatus       func() any                              // last repair scan outcome
	rebalanceRunning   func() bool                             // whether a rebalance is in progress
	// localStore is the node-local metadata store, which differs from store only
	// in a cluster (store is then Raft-backed). In-progress multipart state is kept
	// node-local (issue #32), so anything asking "does this upload exist here?"
	// must ask this one. Nil ⇒ same as store.
	localStore metadata.StoreAPI
	// jwtKeyStore persists the console signing key across restarts and across a
	// credential rotation. Nil leaves the key in memory only.
	jwtKeyStore jwtKeyStore
	// logins throttles the credential endpoints, always on and independent of
	// the general rate limiter.
	logins *loginThrottle
	// authMu guards the admin credential pair in cfg.Auth and the console signer
	// in jwt. Changing the admin password replaces both while logins and every
	// authenticated request read them, and it used to do so with no
	// synchronization at all.
	authMu sync.RWMutex
	// speedtestRunning and reencryptRunning let one run of each at a time.
	speedtestRunning atomic.Bool
	reencryptRunning atomic.Bool
}

// adminCredentials returns the current admin access and secret key.
func (h *APIHandler) adminCredentials() (accessKey, secretKey string) {
	h.authMu.RLock()
	defer h.authMu.RUnlock()
	return h.cfg.Auth.AdminAccessKey, h.cfg.Auth.AdminSecretKey
}

// jwtService returns the console session signer in use.
func (h *APIHandler) jwtService() *JWTService {
	h.authMu.RLock()
	defer h.authMu.RUnlock()
	return h.jwt
}

// jwtKeyStore is the slice of the metadata store that holds the console signing
// key. Narrow on purpose: the console does not need the rest of the store to
// rotate a key.
type jwtKeyStore interface {
	SetJWTSigningKey(key []byte) error
}

// SetJWTSigningKey installs the per-installation console signing key, which the
// server loads or generates at startup and persists. Called before serving.
func (h *APIHandler) SetJWTSigningKey(key []byte) {
	svc := NewJWTService(key)
	h.authMu.Lock()
	h.jwt = svc
	h.authMu.Unlock()
}

// SetJWTKeyStore wires where a rotated signing key is persisted.
func (h *APIHandler) SetJWTKeyStore(s jwtKeyStore) { h.jwtKeyStore = s }

// SetLocalStore wires the node-local metadata store, used for state that is
// deliberately not replicated (in-progress multipart uploads). No-op single-node.
func (h *APIHandler) SetLocalStore(local metadata.StoreAPI) { h.localStore = local }

// ReplicationFunc is called after a dashboard-initiated object mutation so the
// write is replicated to peers, mirroring the S3 API path. Without this, objects
// uploaded or deleted through the web UI never enqueue replication events.
type ReplicationFunc func(eventType, bucket, key string, size int64, etag, versionID string)

// ClusterProxyFunc forwards a single-object request to the node that owns it,
// returning true if it handled the request.
type ClusterProxyFunc func(w http.ResponseWriter, r *http.Request, bucket, key string) bool

// SetClusterRouting wires the cluster object-placement hooks (no-op single-node).
func (h *APIHandler) SetClusterRouting(proxy ClusterProxyFunc, owner func(bucket, key string) (string, bool)) {
	h.clusterProxy = proxy
	h.clusterOwner = owner
}

// SetClusterInfo wires the cluster-wide capacity rollup (no-op single-node).
// nodeAddrs returns the current nodeID -> peer-address map; secret authenticates
// the inter-node /cluster/sysinfo call.
func (h *APIHandler) SetClusterInfo(selfID string, nodeAddrs func() map[string]string, secret string) {
	h.clusterSelfID = selfID
	h.clusterNodeAddrs = nodeAddrs
	h.clusterSecret = secret
}

func NewAPIHandler(store metadata.StoreAPI, engine storage.Engine, mc *metrics.Collector, cfg *config.Config, activity *ActivityLog) *APIHandler {
	return &APIHandler{
		store:   store,
		engine:  engine,
		metrics: mc,
		cfg:     cfg,
		// A random key until the server installs the persisted one. Signing with
		// an ephemeral key means a bug that loses the real one costs everyone
		// their session, which is recoverable; signing with a derived one meant
		// anyone holding the admin secret could forge sessions, which is not.
		jwt:      NewJWTService(nil),
		logins:   newLoginThrottle(),
		activity: activity,
	}
}

// SetReplicationFunc wires the replication enqueue callback so dashboard uploads
// and deletes replicate to peers just like writes via the S3 API.
func (h *APIHandler) SetReplicationFunc(fn ReplicationFunc) {
	h.onReplication = fn
}

// SetSearchIndex sets the search index for the API handler.
func (h *APIHandler) SetSearchIndex(idx *search.Index) {
	h.searchIndex = idx
}

// SetVectorManager sets the vector / semantic-search manager.
func (h *APIHandler) SetVectorManager(m *vector.Manager) {
	h.vectorMgr = m
}

// SetMigrator sets the S3 migration manager.
func (h *APIHandler) SetMigrator(m *migrate.Manager) {
	h.migrator = m
}

// SetUpdater sets the self-update checker.
func (h *APIHandler) SetUpdater(u *selfupdate.Updater) {
	h.updater = u
}

// SetSnapshotManager sets the bucket-snapshot (git-for-buckets) manager.
func (h *APIHandler) SetSnapshotManager(m *snapshot.Manager) {
	h.snapshots = m
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
// SetAllowPrivateLambda mirrors lambda.allow_private_endpoints for the
// dashboard's trigger configuration.
func (h *APIHandler) SetAllowPrivateLambda(allow bool) { h.allowPrivateLambda = allow }

func (h *APIHandler) SetS3Authenticator(auth *s3auth.Authenticator) {
	h.s3Auth = auth
}

// SetHealer sets the erasure-coding healer used by the manual heal endpoint.
func (h *APIHandler) SetHealer(healer *erasure.Healer) {
	h.ecHealer = healer
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
	if path == "/auth/oidc/start" && r.Method == http.MethodPost {
		h.handleOIDCStart(w, r)
		return
	}
	if path == "/auth/oidc/callback" && r.Method == http.MethodPost {
		h.handleOIDCCallback(w, r)
		return
	}

	// All other routes require JWT
	user, err := h.authenticateUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Everything is admin-only unless it is on the short list a non-admin
	// session needs to use the dashboard. The gate used to be the other way
	// round, a list of admin prefixes, so every route nobody remembered to add
	// was open to any session, OIDC auto-created users with no policies
	// included: the live log and trace streams, every recent S3 call with its
	// client address, every bucket's name and size, semantic search across all
	// buckets, webhook URLs carrying their tokens, the data directories, and a
	// replica repair anyone could start. A new route is now admin-only until
	// someone decides otherwise.
	if user != "admin" {
		if !nonAdminRoute(r.Method, path) {
			writeError(w, http.StatusForbidden, "admin access required")
			return
		}
		if err := h.checkConsoleIP(r, user); err != nil {
			writeError(w, http.StatusForbidden, "access denied: "+err.Error())
			return
		}
	}

	// Every route that names a bucket is authorized against the caller's IAM
	// policies, the same ones the S3 API enforces. Without this the console was
	// a complete bypass of the authorization model: any valid session could
	// read, overwrite and delete objects in any bucket, and change bucket
	// configuration (security assessment finding 14).
	if rest, ok := strings.CutPrefix(path, "/buckets/"); ok {
		if err := h.authorizeConsoleBucket(r, rest); err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
	}

	switch {
	case path == "/auth/me" && r.Method == http.MethodGet:
		h.handleMe(w, r)

	// Bucket routes
	case path == "/buckets" && r.Method == http.MethodGet:
		h.handleListBuckets(w, r)

	case path == "/buckets" && r.Method == http.MethodPost:
		h.handleCreateBucket(w, r, user)

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

	// Vector / semantic search
	case path == "/vectors/query" && r.Method == http.MethodPost:
		h.handleVectorQuery(w, r)
	case path == "/vectors/status" && r.Method == http.MethodGet:
		h.handleVectorStatus(w, r)

	// Version / update status
	case path == "/version" && r.Method == http.MethodGet:
		h.handleVersion(w, r)

	// System / capacity overview (version, disk total/used/free, object usage)
	case path == "/system" && r.Method == http.MethodGet:
		h.handleSystemInfo(w, r)

	// Cluster-wide capacity rollup across all nodes (mc-admin-info style)
	case path == "/cluster/info" && r.Method == http.MethodGet:
		h.handleClusterInfo(w, r)

	// Cluster membership + operations (status readable; mutations admin-only above)
	case path == "/cluster/status" && r.Method == http.MethodGet:
		h.handleClusterStatus(w, r)
	case path == "/cluster/shards" && r.Method == http.MethodGet:
		h.handleClusterShards(w, r)
	case path == "/cluster/join" && r.Method == http.MethodPost:
		h.handleClusterJoin(w, r)
	case path == "/cluster/leave" && r.Method == http.MethodPost:
		h.handleClusterLeave(w, r)
	case path == "/cluster/drain" && r.Method == http.MethodPost:
		h.handleClusterDrain(w, r, true)
	case path == "/cluster/undrain" && r.Method == http.MethodPost:
		h.handleClusterDrain(w, r, false)
	case path == "/cluster/rebalance" && r.Method == http.MethodPost:
		h.handleClusterRebalance(w, r)
	case path == "/cluster/repair" && r.Method == http.MethodPost:
		h.handleClusterRepair(w, r)
	case path == "/cluster/repair" && r.Method == http.MethodGet:
		h.handleClusterRepairStatus(w, r)

	// Cost estimator (TCO vs managed clouds)
	case path == "/tco" && r.Method == http.MethodGet:
		h.handleTCO(w, r)

	// Migration from an S3-compatible source
	case path == "/migrate/test" && r.Method == http.MethodPost:
		h.handleMigrateTest(w, r)
	case path == "/migrate" && r.Method == http.MethodPost:
		h.handleMigrateStart(w, r)
	case path == "/migrate/jobs" && r.Method == http.MethodGet:
		h.handleMigrateJobs(w, r)
	case path == "/migrate/cancel" && r.Method == http.MethodPost:
		h.handleMigrateCancel(w, r)
	case path == "/compact" && r.Method == http.MethodPost:
		h.handleCompact(w, r)
	case path == "/reencrypt" && r.Method == http.MethodPost:
		h.handleReencrypt(w, r)
	case path == "/reclaim" && r.Method == http.MethodPost:
		h.handleReclaim(w, r)

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
	case "encryption":
		h.handleBucketEncryption(w, r, name, keyRest)
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
	case "search":
		// /buckets/{name}/search?prefix=&q= — filter one folder level
		if r.Method == http.MethodGet {
			h.handleSearchObjects(w, r, name)
		} else {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
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
	case "snapshots":
		h.routeSnapshots(w, r, name, keyRest)
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

// nonAdminRoute reports whether a route may be called by a session that is not
// the admin. Everything else is admin-only.
//
// Bucket routes are on the list because each one is authorized against the
// caller's IAM policies by authorizeConsoleBucket. The cross-bucket read routes
// are on it because their handlers filter what they return to the buckets the
// caller may list. Creating a bucket is checked in its handler, since the
// bucket it names does not exist yet.
func nonAdminRoute(method, path string) bool {
	switch {
	case path == "/auth/me" && method == http.MethodGet,
		path == "/version" && method == http.MethodGet,
		path == "/buckets" && (method == http.MethodGet || method == http.MethodPost),
		strings.HasPrefix(path, "/buckets/"),
		path == "/stats" && method == http.MethodGet,
		path == "/activity" && method == http.MethodGet,
		path == "/tco" && method == http.MethodGet:
		return true
	}
	return false
}
