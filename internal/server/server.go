package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"net/http/pprof"
	"runtime/debug"

	"github.com/Kodiqa-Solutions/VaultS3/internal/accesslog"
	"github.com/Kodiqa-Solutions/VaultS3/internal/api"
	"github.com/Kodiqa-Solutions/VaultS3/internal/backup"
	"github.com/Kodiqa-Solutions/VaultS3/internal/cluster"
	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/dashboard"
	"github.com/Kodiqa-Solutions/VaultS3/internal/erasure"
	"github.com/Kodiqa-Solutions/VaultS3/internal/lambda"
	"github.com/Kodiqa-Solutions/VaultS3/internal/lifecycle"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metrics"
	"github.com/Kodiqa-Solutions/VaultS3/internal/middleware"
	"github.com/Kodiqa-Solutions/VaultS3/internal/notify"
	"github.com/Kodiqa-Solutions/VaultS3/internal/ratelimit"
	"github.com/Kodiqa-Solutions/VaultS3/internal/replication"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
	"github.com/Kodiqa-Solutions/VaultS3/internal/scanner"
	"github.com/Kodiqa-Solutions/VaultS3/internal/search"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
	"github.com/Kodiqa-Solutions/VaultS3/internal/tiering"
)

type Server struct {
	cfg             *config.Config
	store           *metadata.Store
	engine          storage.Engine
	s3h             *s3.Handler
	metrics         *metrics.Collector
	activity        *api.ActivityLog
	accessLog       *accesslog.AccessLogger
	notifyDisp      *notify.Dispatcher
	replWorker      *replication.Worker
	biDirWorker     *replication.BiDirectionalWorker
	searchIndex     *search.Index
	scanWorker      *scanner.Scanner
	tieringMgr      *tiering.Manager
	backupSched     *backup.Scheduler
	rateLimiter     *ratelimit.Limiter
	lambdaMgr       *lambda.TriggerManager
	accessUpdater   *metadata.AccessUpdater
	clusterNode     *cluster.Node
	clusterProxy    *cluster.Proxy
	failoverProxy   *cluster.FailoverProxy
	failureDetector *cluster.FailureDetector
	rebalancer      *cluster.Rebalancer
	ecHealer        *erasure.Healer
	s3Auth          *s3.Authenticator
}

