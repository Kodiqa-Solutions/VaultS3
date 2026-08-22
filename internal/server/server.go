package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"net/http/pprof"
	"runtime/debug"

	"github.com/Kodiqa-Solutions/VaultS3/internal/accesslog"
	"github.com/Kodiqa-Solutions/VaultS3/internal/api"
	"github.com/Kodiqa-Solutions/VaultS3/internal/backup"
	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketcrypto"
	"github.com/Kodiqa-Solutions/VaultS3/internal/bucketkeys"
	"github.com/Kodiqa-Solutions/VaultS3/internal/cluster"
	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/dashboard"
	"github.com/Kodiqa-Solutions/VaultS3/internal/erasure"
	"github.com/Kodiqa-Solutions/VaultS3/internal/lambda"
	"github.com/Kodiqa-Solutions/VaultS3/internal/lifecycle"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metrics"
	"github.com/Kodiqa-Solutions/VaultS3/internal/middleware"
	"github.com/Kodiqa-Solutions/VaultS3/internal/migrate"
	"github.com/Kodiqa-Solutions/VaultS3/internal/notify"
	"github.com/Kodiqa-Solutions/VaultS3/internal/ratelimit"
	"github.com/Kodiqa-Solutions/VaultS3/internal/replication"
	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
	"github.com/Kodiqa-Solutions/VaultS3/internal/scanner"
	"github.com/Kodiqa-Solutions/VaultS3/internal/search"
	"github.com/Kodiqa-Solutions/VaultS3/internal/selfupdate"
	"github.com/Kodiqa-Solutions/VaultS3/internal/snapshot"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
	"github.com/Kodiqa-Solutions/VaultS3/internal/tiering"
	"github.com/Kodiqa-Solutions/VaultS3/internal/vector"
)

// Version is the running build version, set by main from the -ldflags value.
var Version = "dev"

// reapClient issues the best-effort inter-node object-delete broadcasts (issue
// #34 layer 2), and replClient streams object data to replica-set peers (issue
// #37, replica_count > 1). Both share the pooled inter-node transport so they
// reuse connections instead of opening one per call: at cluster write rates that
// churn is what starves a node of ephemeral ports and makes connects to it fail
// intermittently (issue #42).
//
// replClient takes no overall timeout, because that would cap the whole request
// including the body and so break large objects; its setup is bounded by the
// shared transport's dial timeout.
var reapClient = cluster.InterNodeClient(10 * time.Second)

var replClient = cluster.InterNodeClient(0)

// clusterControllerAdapter adapts *cluster.Node to api.ClusterController so the
// admin API can drive membership without importing internal/cluster's raft types.
type clusterControllerAdapter struct {
	n  *cluster.Node
	rt *cluster.ShardRuntime
}

func (a clusterControllerAdapter) SelfID() string             { return a.n.NodeID() }
func (a clusterControllerAdapter) IsLeader() bool             { return a.n.IsLeader() }
func (a clusterControllerAdapter) LeaderID() string           { return a.n.LeaderID() }
func (a clusterControllerAdapter) Join(id, addr string) error { return a.n.Join(id, addr) }
func (a clusterControllerAdapter) Leave(id string) error      { return a.n.Leave(id) }

// ShardMap reports the committed metadata shard assignment. A read error is
// reported as "no map" after logging: this endpoint is informational, and an
// unreadable map is not an assignment the caller should act on.
func (a clusterControllerAdapter) ShardMap() *api.ShardAssignment {
	m, err := a.n.CommittedShardMap()
	if err != nil {
		slog.Warn("cluster: cannot read the committed shard map", "error", err)
		return nil
	}
	if m == nil {
		return nil
	}
	return &api.ShardAssignment{
		Version:  m.Version,
		Epoch:    m.Epoch,
		Shards:   m.Shards,
		Replicas: m.Replicas,
		Members:  m.Members,
		Founders: m.Founders,
	}
}

// LocalShards reports the metadata shard groups this node is running, which is
// how an operator watches reconciliation: a group whose Raft members differ from
// the committed assignment is one the reconciler is still working on.
func (a clusterControllerAdapter) LocalShards() []api.LocalShard {
	if a.rt == nil {
		return nil
	}
	shards := a.rt.Shards()
	sort.Ints(shards)
	out := make([]api.LocalShard, 0, len(shards))
	for _, shard := range shards {
		g, err := a.rt.Group(shard)
		if err != nil {
			continue
		}
		ls := api.LocalShard{Shard: shard, IsLeader: g.IsLeader(), LeaderID: g.LeaderID()}
		if servers, err := g.Members(); err == nil {
			for _, srv := range servers {
				ls.Members = append(ls.Members, string(srv.ID))
			}
		}
		out = append(out, ls)
	}
	return out
}

func (a clusterControllerAdapter) Members() []api.ClusterMember {
	leaderID := a.n.LeaderID()
	members := a.n.MembersInfo()
	out := make([]api.ClusterMember, 0, len(members))
	for _, m := range members {
		out = append(out, api.ClusterMember{
			NodeID:   m.ID,
			Address:  m.Address,
			Suffrage: m.Suffrage,
			Leader:   m.ID == leaderID,
		})
	}
	return out
}

type Server struct {
	cfg             *config.Config
	store           *metadata.Store
	metaStore       metadata.StoreAPI
	engine          storage.Engine
	keyMgr          *bucketcrypto.Manager
	s3h             *s3.Handler
	metrics         *metrics.Collector
	activity        *api.ActivityLog
	accessLog       *accesslog.AccessLogger
	notifyDisp      *notify.Dispatcher
	replWorker      *replication.Worker
	biDirWorker     *replication.BiDirectionalWorker
	replicationFunc func(eventType, bucket, key string, size int64, etag, versionID string)
	searchIndex     *search.Index
	vectorMgr       *vector.Manager
	scanWorker      *scanner.Scanner
	tieringMgr      *tiering.Manager
	backupSched     *backup.Scheduler
	rateLimiter     *ratelimit.Limiter
	lambdaMgr       *lambda.TriggerManager
	accessUpdater   *metadata.AccessUpdater
	clusterNode     *cluster.Node
	clusterProxy    *cluster.Proxy
	shardService    *cluster.ShardService
	shardRuntime    *cluster.ShardRuntime
	shardRouter     *cluster.ShardRouter
	failoverProxy   *cluster.FailoverProxy
	failureDetector *cluster.FailureDetector
	rebalancer      *cluster.Rebalancer
	ecHealer        *erasure.Healer
	s3Auth          *s3.Authenticator
	writable        *atomic.Bool // node-local write gate shared by the S3 + admin handlers (drain)
	// reapElsewhere drops an object's data on the OTHER nodes; nil single-node.
	// Held here so background sweeps (lifecycle expiry) reclaim cluster-wide too,
	// not just the request-path deletes (issue #47).
	reapElsewhere func(bucket, key, versionID string)
}

