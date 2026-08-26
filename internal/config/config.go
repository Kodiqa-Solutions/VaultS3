package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Storage       StorageConfig       `yaml:"storage"`
	Auth          AuthConfig          `yaml:"auth"`
	Encryption    EncryptionConfig    `yaml:"encryption"`
	Compression   CompressionConfig   `yaml:"compression"`
	Packing       PackingConfig       `yaml:"packing"`
	Logging       LoggingConfig       `yaml:"logging"`
	Lifecycle     LifecycleConfig     `yaml:"lifecycle"`
	Security      SecurityConfig      `yaml:"security"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Replication   ReplicationConfig   `yaml:"replication"`
	Scanner       ScannerConfig       `yaml:"scanner"`
	Tiering       TieringConfig       `yaml:"tiering"`
	Backup        BackupConfig        `yaml:"backup"`
	RateLimit     RateLimitConfig     `yaml:"rate_limit"`
	OIDC          OIDCConfig          `yaml:"oidc"`
	Lambda        LambdaConfig        `yaml:"lambda"`
	Erasure       ErasureConfig       `yaml:"erasure"`
	Cluster       ClusterConfig       `yaml:"cluster"`
	Memory        MemoryConfig        `yaml:"memory"`
	Vector        VectorConfig        `yaml:"vector"`
	AutoUpdate    AutoUpdateConfig    `yaml:"auto_update"`
	Metrics       MetricsConfig       `yaml:"metrics"`
	Debug         bool                `yaml:"debug"`
}

// AutoUpdateConfig controls the daily update check and optional self-update.
// The check (notifier) and apply are both opt-in. Self-update only ever replaces
// the binary — object data, metadata, and config are never touched.
type AutoUpdateConfig struct {
	Enabled          bool `yaml:"enabled"`              // run the daily check + dashboard notifier
	Apply            bool `yaml:"apply"`                // also download+install automatically (binary deploys only)
	CheckIntervalHrs int  `yaml:"check_interval_hours"` // default 24
	AllowMajor       bool `yaml:"allow_major"`          // allow auto-crossing a major version (default false)
}

// VectorConfig configures the optional semantic / vector search add-on. When
// enabled, object text is embedded via an OpenAI-compatible endpoint and indexed
// for similarity search (semantic search + RAG retrieval).
type VectorConfig struct {
	Enabled        bool     `yaml:"enabled"`
	EmbeddingURL   string   `yaml:"embedding_url"`    // OpenAI-compatible /v1/embeddings endpoint
	APIKey         string   `yaml:"api_key"`          // optional; empty for local servers (Ollama, etc.)
	Model          string   `yaml:"model"`            // embedding model name
	Dimensions     int      `yaml:"dimensions"`       // optional hint; pinned on first vector otherwise
	MaxVectors     int      `yaml:"max_vectors"`      // cap on indexed vectors (0 = default)
	AutoIndex      bool     `yaml:"auto_index"`       // embed text objects automatically on upload
	IndexPrefixes  []string `yaml:"index_prefixes"`   // if set, only auto-index keys under these prefixes
	MaxObjectBytes int64    `yaml:"max_object_bytes"` // skip auto-indexing objects larger than this
	PersistPath    string   `yaml:"persist_path"`     // file to persist the index across restarts
	TimeoutSecs    int      `yaml:"timeout_secs"`     // embedding HTTP timeout
}

type ErasureConfig struct {
	Enabled      bool     `yaml:"enabled"`
	DataShards   int      `yaml:"data_shards"`
	ParityShards int      `yaml:"parity_shards"`
	BlockSize    int64    `yaml:"block_size"`
	DataDirs     []string `yaml:"data_dirs"`
	HealInterval int      `yaml:"heal_interval_secs"`
}

type ClusterConfig struct {
	Enabled       bool              `yaml:"enabled"`
	NodeID        string            `yaml:"node_id"`
	BindAddr      string            `yaml:"bind_addr"`
	RaftPort      int               `yaml:"raft_port"`
	APIPort       int               `yaml:"api_port"`  // API port for this node (for proxy, defaults to server.port)
	Peers         []string          `yaml:"peers"`     // Raft peers: "nodeID@host:raftPort"
	PeerAPIs      map[string]string `yaml:"peer_apis"` // nodeID → "host:apiPort" for proxying
	Bootstrap     bool              `yaml:"bootstrap"`
	JoinAddr      string            `yaml:"join_addr"` // API addr of an existing member to auto-join on startup (host:apiPort)
	Secret        string            `yaml:"secret"`    // shared secret authenticating inter-node endpoints (join/leave/apply)
	DataDir       string            `yaml:"data_dir"`
	SnapshotCount int               `yaml:"snapshot_count"`
	// MetadataShards splits object metadata across independent Raft groups so a
	// node indexes only the buckets it holds instead of every object in the
	// cluster (issue #50). 0 or 1 keeps the single group that holds everything,
	// which is the default and the pre-sharding topology. Buckets hash to a
	// shard, so this is fixed for the life of a cluster: changing it on an
	// existing cluster requires migrating metadata between groups.
	MetadataShards int `yaml:"metadata_shards"`
	// MetadataReplicas is how many nodes hold each shard. Clamped to the cluster
	// size. Only meaningful when MetadataShards > 1.
	MetadataReplicas int             `yaml:"metadata_replicas"`
	Placement        PlacementConfig `yaml:"placement"`
	Detector         DetectorConfig  `yaml:"detector"`
	Rebalance        RebalanceConfig `yaml:"rebalance"`
}

type PlacementConfig struct {
	ReplicaCount int `yaml:"replica_count"`
	ReadQuorum   int `yaml:"read_quorum"`
	WriteQuorum  int `yaml:"write_quorum"`
	VirtualNodes int `yaml:"virtual_nodes"`
}

type DetectorConfig struct {
	ProbeIntervalSecs int `yaml:"probe_interval_secs"`
	SuspectAfter      int `yaml:"suspect_after"`
	DownAfter         int `yaml:"down_after"`
	ProbeTimeoutSecs  int `yaml:"probe_timeout_secs"`
}

type RebalanceConfig struct {
	MaxBandwidthMBps int `yaml:"max_bandwidth_mbps"`
	BatchSize        int `yaml:"batch_size"`
}

// MetricsConfig controls what the Prometheus endpoint exposes anonymously.
type MetricsConfig struct {
	// PublicBucketLabels serves the per-bucket series (names, object counts,
	// sizes, quotas) to unauthenticated scrapes. Off by default: that is the same
	// inventory the S3 API protects behind ListBuckets, and it was readable by
	// anyone who could reach the port. A scrape carrying the cluster secret
	// always receives the full set.
	PublicBucketLabels bool `yaml:"public_bucket_labels"`
}

type MemoryConfig struct {
	MaxSearchEntries int `yaml:"max_search_entries"`
	GoMemLimitMB     int `yaml:"go_mem_limit_mb"`
}

type OIDCConfig struct {
	Enabled   bool   `yaml:"enabled"`
	IssuerURL string `yaml:"issuer_url"`
	ClientID  string `yaml:"client_id"`
	// ClientSecret turns the dashboard into a confidential OAuth client. It is
	// needed for the authorization-code flow on providers that issue one (the
	// default for Authentik and Keycloak). The secret is only ever used
	// server-side, in the back-channel token exchange, and is never sent to the
	// browser. Leave empty for a public client, which authenticates with PKCE
	// alone. (env: VAULTS3_OIDC_CLIENT_SECRET)
	ClientSecret string `yaml:"client_secret"`
	// Flow selects the OAuth flow: "code" (authorization code + PKCE, the modern
	// default), "implicit" (legacy, deprecated by OAuth 2.1), or "" / "auto" to
	// pick from what the provider advertises in its discovery document.
	Flow string `yaml:"flow"`
	// Scopes pins the OAuth scopes requested at login. Empty negotiates them from
	// the provider's discovery document, which is what keeps a request for
	// "groups" from failing outright on providers that do not define it.
	Scopes          []string          `yaml:"scopes"`
	AllowedDomains  []string          `yaml:"allowed_domains"`
	RoleMapping     map[string]string `yaml:"role_mapping"`
	AutoCreateUsers bool              `yaml:"auto_create_users"`
	// AllowImplicitFlow re-enables POST /api/v1/auth/oidc, which accepts an ID
	// token the client presents. Off by default: that token is bound to nothing
	// this server issued, so a captured or attacker-minted one creates a session
	// (security assessment finding 3). The authorization-code flow is unaffected.
	AllowImplicitFlow bool `yaml:"allow_implicit_flow"`
	JWKSCacheSecs     int  `yaml:"jwks_cache_secs"`
}

type LambdaConfig struct {
	Enabled         bool  `yaml:"enabled"`
	MaxResponseSize int64 `yaml:"max_response_size"`
	TimeoutSecs     int   `yaml:"timeout_secs"`
	MaxWorkers      int   `yaml:"max_workers"`
	QueueSize       int   `yaml:"queue_size"`
}

type RateLimitConfig struct {
	Enabled        bool    `yaml:"enabled"`
	RequestsPerSec float64 `yaml:"requests_per_sec"`
	BurstSize      int     `yaml:"burst_size"`
	PerKeyRPS      float64 `yaml:"per_key_rps"`
	PerKeyBurst    int     `yaml:"per_key_burst"`
}

type TieringConfig struct {
	Enabled          bool   `yaml:"enabled"`
	ColdDataDir      string `yaml:"cold_data_dir"`
	MigrateAfterDays int    `yaml:"migrate_after_days"`
	ScanIntervalSecs int    `yaml:"scan_interval_secs"`
}

type BackupTarget struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"` // "local" or "s3"
	Path        string `yaml:"path"`
	S3Endpoint  string `yaml:"s3_endpoint"`
	S3AccessKey string `yaml:"s3_access_key"`
	S3SecretKey string `yaml:"s3_secret_key"`
	S3Bucket    string `yaml:"s3_bucket"`
}