func New(cfg *config.Config) (*Server, error) {
	// Initialize storage engine
	fs, err := storage.NewFileSystem(cfg.Storage.DataDir)
	if err != nil {
		return nil, fmt.Errorf("init storage: %w", err)
	}

	var engine storage.Engine = fs

	// Wrap with compression if enabled (compress before encrypt)
	if cfg.Compression.Enabled {
		engine = storage.NewCompressedEngine(engine)
		slog.Info("compression enabled", "algorithm", "gzip")
	}

	// Wrap with encryption if enabled (SSE-S3 or SSE-KMS)
	if cfg.Encryption.Enabled {
		if cfg.Encryption.KMS.Enabled {
			// SSE-KMS: use KMS for key management
			kms := storage.NewKMS(storage.KMSConfig{
				Provider:   cfg.Encryption.KMS.Provider,
				VaultAddr:  cfg.Encryption.KMS.VaultAddr,
				VaultToken: cfg.Encryption.KMS.VaultToken,
				KeyName:    cfg.Encryption.KMS.KeyName,
				LocalKey:   cfg.Encryption.KMS.LocalKey,
			})
			keyName := cfg.Encryption.KMS.KeyName
			if keyName == "" {
				keyName = "vaults3-default"
			}
			enc, err := storage.NewKMSEncryptedEngine(engine, kms, keyName)
			if err != nil {
				return nil, fmt.Errorf("init KMS encryption: %w", err)
			}
			engine = enc
			slog.Info("SSE-KMS encryption enabled", "provider", cfg.Encryption.KMS.Provider, "key", keyName)
		} else {
			// SSE-S3: static key
			keyBytes, err := cfg.Encryption.KeyBytes()
			if err != nil {
				return nil, fmt.Errorf("encryption config: %w", err)
			}
			enc, err := storage.NewEncryptedEngine(engine, keyBytes)
			if err != nil {
				return nil, fmt.Errorf("init encryption: %w", err)
			}
			engine = enc
			slog.Info("SSE-S3 encryption enabled", "algorithm", "AES-256-GCM")
		}
	}

	// Wrap with erasure coding if enabled
	var ecEngine *erasure.Engine
	var ecHealer *erasure.Healer
	if cfg.Erasure.Enabled {
		ec, err := erasure.NewEngine(engine, cfg.Erasure)
		if err != nil {
			return nil, fmt.Errorf("init erasure coding: %w", err)
		}
		ecEngine = ec
		engine = ec
		slog.Info("erasure coding enabled",
			"data_shards", cfg.Erasure.DataShards,
			"parity_shards", cfg.Erasure.ParityShards,
			"block_size", cfg.Erasure.BlockSize,
			"extra_dirs", len(cfg.Erasure.DataDirs),
		)
	}

	// Initialize metadata store
	metaDir := cfg.Storage.MetadataDir
	if err := os.MkdirAll(metaDir, 0755); err != nil {
		return nil, fmt.Errorf("create metadata dir: %w", err)
	}
	store, err := metadata.NewStore(filepath.Join(metaDir, "vaults3.db"))
	if err != nil {
		return nil, fmt.Errorf("init metadata: %w", err)
	}

	// Initialize erasure healer if EC is enabled
	if ecEngine != nil {
		healInterval := cfg.Erasure.HealInterval
		if healInterval <= 0 {
			healInterval = 3600
		}
		ecHealer = erasure.NewHealer(store, ecEngine, healInterval)
	}

	// Initialize cluster if enabled
	var clusterNode *cluster.Node
	var clusterProxy *cluster.Proxy
	if cfg.Cluster.Enabled {
		node, err := cluster.NewNode(cfg.Cluster, store)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("init cluster: %w", err)
		}
		clusterNode = node

		// Build hash ring from configured peers + self
		vnodes := cfg.Cluster.Placement.VirtualNodes
		if vnodes <= 0 {
			vnodes = 128
		}
		ring := cluster.NewHashRing(vnodes)
		ring.AddNode(cfg.Cluster.NodeID)

		// Build peer API address map
		nodeAddrs := make(map[string]string)
		apiPort := cfg.Cluster.APIPort
		if apiPort == 0 {
			apiPort = cfg.Server.Port
		}
		nodeAddrs[cfg.Cluster.NodeID] = fmt.Sprintf("%s:%d", cfg.Cluster.BindAddr, apiPort)

		// Add peers to ring and address map
		for _, peer := range cfg.Cluster.Peers {
			nodeID, _, ok := cluster.ParsePeer(peer)
			if !ok {
				continue
			}
			ring.AddNode(nodeID)
		}
		// Explicit peer API addresses override auto-derived ones
		for nodeID, addr := range cfg.Cluster.PeerAPIs {
			nodeAddrs[nodeID] = addr
			if !ring.HasNode(nodeID) {
				ring.AddNode(nodeID)
			}
		}

		clusterProxy = cluster.NewProxy(ring, node, cfg.Cluster.Placement, nodeAddrs)
		slog.Info("cluster mode enabled",
			"node_id", cfg.Cluster.NodeID,
			"ring_nodes", ring.NodeCount(),
			"replica_count", cfg.Cluster.Placement.ReplicaCount,
		)
	}

	// Initialize failure detector and failover proxy if cluster is enabled
	var failureDetector *cluster.FailureDetector
	var failoverProxy *cluster.FailoverProxy
	var rebalancer *cluster.Rebalancer
	if clusterNode != nil && clusterProxy != nil {
		// Failure detector
		failureDetector = cluster.NewFailureDetector(cfg.Cluster.NodeID, cfg.Cluster.Detector)
		for nodeID, addr := range clusterProxy.NodeAddrs() {
			failureDetector.AddNode(nodeID, addr)
		}

		// Failover proxy wraps the basic proxy with failure awareness
		failoverProxy = cluster.NewFailoverProxy(clusterProxy, failureDetector)

		// Wire callbacks: node down/recover → failover + rebalance
		rebalancer = cluster.NewRebalancer(store, engine, clusterProxy.Ring(), clusterProxy, cfg.Cluster.NodeID, cfg.Cluster.Rebalance)
		failureDetector.SetCallbacks(
			func(nodeID string) {
				failoverProxy.OnNodeDown(nodeID)
				rebalancer.Trigger()
			},
			func(nodeID string) {
				failoverProxy.OnNodeRecover(nodeID)
				rebalancer.Trigger()
			},
		)
	}

	// Initialize S3 authenticator
	auth := s3.NewAuthenticator(cfg.Auth.AdminAccessKey, cfg.Auth.AdminSecretKey, store,
		cfg.Security.IPAllowlist, cfg.Security.IPBlocklist)

	// Load persisted admin credentials (overrides config/env if previously changed via dashboard)
	if ak, sk, err := store.GetAdminCredentials(); err == nil && ak != "" && sk != "" {
		cfg.Auth.AdminAccessKey = ak
		cfg.Auth.AdminSecretKey = sk
		auth.UpdateAdminCredentials(ak, sk)
		slog.Info("loaded persisted admin credentials")
	}

	// Initialize metrics collector
	mc := metrics.NewCollector(store, engine)

	// Initialize activity log
	activityLog := api.NewActivityLog()

	// Initialize S3 handler
	s3h := s3.NewHandler(store, engine, auth, cfg.Encryption.Enabled, cfg.Server.Domain, mc)

	// Wire cluster proxy into S3 handler (use failover proxy if available)
	if failoverProxy != nil {
		s3h.SetClusterProxy(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
			return failoverProxy.ForwardWithRetry(w, r, bucket, key)
		})
	} else if clusterProxy != nil {
		s3h.SetClusterProxy(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
			targetNode := clusterProxy.ShouldProxy(bucket, key)
			if targetNode == "" {
				return false
			}
			clusterProxy.ForwardRequest(w, r, targetNode)
			return true
		})
	}

	// Initialize access logger if enabled
	var accessLogger *accesslog.AccessLogger
	if cfg.Logging.Enabled {
		var err error
		accessLogger, err = accesslog.NewAccessLogger(cfg.Logging.FilePath)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("init access logger: %w", err)
		}
		slog.Info("access logging enabled", "path", cfg.Logging.FilePath)
	}

	// Wire activity recording from S3 handler to activity log + access logger
	s3h.SetActivityFunc(func(method, bucket, key string, status int, size int64, clientIP string) {
		// Skip browser noise
		if bucket == "favicon.ico" {
			return
		}
		now := time.Now().UTC()
		activityLog.Record(api.ActivityEntry{
			Time:     now,
			Method:   method,
			Bucket:   bucket,
			Key:      key,
			Status:   status,
			Size:     size,
			ClientIP: clientIP,
		})
		if accessLogger != nil {
			accessLogger.Log(accesslog.AccessEntry{
				Time:     now,
				Method:   method,
				Bucket:   bucket,
				Key:      key,
				Status:   status,
				Bytes:    size,
				ClientIP: clientIP,
			})
		}
	})

	// Wire audit trail recording
	s3h.SetAuditFunc(func(principal, userID, action, resource, effect, sourceIP string, statusCode int) {
		store.PutAuditEntry(metadata.AuditEntry{
			Time:       time.Now().UnixNano(),
			Principal:  principal,
			UserID:     userID,
			Action:     action,
			Resource:   resource,
			Effect:     effect,
			SourceIP:   sourceIP,
			StatusCode: statusCode,
		})
	})

	// Initialize notification dispatcher
	nc := cfg.Notifications
	notifyDispatcher := notify.NewDispatcher(store, nc.MaxWorkers, nc.QueueSize, nc.TimeoutSecs, nc.MaxRetries)

	// Register notification backends
	if nc.Kafka.Enabled && len(nc.Kafka.Brokers) > 0 && nc.Kafka.Topic != "" {
		notifyDispatcher.AddBackend(notify.NewKafkaBackend(nc.Kafka.Brokers, nc.Kafka.Topic))
	}
	if nc.NATS.Enabled && nc.NATS.URL != "" && nc.NATS.Subject != "" {
		natsBackend, err := notify.NewNATSBackend(nc.NATS.URL, nc.NATS.Subject)
		if err != nil {
			slog.Warn("NATS backend failed to connect", "error", err)
		} else {
			notifyDispatcher.AddBackend(natsBackend)
		}
	}
	if nc.Redis.Enabled && nc.Redis.Addr != "" {
		notifyDispatcher.AddBackend(notify.NewRedisBackend(nc.Redis.Addr, nc.Redis.Channel, nc.Redis.ListKey))
	}
	if nc.AMQP.Enabled && nc.AMQP.URL != "" {
		notifyDispatcher.AddBackend(notify.NewAMQPBackend(nc.AMQP.URL, nc.AMQP.Exchange, nc.AMQP.RoutingKey))
	}
	if nc.Postgres.Enabled && nc.Postgres.ConnStr != "" {
		pgBackend, err := notify.NewPostgresBackend(nc.Postgres.ConnStr, nc.Postgres.Table)
		if err != nil {
			slog.Warn("PostgreSQL notification backend failed", "error", err)
		} else {
			notifyDispatcher.AddBackend(pgBackend)
		}
	}

	s3h.SetNotificationFunc(func(eventType, bucket, key string, size int64, etag, versionID string) {
		notifyDispatcher.Dispatch(bucket, key, eventType, size, etag, versionID)
	})

	// Initialize replication worker if enabled
	var replWorker *replication.Worker
	var biDirWorker *replication.BiDirectionalWorker
	if cfg.Replication.Enabled && len(cfg.Replication.Peers) > 0 {
		// Register peer access keys so replication header is only trusted from peers
		var peerKeys []string
		for _, peer := range cfg.Replication.Peers {
			peerKeys = append(peerKeys, peer.AccessKey)
		}
		s3h.SetReplicationPeerKeys(peerKeys)

		if cfg.Replication.Mode == "active-active" {
			// Active-active bidirectional replication
			biDirWorker = replication.NewBiDirectionalWorker(store, engine, cfg.Replication)
			changeLog := biDirWorker.ChangeLog()
			siteID := biDirWorker.SiteID()
			s3h.SetReplicationFunc(func(eventType, bucket, key string, size int64, etag, versionID string) {
				evtType := "put"
				if eventType == "s3:ObjectRemoved:Delete" {
					evtType = "delete"
				}
				vc := replication.NewVectorClock()
				vc.Increment(siteID)
				// Also store the vector clock on the object metadata
				if meta, err := store.GetObjectMeta(bucket, key); err == nil {
					existingVC, _ := replication.ParseVectorClock(meta.VectorClock)
					vc = existingVC.Merge(vc)
					vc.Increment(siteID)
					meta.VectorClock = vc.Bytes()
					store.PutObjectMeta(*meta)
				}
				changeLog.Record(bucket, key, evtType, etag, size, vc)
			})
			slog.Info("active-active replication enabled",
				"site_id", siteID,
				"peers", len(cfg.Replication.Peers),
				"conflict_strategy", cfg.Replication.ConflictStrategy,
			)
		} else {
			// Traditional push-based replication
			replWorker = replication.NewWorker(store, engine, cfg.Replication)
			s3h.SetReplicationFunc(func(eventType, bucket, key string, size int64, etag, versionID string) {
				evtType := "put"
				if eventType == "s3:ObjectRemoved:Delete" {
					evtType = "delete"
				}
				for _, peer := range cfg.Replication.Peers {
					store.EnqueueReplication(metadata.ReplicationEvent{
						Type:   evtType,
						Bucket: bucket,
						Key:    key,
						ETag:   etag,
						Peer:   peer.Name,
						Size:   size,
					})
				}
			})
			slog.Info("push replication enabled", "peers", len(cfg.Replication.Peers), "interval_secs", cfg.Replication.ScanIntervalSecs)
		}
	}

	// Build search index
	searchIdx := search.NewIndex(store, cfg.Memory.MaxSearchEntries)
	if err := searchIdx.Build(); err != nil {
		slog.Warn("search index build failed", "error", err)
	}
	s3h.SetSearchUpdateFunc(func(eventType, bucket, key string) {
		if eventType == "delete" {
			searchIdx.Remove(bucket, key)
		} else {
			if meta, err := store.GetObjectMeta(bucket, key); err == nil {
				searchIdx.Update(bucket, key, *meta)
			}
		}
	})

	// Initialize scanner if enabled
	var scanWorker *scanner.Scanner
	if cfg.Scanner.Enabled && cfg.Scanner.WebhookURL != "" {
		scanWorker = scanner.NewScanner(store, engine,
			cfg.Scanner.WebhookURL, cfg.Scanner.Workers,
			cfg.Scanner.TimeoutSecs, cfg.Scanner.QuarantineBucket,
			cfg.Scanner.FailClosed, cfg.Scanner.MaxScanSizeBytes, 256)
		s3h.SetScanFunc(func(bucket, key string, size int64) {
			scanWorker.Scan(bucket, key, size)
		})
	}

	// Initialize tiering if enabled
	var tieringMgr *tiering.Manager
	if cfg.Tiering.Enabled && cfg.Tiering.ColdDataDir != "" {
		coldFS, err := storage.NewFileSystem(cfg.Tiering.ColdDataDir)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("init cold storage: %w", err)
		}
		tieringMgr = tiering.NewManager(store, fs, coldFS, cfg.Tiering.MigrateAfterDays, cfg.Tiering.ScanIntervalSecs)
		slog.Info("tiering enabled", "cold_dir", cfg.Tiering.ColdDataDir, "migrate_after_days", cfg.Tiering.MigrateAfterDays)
	}

	// Initialize backup scheduler if enabled
	var backupSched *backup.Scheduler
	if cfg.Backup.Enabled && len(cfg.Backup.Targets) > 0 {
		backupSched = backup.NewScheduler(store, engine, cfg.Backup)
		slog.Info("backup enabled", "targets", len(cfg.Backup.Targets), "schedule", cfg.Backup.ScheduleCron)
	}

	// Initialize rate limiter if enabled
	var rateLimiter *ratelimit.Limiter
	if cfg.RateLimit.Enabled {
		rateLimiter = ratelimit.NewLimiter(
			cfg.RateLimit.RequestsPerSec, cfg.RateLimit.BurstSize,
			cfg.RateLimit.PerKeyRPS, cfg.RateLimit.PerKeyBurst,
		)
		s3h.SetRateLimiter(rateLimiter)
		slog.Info("rate limiting enabled",
			"ip_rps", cfg.RateLimit.RequestsPerSec, "ip_burst", cfg.RateLimit.BurstSize,
			"key_rps", cfg.RateLimit.PerKeyRPS, "key_burst", cfg.RateLimit.PerKeyBurst)
	}

	// Initialize lambda trigger manager if enabled
	var lambdaMgr *lambda.TriggerManager
	if cfg.Lambda.Enabled {
		lambdaMgr = lambda.NewTriggerManager(store, engine, cfg.Lambda)
		s3h.SetLambdaFunc(func(eventType, bucket, key string, size int64, etag, versionID string) {
			lambdaMgr.Dispatch(bucket, key, eventType, size, etag, versionID)
		})
		slog.Info("lambda triggers enabled", "workers", cfg.Lambda.MaxWorkers, "queue_size", cfg.Lambda.QueueSize)
	}

	// Initialize batched access updater
	accessUpdater := metadata.NewAccessUpdater(store, 30*time.Second)
	s3h.SetAccessUpdater(accessUpdater)

	// Initialize built-in IAM policies
	initBuiltinPolicies(store)

	return &Server{
		cfg:             cfg,
		store:           store,
		engine:          engine,
		s3h:             s3h,
		metrics:         mc,
		activity:        activityLog,
		accessLog:       accessLogger,
		notifyDisp:      notifyDispatcher,
		replWorker:      replWorker,
		biDirWorker:     biDirWorker,
		searchIndex:     searchIdx,
		scanWorker:      scanWorker,
		tieringMgr:      tieringMgr,
		backupSched:     backupSched,
		rateLimiter:     rateLimiter,
		lambdaMgr:       lambdaMgr,
		accessUpdater:   accessUpdater,
		clusterNode:     clusterNode,
		clusterProxy:    clusterProxy,
		failoverProxy:   failoverProxy,
		failureDetector: failureDetector,
		rebalancer:      rebalancer,
		ecHealer:        ecHealer,
		s3Auth:          auth,
	}, nil
}