func New(cfg *config.Config) (*Server, error) {
	// Initialize storage engine
	fs, err := storage.NewFileSystem(cfg.Storage.DataDir)
	if err != nil {
		return nil, fmt.Errorf("init storage: %w", err)
	}

	var engine storage.Engine = fs
	var perBucketEngine *storage.PerBucketEngine

	// Compression is wrapped first, which makes encryption the OUTER engine: a
	// write is encrypted and then handed to the compressor. That is the reverse
	// of what this comment used to claim, and it means compression saves nothing
	// when encryption is on, since ciphertext does not compress (measured 1.00x
	// on a highly repetitive payload). Swapping the order would fix that but
	// changes the on-disk layering, so existing objects would need format
	// detection in both directions; left as its own change.
	if cfg.Compression.Enabled {
		engine = storage.NewCompressedEngine(engine)
		// Writes are zstd; gzip is still decoded on read for objects written by
		// older versions. The log said "gzip" long after that stopped being true.
		slog.Info("compression enabled", "algorithm", "zstd", "reads", "zstd+gzip")
	}

	// Wrap with encryption if enabled (SSE-S3 or SSE-KMS)
	if cfg.Encryption.Enabled {
		if cfg.Encryption.PerBucket {
			// Per-bucket encryption: the configured key is the master KEK; objects are
			// encrypted with a per-bucket data key (provisioned on opt-in). The manager
			// is wired after the metadata store is ready.
			if _, err := cfg.Encryption.KeyBytes(); err != nil {
				return nil, fmt.Errorf("per-bucket encryption needs a valid master key: %w", err)
			}
			legacy, err := cfg.Encryption.LegacyKeyBytes()
			if err != nil {
				return nil, fmt.Errorf("encryption config: %w", err)
			}
			pe, err := storage.NewPerBucketEngine(engine, legacy)
			if err != nil {
				return nil, fmt.Errorf("init per-bucket encryption: %w", err)
			}
			engine = pe
			perBucketEngine = pe
			slog.Info("per-bucket encryption enabled (per-bucket keys, opt-in via PUT ?encryption)")
		} else if cfg.Encryption.KMS.Enabled {
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

	// Wrap with small-file packing if enabled (experimental). Small objects are
	// packed as zstd frames into large volume files; large objects fall through to
	// the layers below. Packed frames bypass the encryption/erasure layers, so for
	// now packing is mutually exclusive with them.
	if cfg.Packing.Enabled {
		if cfg.Encryption.Enabled || cfg.Erasure.Enabled {
			slog.Warn("small-file packing disabled: it does not yet compose with encryption or erasure coding")
		} else {
			pe, err := storage.NewPackedEngine(engine, cfg.Packing.MaxObjectSize, cfg.Packing.VolumeMaxSize)
			if err != nil {
				return nil, fmt.Errorf("init packing: %w", err)
			}
			engine = pe
			slog.Info("small-file packing enabled",
				"max_object_size", cfg.Packing.MaxObjectSize,
				"volume_max_size", cfg.Packing.VolumeMaxSize,
			)
			if cfg.Packing.CompactIntervalHours > 0 {
				ratio := cfg.Packing.CompactMinDeadRatio
				if ratio <= 0 {
					ratio = 0.5
				}
				interval := time.Duration(cfg.Packing.CompactIntervalHours) * time.Hour
				go func() {
					t := time.NewTicker(interval)
					defer t.Stop()
					for range t.C {
						if n, err := pe.Compact(ratio); err != nil {
							slog.Error("pack compaction failed", "error", err)
						} else if n > 0 {
							slog.Info("pack compaction reclaimed space", "bytes", n)
						}
					}
				}()
			}
		}
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

	// Initialize cluster if enabled
	var clusterNode *cluster.Node
	var clusterProxy *cluster.Proxy
	var shardService *cluster.ShardService
	var shardRuntime *cluster.ShardRuntime
	var shardRouter *cluster.ShardRouter
	if cfg.Cluster.Enabled {
		// Normalise here rather than letting each component default its own copy,
		// so the shard service and the Raft node agree on the replica count.
		cluster.ApplyDefaults(&cfg.Cluster)
		// Sharding splits object metadata across independent Raft groups, so the
		// shard count fixes which bucket lives in which group and cannot change
		// afterwards. A count larger than a node could run is refused rather than
		// clamped: a silent adjustment would leave an operator believing their
		// metadata is spread differently than it is (issue #50).
		if cfg.Cluster.MetadataShards > cluster.MaxMetadataShards {
			store.Close()
			return nil, fmt.Errorf("cluster.metadata_shards is %d, which is more than the %d this build supports",
				cfg.Cluster.MetadataShards, cluster.MaxMetadataShards)
		}
		// Turning sharding on for a cluster that already holds object metadata
		// would leave every existing record in the control group, which nothing
		// reads once objects are routed to shards: the data would still be on
		// disk and every object would 404. There is no in-place migration, so
		// this is refused rather than attempted.
		if cfg.Cluster.MetadataShards > 1 {
			hasObjects, err := store.HasObjectMetadata()
			if err != nil {
				store.Close()
				return nil, fmt.Errorf("check existing object metadata: %w", err)
			}
			if hasObjects {
				store.Close()
				return nil, fmt.Errorf("cluster.metadata_shards is %d, but this node's metadata store already holds object metadata "+
					"that was written unsharded; there is no in-place migration, so either set metadata_shards back to 1 "+
					"or start a new sharded cluster and copy the objects across (see docs/design/sharded-metadata.md)",
					cfg.Cluster.MetadataShards)
			}
		}
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
		// Metadata sharding (issue #50). The shard groups share the control
		// group's Raft port through the transport mux, resolve peer addresses
		// through the control group's live membership, and are supervised by one
		// loop that creates the assignment, keeps this node's groups in step with
		// it, and reconciles each shard's own Raft configuration.
		if cfg.Cluster.MetadataShards > 1 {
			mux := node.TransportMux()
			if mux == nil {
				node.Shutdown()
				store.Close()
				return nil, fmt.Errorf("cluster: metadata sharding needs the shared Raft transport, which this node did not start")
			}
			shardRuntime = cluster.NewShardRuntime(cfg.Cluster.NodeID, cfg.Cluster.DataDir, mux, node.ShardAddressProvider())
			proxy := clusterProxy
			shardRouter = cluster.NewShardRouter(cfg.Cluster.NodeID, shardRuntime, func(nodeID string) (string, bool) {
				addr, ok := proxy.NodeAddrs()[nodeID]
				return addr, ok && addr != ""
			}, cfg.Cluster.Secret)
			shardService = cluster.NewShardService(cfg.Cluster.NodeID, node, node.CommittedShardMap,
				ring, shardRuntime, shardRouter, node.ShardAddressProvider(),
				cfg.Cluster.MetadataShards, cfg.Cluster.MetadataReplicas)
			slog.Info("cluster: metadata sharding enabled",
				"shards", cfg.Cluster.MetadataShards, "replicas", cfg.Cluster.MetadataReplicas)
		}
		// Pin peers to their configured API addresses so the Raft-derived ones,
		// which assume every node shares this node's API port, cannot replace them.
		clusterProxy.SetPeerAPIs(cfg.Cluster.PeerAPIs)
		slog.Info("cluster mode enabled",
			"node_id", cfg.Cluster.NodeID,
			"ring_nodes", ring.NodeCount(),
			"replica_count", cfg.Cluster.Placement.ReplicaCount,
		)
	}

	// When clustered, route metadata WRITES through Raft consensus so every node
	// converges; reads stay local. Single-node uses the store directly. Handlers
	// depend on the metadata.StoreAPI interface, which both satisfy.
	var metaStore metadata.StoreAPI = store
	if clusterNode != nil {
		distributed := metadata.NewDistributedStore(store, clusterNode)
		metaStore = distributed
		slog.Info("cluster: metadata writes routed through Raft consensus")
		if shardRouter != nil {
			// Object metadata now lives in the shard that owns its bucket, and is
			// reached with one store-level hop when this node holds no copy of
			// that shard. Everything else stays in the control group (issue #50).
			metaStore = metadata.NewShardedStore(distributed, shardRouter)
		}
	}

	// Initialize erasure healer if EC is enabled
	if ecEngine != nil {
		healInterval := cfg.Erasure.HealInterval
		if healInterval <= 0 {
			healInterval = 3600
		}
		ecHealer = erasure.NewHealer(metaStore, ecEngine, healInterval)

		// Let a bucket opt out of erasure coding, so data that is cheap to
		// recreate can be stored once instead of carrying parity (issue #39).
		// The global setting remains the default for buckets that say nothing.
		ecEngine.SetBucketPolicy(func(bucket string) bool {
			return metaStore.BucketDurability(bucket, true, 0).ErasureEnabled
		})
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
		rebalancer = cluster.NewRebalancer(metaStore, engine, clusterProxy.Ring(), clusterProxy, cfg.Cluster.NodeID, cfg.Cluster.Rebalance)
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
	// Behind a reverse-proxy subpath, the client signs the URI with the prefix that
	// the proxy strips before we see it, so SigV4 verification must add it back to
	// match (issue #36). Reuses server.base_path.
	auth.SetBasePath(cfg.Server.BasePath, cfg.Server.TrustForwardedPrefix)

	// Load persisted admin credentials (overrides config/env if previously changed via dashboard)
	if ak, sk, err := store.GetAdminCredentials(); err == nil && ak != "" && sk != "" {
		cfg.Auth.AdminAccessKey = ak
		cfg.Auth.AdminSecretKey = sk
		auth.UpdateAdminCredentials(ak, sk)
		slog.Info("loaded persisted admin credentials")
	}

	// Initialize metrics collector
	mc := metrics.NewCollector(metaStore, engine)

	// Initialize activity log
	activityLog := api.NewActivityLog()

	// Initialize S3 handler
	s3h := s3.NewHandler(metaStore, engine, auth, cfg.Encryption.Enabled, cfg.Server.Domain, mc)

	// Node write gate (drain): shared by the S3 handler (rejects object writes when
	// draining) and the admin API (toggles it). Starts writable.
	writable := &atomic.Bool{}
	writable.Store(true)
	s3h.SetWritableFlag(writable)
	// Defaults a bucket inherits when it sets no durability override (issue #39).
	s3h.SetDurabilityDefaults(cfg.Erasure.Enabled, cfg.Cluster.Placement.ReplicaCount)

	// Keep in-progress multipart upload metadata on the node-local store, not Raft.
	// All requests for an object route to the same owner node and its parts live on
	// that node's local disk, so replicating the metadata only added a
	// read-after-write lag that 404'd concurrent part uploads (issue #32). `store`
	// is the raw local store even when metaStore is the distributed one.
	s3h.SetLocalMultipartStore(store)

	// Cluster delete-reaper: after a delete, remove the object's data file from
	// every other node so an orphan copy left by a past ring/primary change doesn't
	// linger on disk (issue #34 layer 2). Best-effort + async — correctness already
	// comes from metadata being authoritative, this only reclaims disk. Every path
	// that removes object data must go through it, request-path or background sweep,
	// or the copies on the other nodes are stranded with no way to reach them
	// (issue #47).
	var reapOne func(bucket, key, versionID string)
	if clusterProxy != nil {
		reapSecret := cfg.Cluster.Secret
		reapSelf := cfg.Cluster.NodeID
		reapScheme := "http"
		if cfg.Server.TLS.Enabled {
			reapScheme = "https"
		}
		reapPost := func(u string, body []byte) {
			go func() {
				var rdr io.Reader
				if body != nil {
					rdr = bytes.NewReader(body)
				}
				req, err := http.NewRequest(http.MethodPost, u, rdr)
				if err != nil {
					return
				}
				if body != nil {
					req.Header.Set("Content-Type", "application/json")
				}
				if reapSecret != "" {
					req.Header.Set("X-Cluster-Secret", reapSecret)
				}
				if resp, err := reapClient.Do(req); err == nil {
					resp.Body.Close()
				}
			}()
		}
		reapOne = func(bucket, key, versionID string) {
			for id, addr := range clusterProxy.NodeAddrs() {
				if id == reapSelf || addr == "" {
					continue
				}
				u := reapScheme + "://" + addr + "/cluster/object-delete?bucket=" +
					url.QueryEscape(bucket) + "&key=" + url.QueryEscape(key)
				if versionID != "" {
					u += "&version=" + url.QueryEscape(versionID)
				}
				reapPost(u, nil)
			}
		}
		s3h.SetReplicaReaper(reapOne)
		// Multipart state is node-local (issue #32), so no single node knows about
		// every upload. Without these two hooks a bucket-level listing showed only
		// the ~1/N of uploads whose key hashed to the listing node, and an upload
		// stranded on its creating node by a ring change answered NoSuchUpload to
		// every abort forever (issue #47 bug B).
		s3h.SetMultipartPeerLister(func(bucket string) []metadata.MultipartUpload {
			return collectPeerUploads(clusterProxy.NodeAddrs(), reapSelf, reapScheme, reapSecret, bucket)
		})
		s3h.SetMultipartHolderFallback(func(w http.ResponseWriter, r *http.Request, uploadID string) bool {
			holder := findUploadHolder(clusterProxy.NodeAddrs(), reapSelf, reapScheme, reapSecret, uploadID)
			if holder == "" {
				return false // genuinely nowhere; the caller writes NoSuchUpload
			}
			clusterProxy.ForwardRequest(w, r, holder)
			return true
		})

		// One request per peer for the whole key list, not one per peer per key: a
		// Spark-style job deletes a thousand keys at a time (issue #47).
		s3h.SetReplicaReaperBatch(func(bucket string, keys []string) {
			body, err := json.Marshal(map[string]any{"bucket": bucket, "keys": keys})
			if err != nil {
				return
			}
			for id, addr := range clusterProxy.NodeAddrs() {
				if id == reapSelf || addr == "" {
					continue
				}
				reapPost(reapScheme+"://"+addr+"/cluster/object-delete-batch", body)
			}
		})

		// How many nodes hold each object. A bucket may ask for fewer copies than
		// the cluster default (scratch data) or more, so this is resolved per write
		// rather than fixed at startup, and stays installed even when the default is
		// 1 because a bucket can raise its own count above it (issue #39).
		defaultReplicas := cfg.Cluster.Placement.ReplicaCount
		replicasFor := func(bucket string) int {
			return metaStore.BucketDurability(bucket, cfg.Erasure.Enabled, defaultReplicas).ReplicaCount
		}
		if failoverProxy != nil {
			failoverProxy.SetReplicaPolicy(replicasFor)
		}

		// After a write, stream the object's data to the other nodes in its replica
		// set so a node loss doesn't make it unavailable (issue #37). Best-effort +
		// async — never blocks/fails the client write; GET failover already tries
		// replicas. Each peer is streamed from the engine (no whole-object buffering).
		{
			repSecret := cfg.Cluster.Secret
			repSelf := cfg.Cluster.NodeID
			repScheme := reapScheme
			localEngine := engine
			ring := clusterProxy.Ring()
			s3h.SetPlacementReplicator(func(bucket, key string) {
				repCount := replicasFor(bucket)
				if repCount <= 1 {
					return // this bucket keeps a single copy
				}
				addrs := clusterProxy.NodeAddrs()
				for _, id := range ring.GetNodes(bucket, key, repCount) {
					if id == repSelf {
						continue
					}
					addr := addrs[id]
					if addr == "" {
						continue
					}
					go func(addr string) {
						reader, size, err := localEngine.GetObject(bucket, key)
						if err != nil {
							return
						}
						defer reader.Close()
						u := repScheme + "://" + addr + "/cluster/replica-put?bucket=" +
							url.QueryEscape(bucket) + "&key=" + url.QueryEscape(key)
						req, err := http.NewRequest(http.MethodPost, u, reader)
						if err != nil {
							return
						}
						req.ContentLength = size
						if repSecret != "" {
							req.Header.Set("X-Cluster-Secret", repSecret)
						}
						if resp, err := replClient.Do(req); err == nil {
							resp.Body.Close()
						}
					}(addr)
				}
			})
		}
	}

	// Per-bucket encryption keys: when a master key is configured, opting a bucket
	// into SSE-S3 provisions a per-bucket data key (see
	// docs/design/per-bucket-encryption.md). Reuses the encryption master key as KEK.
	var keyMgr *bucketcrypto.Manager
	if mk, err := cfg.Encryption.KeyBytes(); err == nil && len(mk) == 32 {
		if km, kerr := bucketkeys.NewManager(metaStore, mk); kerr == nil {
			keyMgr = km
			s3h.SetKeyManager(keyMgr)
			if perBucketEngine != nil {
				perBucketEngine.SetManager(keyMgr) // activate per-bucket crypto in the data path
			}
			slog.Info("per-bucket encryption key management enabled")
		}
	}

	// Wire cluster proxy into S3 handler (use failover proxy if available)
	if failoverProxy != nil {
		s3h.SetClusterProxy(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
			// A bucket listing must be answered by a node that has every committed
			// write applied, or a list right after a PUT misses the just-written key
			// on a lagging follower (issue #37: `mc stat` lists before it HEADs, so
			// this surfaced as a phantom read-after-write miss). Route listings to the
			// leader; object GET/HEAD keep owner routing + the per-key barrier.
			if requiresLeaderRead(r, bucket, key) {
				return failoverProxy.ForwardReadToLeader(w, r)
			}
			return failoverProxy.ForwardWithRetry(w, r, bucket, key)
		})
		// Last resort for a read whose metadata is here but whose data has not been
		// replicated to this node yet (issue #42).
		s3h.SetDataHolderFallback(failoverProxy.ForwardToDataHolder)
	} else if clusterProxy != nil {
		s3h.SetClusterProxy(func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
			if requiresLeaderRead(r, bucket, key) {
				return clusterProxy.ForwardReadToLeader(w, r)
			}
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
	notifyDispatcher := notify.NewDispatcher(metaStore, nc.MaxWorkers, nc.QueueSize, nc.TimeoutSecs, nc.MaxRetries)

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
	// Shared so both the S3 handler and the dashboard API handler enqueue
	// replication events — dashboard uploads/deletes must replicate too (issue #10).
	var replicationFunc func(eventType, bucket, key string, size int64, etag, versionID string)
	if cfg.Replication.Enabled && len(cfg.Replication.Peers) > 0 {
		// Register peer access keys so replication header is only trusted from peers
		var peerKeys []string
		for _, peer := range cfg.Replication.Peers {
			peerKeys = append(peerKeys, peer.AccessKey)
		}
		s3h.SetReplicationPeerKeys(peerKeys)

		if cfg.Replication.Mode == "active-active" {
			// Active-active bidirectional replication
			biDirWorker = replication.NewBiDirectionalWorker(metaStore, engine, cfg.Replication)
			changeLog := biDirWorker.ChangeLog()
			siteID := biDirWorker.SiteID()
			replicationFunc = func(eventType, bucket, key string, size int64, etag, versionID string) {
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
			}
			slog.Info("active-active replication enabled",
				"site_id", siteID,
				"peers", len(cfg.Replication.Peers),
				"conflict_strategy", cfg.Replication.ConflictStrategy,
			)
		} else {
			// Traditional push-based replication
			replWorker = replication.NewWorker(metaStore, engine, cfg.Replication)
			replicationFunc = func(eventType, bucket, key string, size int64, etag, versionID string) {
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
			}
			slog.Info("push replication enabled", "peers", len(cfg.Replication.Peers), "interval_secs", cfg.Replication.ScanIntervalSecs)
		}
		s3h.SetReplicationFunc(replicationFunc)
	}

	// Build search index
	searchIdx := search.NewIndex(metaStore, cfg.Memory.MaxSearchEntries)
	if err := searchIdx.Build(); err != nil {
		slog.Warn("search index build failed", "error", err)
	}

	// Optional vector / semantic-search add-on
	var vectorMgr *vector.Manager
	if cfg.Vector.Enabled && cfg.Vector.EmbeddingURL != "" {
		emb := vector.NewOpenAICompatEmbedder(cfg.Vector.EmbeddingURL, cfg.Vector.APIKey, cfg.Vector.Model, cfg.Vector.TimeoutSecs)
		vectorMgr = vector.NewManager(emb, vector.NewIndex(cfg.Vector.Dimensions, cfg.Vector.MaxVectors), cfg.Vector.PersistPath)
		slog.Info("vector search enabled", "model", cfg.Vector.Model, "auto_index", cfg.Vector.AutoIndex, "vectors", vectorMgr.Count())
	}

	s3h.SetSearchUpdateFunc(func(eventType, bucket, key string) {
		if eventType == "delete" {
			searchIdx.Remove(bucket, key)
			if vectorMgr != nil {
				vectorMgr.Remove(bucket, key)
			}
			return
		}
		meta, err := store.GetObjectMeta(bucket, key)
		if err != nil {
			return
		}
		searchIdx.Update(bucket, key, *meta)
		// Auto-index for vector search runs off the request path (embedding is a
		// network call) and is strictly best-effort.
		if vectorMgr != nil && cfg.Vector.AutoIndex && shouldVectorIndex(cfg.Vector, key, meta) {
			go indexObjectVector(vectorMgr, engine, bucket, key, cfg.Vector)
		}
	})

	// Initialize scanner if enabled
	var scanWorker *scanner.Scanner
	if cfg.Scanner.Enabled && cfg.Scanner.WebhookURL != "" {
		scanWorker = scanner.NewScanner(metaStore, engine,
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
		tieringMgr = tiering.NewManager(metaStore, fs, coldFS, cfg.Tiering.MigrateAfterDays, cfg.Tiering.ScanIntervalSecs)
		slog.Info("tiering enabled", "cold_dir", cfg.Tiering.ColdDataDir, "migrate_after_days", cfg.Tiering.MigrateAfterDays)
	}

	// Initialize backup scheduler if enabled
	var backupSched *backup.Scheduler
	if cfg.Backup.Enabled && len(cfg.Backup.Targets) > 0 {
		backupSched = backup.NewScheduler(metaStore, engine, cfg.Backup)
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
		lambdaMgr = lambda.NewTriggerManager(metaStore, engine, cfg.Lambda)
		s3h.SetLambdaFunc(func(eventType, bucket, key string, size int64, etag, versionID string) {
			lambdaMgr.Dispatch(bucket, key, eventType, size, etag, versionID)
		})
		slog.Info("lambda triggers enabled", "workers", cfg.Lambda.MaxWorkers, "queue_size", cfg.Lambda.QueueSize)
	}

	// Initialize batched access updater
	accessUpdater := metadata.NewAccessUpdater(metaStore, 30*time.Second)
	s3h.SetAccessUpdater(accessUpdater)

	// Initialize built-in IAM policies
	initBuiltinPolicies(store)

	return &Server{
		cfg:             cfg,
		store:           store,
		metaStore:       metaStore,
		engine:          engine,
		reapElsewhere:   reapOne,
		keyMgr:          keyMgr,
		s3h:             s3h,
		metrics:         mc,
		activity:        activityLog,
		accessLog:       accessLogger,
		notifyDisp:      notifyDispatcher,
		replWorker:      replWorker,
		biDirWorker:     biDirWorker,
		replicationFunc: replicationFunc,
		searchIndex:     searchIdx,
		vectorMgr:       vectorMgr,
		scanWorker:      scanWorker,
		tieringMgr:      tieringMgr,
		backupSched:     backupSched,
		rateLimiter:     rateLimiter,
		lambdaMgr:       lambdaMgr,
		accessUpdater:   accessUpdater,
		clusterNode:     clusterNode,
		clusterProxy:    clusterProxy,
		shardService:    shardService,
		shardRuntime:    shardRuntime,
		shardRouter:     shardRouter,
		failoverProxy:   failoverProxy,
		failureDetector: failureDetector,
		rebalancer:      rebalancer,
		writable:        writable,
		ecHealer:        ecHealer,
		s3Auth:          auth,
	}, nil
}

// Run starts the server and blocks until shutdown signal is received.
// It handles graceful shutdown with a configurable timeout.
func (s *Server) Run() error {
	addr := s.cfg.ListenAddr()

	// Dashboard API
	apiHandler := api.NewAPIHandler(s.metaStore, s.engine, s.metrics, s.cfg, s.activity)
	apiHandler.SetS3Authenticator(s.s3Auth)
	// Graph the measured physical footprint next to the logical size, so growth
	// that never shows up in object bytes (old versions, replicas, Raft logs) is
	// visible in Prometheus and not just in the dashboard (issue #43).
	s.metrics.SetDiskUsage(apiHandler.DiskUsage)
	// Share the node write gate so the admin drain/undrain endpoints toggle the
	// same flag the S3 handler enforces.
	apiHandler.SetWritable(s.writable)
	apiHandler.SetSearchIndex(s.searchIndex)
	apiHandler.SetMigrator(migrate.NewManager(s.metaStore, s.engine))
	apiHandler.SetSnapshotManager(snapshot.NewManager(s.metaStore))
	// Per-bucket encryption controls (enable/rotate/shred) for the dashboard share
	// the SAME manager as the engine, so a shred evicts the live key cache too.
	if s.keyMgr != nil {
		apiHandler.SetKeyManager(s.keyMgr)
	}
	if s.replicationFunc != nil {
		apiHandler.SetReplicationFunc(s.replicationFunc)
	}
	// Cluster object routing: dashboard uploads place each file on its hash owner,
	// and downloads/deletes proxy to the owner — so dashboard data is consistent
	// with the S3 path across the cluster.
	if s.failoverProxy != nil && s.clusterProxy != nil {
		apiHandler.SetClusterRouting(
			func(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
				return s.failoverProxy.ForwardWithRetry(w, r, bucket, key)
			},
			func(bucket, key string) (string, bool) {
				return s.clusterProxy.OwnerAPIAddr(bucket, key)
			},
		)
		// Cluster-wide capacity rollup: this node aggregates every node's
		// /api/v1/system for the mc-admin-info style view.
		apiHandler.SetClusterInfo(s.cfg.Cluster.NodeID, s.clusterProxy.NodeAddrs, s.cfg.Cluster.Secret)
	}
	// In-progress multipart state is node-local (issue #32), so the orphan reclaim
	// must ask the local store whether an upload still exists rather than the
	// Raft-backed one, which never sees it.
	apiHandler.SetLocalStore(s.store)
	// Cluster membership + rebalance operations for the admin API / vaults3-cli.
	if s.clusterNode != nil {
		apiHandler.SetClusterController(
			clusterControllerAdapter{n: s.clusterNode, rt: s.shardRuntime},
			func() {
				if s.rebalancer != nil {
					s.rebalancer.Trigger()
				}
			},
			func() bool { return s.rebalancer != nil && s.rebalancer.IsRunning() },
		)
	}

	// Update checker (notifier always; auto-apply only if explicitly enabled).
	updater := selfupdate.New(Version)
	apiHandler.SetUpdater(updater)
	if s.cfg.AutoUpdate.Enabled {
		go s.runUpdateChecker(updater)
	}
	if s.vectorMgr != nil {
		apiHandler.SetVectorManager(s.vectorMgr)
		// Persist the vector index periodically so embeddings survive restarts.
		go func() {
			t := time.NewTicker(2 * time.Minute)
			defer t.Stop()
			for range t.C {
				if err := s.vectorMgr.Save(); err != nil {
					slog.Warn("vector: periodic save failed", "error", err)
				}
			}
		}()
	}
	if s.scanWorker != nil {
		apiHandler.SetScanner(s.scanWorker)
	}
	if s.tieringMgr != nil {
		apiHandler.SetTieringManager(s.tieringMgr)
	}
	if s.ecHealer != nil {
		apiHandler.SetHealer(s.ecHealer)
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
				oidcValidator.SetClientSecret(s.cfg.OIDC.ClientSecret)
				oidcValidator.SetScopes(s.cfg.OIDC.Scopes)
				// Seal login state with a key every node derives identically, so a
				// login started on one node can finish on another behind a load
				// balancer.
				oidcValidator.SetStateKey(s.cfg.Auth.AdminSecretKey)
				apiHandler.SetOIDCValidator(oidcValidator)
				slog.Info("OIDC enabled",
					"issuer", s.cfg.OIDC.IssuerURL,
					"flow", apiHandler.OIDCFlow(),
					"confidential_client", s.cfg.OIDC.ClientSecret != "",
					"scopes", oidcValidator.Scopes(),
				)
			}
		}
	}

	// When ConsolePort is set, the dashboard (Web UI) + its API move to a separate
	// listener (issue #18) so the S3 API and the console can have independent ports,
	// network rules, and TLS. Otherwise everything is served on the main port.
	splitConsole := s.cfg.Server.ConsolePort > 0 && s.cfg.Server.ConsolePort != s.cfg.Server.Port

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler(s.metrics.StartTime()))
	mux.HandleFunc("/ready", readyHandler(s.store))
	mux.Handle("/metrics", s.metrics)
	if !splitConsole {
		mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard/favicon.svg", http.StatusMovedPermanently)
		})
		mux.Handle("/api/v1/", apiHandler)
		mux.Handle("/dashboard/", dashboard.Handler(s.cfg.Server.BasePath, s.cfg.Server.TrustForwardedPrefix))
	}

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
		// Read-only ownership probe (issue #37 diagnosis): from THIS pod's view, who
		// owns a key, where a request would route, and whether this pod holds the
		// key's metadata/data locally. curl it against every pod for the same key: if
		// they disagree on "owner", the placement ring is inconsistent across pods
		// (the read-after-write miss cause); if they agree but the owner lacks data,
		// it's placement; if only metadata lags, it's replication.
		ownershipHandler := func(w http.ResponseWriter, r *http.Request) {
			// Reveals object existence + cluster topology, and this is the public S3
			// port, so require the cluster secret when one is configured.
			if s.cfg.Cluster.Secret != "" && !hmac.Equal([]byte(r.Header.Get("X-Cluster-Secret")), []byte(s.cfg.Cluster.Secret)) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if s.clusterProxy == nil {
				http.Error(w, "cluster proxy not initialized", http.StatusServiceUnavailable)
				return
			}
			bucket := r.URL.Query().Get("bucket")
			key := r.URL.Query().Get("key")
			// Path-style fallback so a trailing slash or LB path-rewrite can't route
			// this to the S3 handler: /cluster/ownership/<bucket>/<key...>.
			if bucket == "" {
				rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/cluster/ownership"), "/")
				if i := strings.IndexByte(rest, '/'); i >= 0 {
					bucket, key = rest[:i], rest[i+1:]
				} else {
					bucket = rest
				}
			}
			ring := s.clusterProxy.Ring()
			rc := s.clusterProxy.Placement().ReplicaCount
			if rc < 1 {
				rc = 1
			}
			owner := ring.GetNode(bucket, key)
			would := ""
			if s.failoverProxy != nil {
				would = s.failoverProxy.ShouldProxy(bucket, key)
			}
			metaLocal := false
			if m, _ := s.metaStore.GetObjectMeta(bucket, key); m != nil {
				metaLocal = true
			}
			dataLocal := false
			if rd, _, err := s.engine.GetObject(bucket, key); err == nil {
				dataLocal = true
				rd.Close()
			}
			jarr := func(ss []string) string {
				var b strings.Builder
				b.WriteByte('[')
				for i, x := range ss {
					if i > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, "%q", x)
				}
				b.WriteByte(']')
				return b.String()
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, "{\"node\":%q,\"owner\":%q,\"self_is_owner\":%v,\"holders\":%s,\"would_proxy_to\":%q,\"meta_present_local\":%v,\"data_present_local\":%v,\"ring_members\":%s}\n",
				s.cfg.Cluster.NodeID, owner, owner == s.cfg.Cluster.NodeID, jarr(ring.GetNodes(bucket, key, rc)), would, metaLocal, dataLocal, jarr(ring.Nodes()))
		}
		// Register the exact path AND the subtree, so a trailing slash or a
		// path-style call (/cluster/ownership/<bucket>/<key>) still reaches this
		// handler instead of falling through to the S3 bucket handler.
		mux.HandleFunc("/cluster/ownership", ownershipHandler)
		mux.HandleFunc("/cluster/ownership/", ownershipHandler)
		mux.HandleFunc("/cluster/sysinfo", apiHandler.ClusterSysInfoHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/reclaim", apiHandler.ClusterReclaimHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/multipart-list", apiHandler.ClusterMultipartListHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/multipart-find", apiHandler.ClusterMultipartFindHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/readindex", s.clusterNode.ReadIndexHandler())
		mux.HandleFunc("/cluster/drain", apiHandler.ClusterDrainHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/object-delete", apiHandler.ClusterObjectDeleteHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/object-delete-batch", apiHandler.ClusterObjectDeleteBatchHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/replica-put", apiHandler.ClusterReplicaPutHandler(s.cfg.Cluster.Secret))
		mux.HandleFunc("/cluster/join", s.clusterNode.JoinHandler())
		mux.HandleFunc("/cluster/leave", s.clusterNode.LeaveHandler())
		mux.HandleFunc("/cluster/apply", s.clusterNode.ApplyHandler())
		if s.shardRouter != nil {
			mux.HandleFunc("/cluster/shard-call", s.shardRouter.CallHandler())
			mux.HandleFunc("/cluster/shard-apply", s.shardRouter.ApplyHandler())
		}
		slog.Info("cluster endpoints registered", "paths", []string{"/cluster/status", "/cluster/sysinfo", "/cluster/join", "/cluster/leave", "/cluster/apply"})
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
	dashURL := fmt.Sprintf("%s://%s/dashboard/", scheme, addr)
	if splitConsole {
		caddr := s.cfg.Server.ConsoleAddress
		if caddr == "" {
			caddr = s.cfg.Server.Address
		}
		dashURL = fmt.Sprintf("%s://%s:%d/dashboard/", scheme, caddr, s.cfg.Server.ConsolePort)
	}
	slog.Info("VaultS3 starting",
		"addr", addr,
		"data_dir", s.cfg.Storage.DataDir,
		"metadata_dir", s.cfg.Storage.MetadataDir,
		"dashboard", dashURL,
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
	lcWorker := lifecycle.NewWorker(s.metaStore, s.engine, s.cfg.Lifecycle.ScanIntervalSecs, s.cfg.Security.AuditRetentionDays)
	// Expiry removes the metadata through Raft, so the first node to sweep hides the
	// object from every other node's next sweep; without reaping, their copies of the
	// data are stranded forever (issue #47).
	lcWorker.SetReaper(s.reapElsewhere)
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

	// Announce this node's current address to the cluster (every node, including
	// the bootstrap one). Runs in the background, retrying until the leader
	// accepts — so pod start order doesn't matter and a restart with a new pod IP
	// self-heals.
	if s.clusterNode != nil && s.cfg.Cluster.JoinAddr != "" {
		joinCtx, joinCancel := context.WithCancel(context.Background())
		defer joinCancel()
		go s.clusterNode.AutoJoin(joinCtx, s.cfg.Cluster.JoinAddr)
	}

	// Keep the data-placement ring in sync with live Raft membership. Without
	// this, auto-clustered nodes (which join dynamically, not via static config)
	// each see only themselves on the ring and place data inconsistently.
	if s.clusterProxy != nil {
		apiPort := s.cfg.Cluster.APIPort
		if apiPort == 0 {
			apiPort = s.cfg.Server.Port
		}
		syncCtx, syncCancel := context.WithCancel(context.Background())
		defer syncCancel()
		go s.clusterProxy.RunMembershipSync(syncCtx, apiPort)
	}

	// Create the metadata shard assignment once the cluster has settled, then keep
	// this node's shard groups and their memberships in step with it. Not started
	// at all unless metadata sharding is configured.
	if s.shardService != nil {
		shardCtx, shardCancel := context.WithCancel(context.Background())
		defer shardCancel()
		go s.shardService.Run(shardCtx)
	}

	// Start backup scheduler if enabled
	if s.backupSched != nil {
		backupCtx, backupCancel := context.WithCancel(context.Background())
		defer backupCancel()
		go s.backupSched.Run(backupCtx)
	}

	slog.Info("search index ready", "objects", s.searchIndex.Count())

	// Pre-create the buckets listed in storage.default_buckets /
	// VAULTS3_DEFAULT_BUCKETS (issue #45). Deliberately ahead of every listener:
	// on a single node this finishes before anything can be served, so a client
	// never sees the server accepting requests without the buckets it declared.
	// (Clustered, it returns immediately and finishes in the background — the
	// Raft write it needs cannot commit until the cluster has a leader.)
	bucketErrCh := make(chan error, 1)
	if err := s.startDefaultBuckets(bucketErrCh); err != nil {
		return err
	}

	// Start separate inter-node listener if configured
	var interNodeServer *http.Server
	if s.cfg.Server.InterNodePort > 0 && s.clusterNode != nil {
		interNodeAddr := fmt.Sprintf("%s:%d", s.cfg.Server.InterNodeAddress, s.cfg.Server.InterNodePort)
		interNodeMux := http.NewServeMux()
		interNodeMux.HandleFunc("/cluster/status", s.clusterNode.StatusHandler())
		interNodeMux.HandleFunc("/cluster/sysinfo", apiHandler.ClusterSysInfoHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/reclaim", apiHandler.ClusterReclaimHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/multipart-list", apiHandler.ClusterMultipartListHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/multipart-find", apiHandler.ClusterMultipartFindHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/readindex", s.clusterNode.ReadIndexHandler())
		interNodeMux.HandleFunc("/cluster/drain", apiHandler.ClusterDrainHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/object-delete", apiHandler.ClusterObjectDeleteHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/object-delete-batch", apiHandler.ClusterObjectDeleteBatchHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/replica-put", apiHandler.ClusterReplicaPutHandler(s.cfg.Cluster.Secret))
		interNodeMux.HandleFunc("/cluster/join", s.clusterNode.JoinHandler())
		interNodeMux.HandleFunc("/cluster/leave", s.clusterNode.LeaveHandler())
		interNodeMux.HandleFunc("/cluster/apply", s.clusterNode.ApplyHandler())
		if s.shardRouter != nil {
			interNodeMux.HandleFunc("/cluster/shard-call", s.shardRouter.CallHandler())
			interNodeMux.HandleFunc("/cluster/shard-apply", s.shardRouter.ApplyHandler())
		}
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

	// Start the separate console (dashboard) listener if configured (issue #18).
	var consoleServer *http.Server
	if splitConsole {
		caddr := s.cfg.Server.ConsoleAddress
		if caddr == "" {
			caddr = s.cfg.Server.Address
		}
		consoleAddr := fmt.Sprintf("%s:%d", caddr, s.cfg.Server.ConsolePort)
		cmux := http.NewServeMux()
		cmux.HandleFunc("/health", healthHandler(s.metrics.StartTime()))
		cmux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard/favicon.svg", http.StatusMovedPermanently)
		})
		cmux.Handle("/api/v1/", apiHandler)
		cmux.Handle("/dashboard/", dashboard.Handler(s.cfg.Server.BasePath, s.cfg.Server.TrustForwardedPrefix))
		cmux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard/", http.StatusFound)
		})
		var chandler http.Handler = cmux
		chandler = middleware.RequestID(chandler)
		chandler = middleware.SecurityHeaders(chandler)
		chandler = middleware.PanicRecovery(chandler)
		consoleServer = &http.Server{Addr: consoleAddr, Handler: chandler}
		go func() {
			slog.Info("console (dashboard) listener started", "addr", consoleAddr)
			var err error
			if s.cfg.Server.TLS.Enabled {
				err = consoleServer.ListenAndServeTLS(s.cfg.Server.TLS.CertFile, s.cfg.Server.TLS.KeyFile)
			} else {
				err = consoleServer.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				slog.Error("console listener error", "error", err)
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
	case err := <-bucketErrCh:
		return err
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
	if consoleServer != nil {
		consoleServer.Shutdown(ctx)
	}
	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown timed out", "timeout", timeout, "error", err)
		return err
	}

	slog.Info("server stopped gracefully")
	return nil
}

// requiresLeaderRead reports whether a request is a bucket-wide read whose result
// depends on fully-replicated metadata, so it must be served by the Raft leader for
// read-your-writes consistency (issue #37). This covers object listings (ListObjects
// v1/v2, ListObjectVersions) and bucket sub-resource reads, all keyed at the bucket
// level (key == "") via GET. It deliberately excludes ListMultipartUploads
// (?uploads): in-progress multipart state is kept node-local alongside the part data
// (issue #32), so the leader is not authoritative for it and it keeps owner routing.
func requiresLeaderRead(r *http.Request, bucket, key string) bool {
	if bucket == "" || key != "" || r.Method != http.MethodGet {
		return false
	}
	if _, ok := r.URL.Query()["uploads"]; ok {
		return false
	}
	return true
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
	// Shard groups first: they take their stream layers from the mux the control
	// node owns, and the node closes that mux on shutdown.
	if s.shardRuntime != nil {
		s.shardRuntime.Close()
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