type BackupConfig struct {
	Enabled       bool           `yaml:"enabled"`
	Targets       []BackupTarget `yaml:"targets"`
	ScheduleCron  string         `yaml:"schedule_cron"`
	RetentionDays int            `yaml:"retention_days"`
	Incremental   bool           `yaml:"incremental"`
}

type ScannerConfig struct {
	Enabled          bool   `yaml:"enabled"`
	WebhookURL       string `yaml:"webhook_url"`
	TimeoutSecs      int    `yaml:"timeout_secs"`
	QuarantineBucket string `yaml:"quarantine_bucket"`
	FailClosed       bool   `yaml:"fail_closed"`
	MaxScanSizeBytes int64  `yaml:"max_scan_size_bytes"`
	Workers          int    `yaml:"workers"`
}

type ReplicationPeer struct {
	Name      string `yaml:"name"`
	URL       string `yaml:"url"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
}

type ReplicationConfig struct {
	Enabled          bool              `yaml:"enabled"`
	Mode             string            `yaml:"mode"`              // "push" (default) or "active-active"
	SiteID           string            `yaml:"site_id"`           // unique identifier for this site (active-active)
	ConflictStrategy string            `yaml:"conflict_strategy"` // "last-writer-wins", "largest-object", "site-preference"
	PreferredSite    string            `yaml:"preferred_site"`    // for site-preference strategy
	Peers            []ReplicationPeer `yaml:"peers"`
	ScanIntervalSecs int               `yaml:"scan_interval_secs"`
	MaxRetries       int               `yaml:"max_retries"`
	BatchSize        int               `yaml:"batch_size"`
}

type NotificationsConfig struct {
	MaxWorkers  int                  `yaml:"max_workers"`
	QueueSize   int                  `yaml:"queue_size"`
	TimeoutSecs int                  `yaml:"timeout_secs"`
	MaxRetries  int                  `yaml:"max_retries"`
	Kafka       KafkaNotifyConfig    `yaml:"kafka"`
	NATS        NATSNotifyConfig     `yaml:"nats"`
	Redis       RedisNotifyConfig    `yaml:"redis"`
	AMQP        AMQPNotifyConfig     `yaml:"amqp"`
	Postgres    PostgresNotifyConfig `yaml:"postgres"`
}

type KafkaNotifyConfig struct {
	Enabled bool     `yaml:"enabled"`
	Brokers []string `yaml:"brokers"`
	Topic   string   `yaml:"topic"`
}

type NATSNotifyConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
	Subject string `yaml:"subject"`
}

type RedisNotifyConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
	Channel string `yaml:"channel"`
	ListKey string `yaml:"list_key"`
}

type AMQPNotifyConfig struct {
	Enabled    bool   `yaml:"enabled"`
	URL        string `yaml:"url"`
	Exchange   string `yaml:"exchange"`
	RoutingKey string `yaml:"routing_key"`
}

type PostgresNotifyConfig struct {
	Enabled bool   `yaml:"enabled"`
	ConnStr string `yaml:"conn_str"`
	Table   string `yaml:"table"`
}

type SecurityConfig struct {
	IPAllowlist        []string `yaml:"ip_allowlist"`
	IPBlocklist        []string `yaml:"ip_blocklist"`
	AuditRetentionDays int      `yaml:"audit_retention_days"`
	STSMaxDurationSecs int      `yaml:"sts_max_duration_secs"`
}

type ServerConfig struct {
	Address             string    `yaml:"address"`
	Port                int       `yaml:"port"`
	Domain              string    `yaml:"domain"` // base domain for virtual-hosted URLs (e.g. "localhost", "s3.example.com")
	ShutdownTimeoutSecs int       `yaml:"shutdown_timeout_secs"`
	TLS                 TLSConfig `yaml:"tls"`
	InterNodeAddress    string    `yaml:"internode_address"` // separate bind address for inter-node traffic
	InterNodePort       int       `yaml:"internode_port"`    // separate port for inter-node traffic
	// ConsolePort, when set, serves the dashboard (Web UI) + its API on a separate
	// port from the S3 API — so each can have its own network rules / TLS / proxy
	// (MinIO-style). 0 = serve everything on Port. See issue #18.
	ConsoleAddress string `yaml:"console_address"`
	ConsolePort    int    `yaml:"console_port"`
	// BasePath hosts the whole app under a reverse-proxy subpath, e.g. "/vaults3"
	// so the dashboard is reachable at /vaults3/dashboard/ (issue #36). Empty =
	// served at the root.
	BasePath string `yaml:"base_path"`
	// TrustForwardedPrefix lets the subpath be auto-detected from the proxy's
	// X-Forwarded-Prefix header when BasePath is unset. Off by default: the header
	// is client-supplied, so only enable it behind a proxy you trust to set/strip
	// it. When BasePath is set it always wins and this is irrelevant (issue #36).
	TrustForwardedPrefix bool `yaml:"trust_forwarded_prefix"`
}

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type StorageConfig struct {
	DataDir     string `yaml:"data_dir"`
	MetadataDir string `yaml:"metadata_dir"`
	// Buckets to create on startup if they do not exist yet, so a container
	// deployment needs no init container or S3 client just to get its first
	// bucket (issue #45). Existing buckets are left untouched.
	// (env: VAULTS3_DEFAULT_BUCKETS, comma-separated)
	DefaultBuckets []string `yaml:"default_buckets"`
	// UsageScanIntervalSecs is how often, at most, VaultS3 measures its own
	// on-disk footprint by walking the data directories, so the dashboard can
	// separate "VaultS3 is using this much" from the filesystem's total used
	// space (issue #43). The walk runs in the background and only when someone
	// asks for the numbers. Set to 0 to disable it on very large or slow
	// filesystems. (env: VAULTS3_USAGE_SCAN_INTERVAL_SECS)
	UsageScanIntervalSecs int `yaml:"usage_scan_interval_secs"`
}

type AuthConfig struct {
	AdminAccessKey string `yaml:"admin_access_key"`
	AdminSecretKey string `yaml:"admin_secret_key"`
}

type EncryptionConfig struct {
	Enabled bool                `yaml:"enabled"`
	Key     string              `yaml:"key"` // hex-encoded 32-byte key (64 hex chars) for SSE-S3
	KMS     KMSEncryptionConfig `yaml:"kms"` // SSE-KMS configuration
	// PerBucket switches from one server-wide key to per-bucket keys: `key` becomes
	// the master KEK that wraps a per-bucket data key, and a bucket is encrypted only
	// after it opts in via PUT ?encryption. See docs/design/per-bucket-encryption.md.
	PerBucket bool `yaml:"per_bucket"`
	// LegacyKey (hex, 32 bytes) is the previous server-wide key, used in per-bucket
	// mode to keep reading objects written before the switch. Optional.
	LegacyKey string `yaml:"legacy_key"`
}

// LegacyKeyBytes decodes the optional legacy global key (empty -> nil).
func (e *EncryptionConfig) LegacyKeyBytes() ([]byte, error) {
	if e.LegacyKey == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(e.LegacyKey)
	if err != nil {
		return nil, fmt.Errorf("legacy_key must be hex-encoded: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("legacy_key must be 32 bytes (64 hex chars), got %d", len(key))
	}
	return key, nil
}

type KMSEncryptionConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Provider   string `yaml:"provider"` // "vault" or "local"
	VaultAddr  string `yaml:"vault_addr"`
	VaultToken string `yaml:"vault_token"`
	KeyName    string `yaml:"key_name"`
	LocalKey   string `yaml:"local_key"` // hex-encoded fallback key
}

type CompressionConfig struct {
	Enabled bool `yaml:"enabled"`
}

// PackingConfig controls small-file packing: objects up to MaxObjectSize are
// packed (as zstd frames) into large append-only volume files instead of
// individual files, avoiding per-file overhead for huge numbers of tiny objects.
// Experimental; does not compose with encryption or erasure coding yet.
type PackingConfig struct {
	Enabled              bool    `yaml:"enabled"`
	MaxObjectSize        int64   `yaml:"max_object_size"`        // bytes; <= this is packed (default 1 MiB)
	VolumeMaxSize        int64   `yaml:"volume_max_size"`        // bytes; roll to a new volume past this (default 1 GiB)
	CompactIntervalHours int     `yaml:"compact_interval_hours"` // background compaction interval; 0 = disabled
	CompactMinDeadRatio  float64 `yaml:"compact_min_dead_ratio"` // compact a volume once this fraction is dead (default 0.5)
}

type LoggingConfig struct {
	Enabled  bool   `yaml:"enabled"`
	FilePath string `yaml:"file_path"`
	Level    string `yaml:"level"` // debug, info, warn, error (default: info)
}

type LifecycleConfig struct {
	ScanIntervalSecs int `yaml:"scan_interval_secs"`
}

// KeyBytes returns the decoded encryption key bytes.
func (e *EncryptionConfig) KeyBytes() ([]byte, error) {
	if !e.Enabled {
		return nil, nil
	}
	key, err := hex.DecodeString(e.Key)
	if err != nil {
		return nil, fmt.Errorf("encryption key must be hex-encoded: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes (64 hex chars), got %d bytes", len(key))
	}
	return key, nil
}

// Load reads a config file, applying defaults for everything it does not set.
// A missing file is an error: the caller named it, so a typo must not silently
// start a server with settings nobody chose.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return parse(data)
}

// LoadOrDefaults is Load for a path the caller did not choose, the built-in
// default location. A missing file there is not an error, it is a first run:
// the defaults alone are a working single-node server, so a freshly downloaded
// binary starts instead of failing on a file it never shipped with (issue #51).
//
// It reports whether defaults were used, so the caller can say so.
func LoadOrDefaults(path string) (cfg *Config, usedDefaults bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, false, fmt.Errorf("read config: %w", err)
		}
		cfg, err = parse(nil)
		return cfg, true, err
	}
	cfg, err = parse(data)
	return cfg, false, err
}

// Defaults is the configuration of a server told nothing at all: a single node
// on port 9000 with local storage and no optional subsystem enabled.
func Defaults() *Config {
	cfg, _ := parse(nil)
	return cfg
}

func parse(data []byte) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Address:             "0.0.0.0",
			Port:                9000,
			ShutdownTimeoutSecs: 30,
		},
		Storage: StorageConfig{
			DataDir:               "./data",
			MetadataDir:           "./metadata",
			UsageScanIntervalSecs: 300,
		},
		Logging: LoggingConfig{
			FilePath: "./access.log",
		},
		Lifecycle: LifecycleConfig{
			ScanIntervalSecs: 3600,
		},
		Security: SecurityConfig{
			AuditRetentionDays: 90,
			STSMaxDurationSecs: 43200,
		},
		Notifications: NotificationsConfig{
			MaxWorkers:  4,
			QueueSize:   256,
			TimeoutSecs: 10,
			MaxRetries:  3,
		},
		Replication: ReplicationConfig{
			ScanIntervalSecs: 30,
			MaxRetries:       5,
			BatchSize:        100,
		},
		Scanner: ScannerConfig{
			TimeoutSecs:      30,
			QuarantineBucket: "vaults3-quarantine",
			MaxScanSizeBytes: 104857600, // 100MB
			Workers:          2,
		},
		Tiering: TieringConfig{
			MigrateAfterDays: 30,
			ScanIntervalSecs: 3600,
		},
		Backup: BackupConfig{
			ScheduleCron:  "0 2 * * *",
			RetentionDays: 30,
		},
		// On by default. A server reachable from the internet is found by
		// scanners within seconds, and an unauthenticated flood is answered
		// before authentication runs, so the limiter is the only thing bounding
		// what it costs us. The ceiling is set far above what a real client
		// sends (measured: a saturating 8-thread boto3 client runs at ~1300
		// req/s) so enabling it by default throttles abuse and nothing else.
		// It is also per-IP on RemoteAddr, and behind a reverse proxy every
		// client shares that address, so a low ceiling here would throttle a
		// whole deployment at once.
		RateLimit: RateLimitConfig{
			Enabled:        true,
			RequestsPerSec: 2000,
			BurstSize:      4000,
			PerKeyRPS:      2000,
			PerKeyBurst:    4000,
		},
		OIDC: OIDCConfig{
			JWKSCacheSecs: 3600,
		},
		Lambda: LambdaConfig{
			MaxResponseSize: 10 * 1024 * 1024, // 10MB
			TimeoutSecs:     30,
			MaxWorkers:      4,
			QueueSize:       256,
		},
		Memory: MemoryConfig{
			MaxSearchEntries: 50000,
		},
	}

	if len(data) > 0 {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	// Apply environment variable overrides
	applyEnvOverrides(cfg)

	// A YAML list can carry stray whitespace, and the env form accepts both "a,b"
	// and "a, b" — normalise the startup bucket list from whichever source it came.
	cfg.Storage.DefaultBuckets = splitList(strings.Join(cfg.Storage.DefaultBuckets, ","))

	// Validate encryption config
	if cfg.Encryption.Enabled {
		if _, err := cfg.Encryption.KeyBytes(); err != nil {
			return nil, fmt.Errorf("invalid encryption config: %w", err)
		}
	}

	return cfg, nil
}

// applyEnvOverrides applies environment variable overrides to the config.
// Environment variables take precedence over YAML config values.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("VAULTS3_ACCESS_KEY"); v != "" {
		cfg.Auth.AdminAccessKey = v
	}
	if v := os.Getenv("VAULTS3_SECRET_KEY"); v != "" {
		cfg.Auth.AdminSecretKey = v
	}
	if v := os.Getenv("VAULTS3_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv("VAULTS3_BASE_PATH"); v != "" {
		cfg.Server.BasePath = v
	}
	if v := os.Getenv("VAULTS3_TRUST_FORWARDED_PREFIX"); v != "" {
		cfg.Server.TrustForwardedPrefix = v == "true" || v == "1"
	}
	if v := os.Getenv("VAULTS3_CONSOLE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.ConsolePort = p
		}
	}
	if v := os.Getenv("VAULTS3_CONSOLE_ADDRESS"); v != "" {
		cfg.Server.ConsoleAddress = v
	}
	// Kept out of the config file so the OAuth client secret can come from a
	// Kubernetes Secret or a Docker secret rather than a mounted ConfigMap.
	if v := os.Getenv("VAULTS3_OIDC_CLIENT_SECRET"); v != "" {
		cfg.OIDC.ClientSecret = v
	}
	if v := os.Getenv("VAULTS3_ADDRESS"); v != "" {
		cfg.Server.Address = v
	}
	if v := os.Getenv("VAULTS3_DOMAIN"); v != "" {
		cfg.Server.Domain = v
	}
	if v := os.Getenv("VAULTS3_DATA_DIR"); v != "" {
		cfg.Storage.DataDir = v
	}
	if v := os.Getenv("VAULTS3_METADATA_DIR"); v != "" {
		cfg.Storage.MetadataDir = v
	}
	// Comma-separated, e.g. VAULTS3_DEFAULT_BUCKETS=app-data,backups
	if v := os.Getenv("VAULTS3_DEFAULT_BUCKETS"); v != "" {
		cfg.Storage.DefaultBuckets = splitList(v)
	}
	// 0 disables the footprint walk entirely, so parse rather than require non-empty.
	if v := os.Getenv("VAULTS3_USAGE_SCAN_INTERVAL_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Storage.UsageScanIntervalSecs = n
		}
	}
	if v := os.Getenv("VAULTS3_ENCRYPTION_KEY"); v != "" {
		cfg.Encryption.Enabled = true
		cfg.Encryption.Key = v
	}
	if v := os.Getenv("VAULTS3_TLS_CERT"); v != "" {
		cfg.Server.TLS.CertFile = v
	}
	if v := os.Getenv("VAULTS3_TLS_KEY"); v != "" {
		cfg.Server.TLS.KeyFile = v
	}
	if os.Getenv("VAULTS3_TLS_CERT") != "" && os.Getenv("VAULTS3_TLS_KEY") != "" {
		cfg.Server.TLS.Enabled = true
	}
	if v := os.Getenv("VAULTS3_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("VAULTS3_CLUSTER_NODE_ID"); v != "" {
		cfg.Cluster.NodeID = v
	}
	if v := os.Getenv("VAULTS3_CLUSTER_BIND_ADDR"); v != "" {
		cfg.Cluster.BindAddr = v
	}
	if v := os.Getenv("VAULTS3_CLUSTER_RAFT_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Cluster.RaftPort = p
		}
	}
	if v := os.Getenv("VAULTS3_CLUSTER_DATA_DIR"); v != "" {
		cfg.Cluster.DataDir = v
	}
	if v := os.Getenv("VAULTS3_CLUSTER_METADATA_SHARDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cluster.MetadataShards = n
		}
	}
	if v := os.Getenv("VAULTS3_CLUSTER_METADATA_REPLICAS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cluster.MetadataReplicas = n
		}
	}
	// Per-pod cluster wiring (the Helm StatefulSet derives these from the pod
	// ordinal so node-0 bootstraps and the rest auto-join).
	if v := os.Getenv("VAULTS3_CLUSTER_ENABLED"); v != "" {
		cfg.Cluster.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("VAULTS3_CLUSTER_BOOTSTRAP"); v != "" {
		cfg.Cluster.Bootstrap = v == "true" || v == "1"
	}
	if v := os.Getenv("VAULTS3_CLUSTER_JOIN_ADDR"); v != "" {
		cfg.Cluster.JoinAddr = v
	}
	if v := os.Getenv("VAULTS3_CLUSTER_SECRET"); v != "" {
		cfg.Cluster.Secret = v
	}
	if v := os.Getenv("VAULTS3_CLUSTER_PEERS"); v != "" {
		cfg.Cluster.Peers = splitList(v)
	}
}

// splitList parses a comma-separated environment variable into a trimmed list,
// dropping empty entries so "a, b," yields two items.
func splitList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func (c *Config) ListenAddr() string {
	return fmt.Sprintf("%s:%d", c.Server.Address, c.Server.Port)
}