// Run starts the server and blocks until shutdown signal is received.
// It handles graceful shutdown with a configurable timeout.
func (s *Server) Run() error {
	addr := s.cfg.ListenAddr()

	// Dashboard API
	apiHandler := api.NewAPIHandler(s.store, s.engine, s.metrics, s.cfg, s.activity)
	apiHandler.SetS3Authenticator(s.s3Auth)
	apiHandler.SetSearchIndex(s.searchIndex)
	if s.scanWorker != nil {
		apiHandler.SetScanner(s.scanWorker)
	}
	if s.tieringMgr != nil {
		apiHandler.SetTieringManager(s.tieringMgr)
	}
	if s.backupSched != nil {
		apiHandler.SetBackupScheduler(s.backupSched)
	}
	if s.rateLimiter != nil {
		apiHandler.SetRateLimiter(s.rateLimiter)
	}

	// Wire OIDC validator if enabled
	if s.cfg.OIDC.Enabled && s.cfg.OIDC.IssuerURL != "" {
		if err := validateExternalURL(s.cfg.OIDC.IssuerURL); err != nil {
			slog.Warn("OIDC issuer URL rejected", "url", s.cfg.OIDC.IssuerURL, "error", err)
		} else {
			oidcValidator, err := api.NewOIDCValidator(
				s.cfg.OIDC.IssuerURL,
				s.cfg.OIDC.ClientID,
				s.cfg.OIDC.AllowedDomains,
				s.cfg.OIDC.JWKSCacheSecs,
			)
			if err != nil {
				slog.Warn("OIDC setup failed", "error", err)
			} else {
				apiHandler.SetOIDCValidator(oidcValidator)
				slog.Info("OIDC enabled", "issuer", s.cfg.OIDC.IssuerURL)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/favicon.svg", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/health", healthHandler(s.metrics.StartTime()))
	mux.HandleFunc("/ready", readyHandler(s.store))
	mux.Handle("/api/v1/", apiHandler)
	mux.Handle("/dashboard/", dashboard.Handler())
	mux.Handle("/metrics", s.metrics)

	// Register pprof endpoints when debug mode is enabled
	if s.cfg.Debug {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		slog.Info("pprof debug endpoints enabled at /debug/pprof/")
	}

	// Register cluster endpoints if enabled
	if s.clusterNode != nil {
		mux.HandleFunc("/cluster/status", s.clusterNode.StatusHandler())
		mux.HandleFunc("/cluster/join", s.clusterNode.JoinHandler())
		mux.HandleFunc("/cluster/leave", s.clusterNode.LeaveHandler())
		slog.Info("cluster endpoints registered", "paths", []string{"/cluster/status", "/cluster/join", "/cluster/leave"})
	}

	// Register bidirectional replication sync endpoint
	if s.biDirWorker != nil {
		mux.HandleFunc("/_replication/sync", s.biDirWorker.HandleSyncRequest)
		slog.Info("bidirectional replication sync endpoint registered", "path", "/_replication/sync")
	}

	mux.Handle("/", s.s3h)

	// Wrap mux with middleware: panic recovery (outermost) → security headers → request ID → latency → mux
	var handler http.Handler = mux
	handler = middleware.Latency(s.metrics, handler)
	handler = middleware.RequestID(handler)
	handler = middleware.SecurityHeaders(handler)
	handler = middleware.PanicRecovery(handler)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	// Log startup info
	scheme := "http"
	if s.cfg.Server.TLS.Enabled {
		scheme = "https"
	}
	slog.Info("VaultS3 starting",
		"addr", addr,
		"data_dir", s.cfg.Storage.DataDir,
		"metadata_dir", s.cfg.Storage.MetadataDir,
		"dashboard", fmt.Sprintf("%s://%s/dashboard/", scheme, addr),
	)
	if s.cfg.Auth.AdminAccessKey == "vaults3-admin" || s.cfg.Auth.AdminSecretKey == "vaults3-secret-change-me" {
		slog.Warn("Using default admin credentials. Set VAULTS3_ACCESS_KEY and VAULTS3_SECRET_KEY environment variables.")
	}
	if s.cfg.Encryption.Enabled {
		slog.Info("encryption enabled", "algorithm", "AES-256-GCM")
	}
	if s.cfg.Server.Domain != "" {
		slog.Info("virtual-hosted URLs enabled", "domain", s.cfg.Server.Domain)
	}
	if s.cfg.Server.TLS.Enabled {
		slog.Info("TLS enabled", "cert", s.cfg.Server.TLS.CertFile, "key", s.cfg.Server.TLS.KeyFile)
	}

	// Apply Go memory limit if configured
	if s.cfg.Memory.GoMemLimitMB > 0 {
		limit := int64(s.cfg.Memory.GoMemLimitMB) * 1024 * 1024
		debug.SetMemoryLimit(limit)
		slog.Info("memory limit set", "mb", s.cfg.Memory.GoMemLimitMB)
	}

	// Start batched access updater
	updaterCtx, updaterCancel := context.WithCancel(context.Background())
	defer updaterCancel()
	go s.accessUpdater.Run(updaterCtx)

	// Start lifecycle worker
	lcCtx, lcCancel := context.WithCancel(context.Background())
	defer lcCancel()
	lcWorker := lifecycle.NewWorker(s.store, s.engine, s.cfg.Lifecycle.ScanIntervalSecs, s.cfg.Security.AuditRetentionDays)
	go lcWorker.Run(lcCtx)
	slog.Info("lifecycle worker started", "interval_secs", s.cfg.Lifecycle.ScanIntervalSecs)

	// Start notification dispatcher
	notifyCtx, notifyCancel := context.WithCancel(context.Background())
	defer notifyCancel()
	s.notifyDisp.Start(notifyCtx)
	slog.Info("notifications started", "workers", s.cfg.Notifications.MaxWorkers, "queue_size", s.cfg.Notifications.QueueSize)

	// Start replication worker if enabled
	if s.replWorker != nil {
		replCtx, replCancel := context.WithCancel(context.Background())
		defer replCancel()
		go s.replWorker.Run(replCtx)
	}

	// Start bidirectional replication if enabled
	if s.biDirWorker != nil {
		biDirCtx, biDirCancel := context.WithCancel(context.Background())
		defer biDirCancel()
		go s.biDirWorker.Run(biDirCtx)
	}

	// Start scanner workers if enabled
	if s.scanWorker != nil {
		scanCtx, scanCancel := context.WithCancel(context.Background())
		defer scanCancel()
		s.scanWorker.Start(scanCtx, s.cfg.Scanner.Workers)
	}

	// Start tiering manager if enabled
	if s.tieringMgr != nil {
		tierCtx, tierCancel := context.WithCancel(context.Background())
		defer tierCancel()
		go s.tieringMgr.Run(tierCtx)
	}

	// Start lambda trigger manager if enabled
	if s.lambdaMgr != nil {
		lambdaCtx, lambdaCancel := context.WithCancel(context.Background())
		defer lambdaCancel()
		s.lambdaMgr.Start(lambdaCtx)
		apiHandler.SetLambdaManager(s.lambdaMgr)
	}

	// Start failure detector if cluster is enabled
	if s.failureDetector != nil {
		detCtx, detCancel := context.WithCancel(context.Background())
		defer detCancel()
		go s.failureDetector.Run(detCtx)
	}

	// Start erasure healer if enabled
	if s.ecHealer != nil {
		ecCtx, ecCancel := context.WithCancel(context.Background())
		defer ecCancel()
		go s.ecHealer.Run(ecCtx)
	}

	// Start backup scheduler if enabled
	if s.backupSched != nil {
		backupCtx, backupCancel := context.WithCancel(context.Background())
		defer backupCancel()
		go s.backupSched.Run(backupCtx)
	}

	slog.Info("search index ready", "objects", s.searchIndex.Count())

	// Start separate inter-node listener if configured
	var interNodeServer *http.Server
	if s.cfg.Server.InterNodePort > 0 && s.clusterNode != nil {
		interNodeAddr := fmt.Sprintf("%s:%d", s.cfg.Server.InterNodeAddress, s.cfg.Server.InterNodePort)
		interNodeMux := http.NewServeMux()
		interNodeMux.HandleFunc("/cluster/status", s.clusterNode.StatusHandler())
		interNodeMux.HandleFunc("/cluster/join", s.clusterNode.JoinHandler())
		interNodeMux.HandleFunc("/cluster/leave", s.clusterNode.LeaveHandler())
		if s.biDirWorker != nil {
			interNodeMux.HandleFunc("/_replication/sync", s.biDirWorker.HandleSyncRequest)
		}
		interNodeServer = &http.Server{
			Addr:    interNodeAddr,
			Handler: interNodeMux,
		}
		go func() {
			slog.Info("inter-node listener started", "addr", interNodeAddr)
			if err := interNodeServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("inter-node listener error", "error", err)
			}
		}()
	}

	// Start server in goroutine
	errCh := make(chan error, 1)
	go func() {
		if s.cfg.Server.TLS.Enabled {
			errCh <- httpServer.ListenAndServeTLS(s.cfg.Server.TLS.CertFile, s.cfg.Server.TLS.KeyFile)
		} else {
			errCh <- httpServer.ListenAndServe()
		}
	}()

	// Wait for signal or server error
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig)
	}

	// Graceful shutdown
	timeout := time.Duration(s.cfg.Server.ShutdownTimeoutSecs) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if interNodeServer != nil {
		interNodeServer.Shutdown(ctx)
	}
	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown timed out", "timeout", timeout, "error", err)
		return err
	}

	slog.Info("server stopped gracefully")
	return nil
}

func initBuiltinPolicies(store *metadata.Store) {
	builtins := []metadata.IAMPolicy{
		{
			Name:     "ReadOnlyAccess",
			Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket","s3:ListAllMyBuckets","s3:GetBucketPolicy"],"Resource":["*"]}]}`,
		},
		{
			Name:     "ReadWriteAccess",
			Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject","s3:ListBucket","s3:ListAllMyBuckets"],"Resource":["*"]}]}`,
		},
		{
			Name:     "FullAccess",
			Document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["*"]}]}`,
		},
	}

	for _, p := range builtins {
		p.CreatedAt = time.Now().UTC()
		// Use CreateIAMPolicy which is a no-op if already exists
		store.CreateIAMPolicy(p)
	}
}

// validateExternalURL checks that a URL does not point to internal/metadata endpoints (SSRF prevention).
func validateExternalURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL must have a host")
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" {
		return fmt.Errorf("URL must not point to localhost")
	}
	if strings.HasPrefix(host, "169.254.") || host == "metadata.google.internal" {
		return fmt.Errorf("URL must not point to cloud metadata service")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
			return fmt.Errorf("URL must not point to loopback, link-local, or private address")
		}
	}
	return nil
}

func (s *Server) Close() {
	if s.rebalancer != nil {
		s.rebalancer.Stop()
	}
	if s.clusterNode != nil {
		s.clusterNode.Shutdown()
	}
	if s.lambdaMgr != nil {
		s.lambdaMgr.Stop()
	}
	if s.rateLimiter != nil {
		s.rateLimiter.Stop()
	}
	if s.accessLog != nil {
		s.accessLog.Close()
	}
	if s.store != nil {
		s.store.Close()
	}
}
