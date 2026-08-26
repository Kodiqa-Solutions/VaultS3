# Configuration

The config file, every option group, TLS, health checks, and metrics.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

## Write a config file

`vaults3 setup` asks a handful of questions, creates the directories, and writes
a config containing only what you chose:

```bash
./vaults3 setup                  # interactive
./vaults3 setup --non-interactive --data-dir ./data --default-bucket local
./vaults3 -config vaults3.yaml   # then start with it
```

It writes the file `0600` because it holds the admin secret, and refuses to
overwrite an existing config unless you pass `--force`.

## Configure

For the full annotated set of options, edit `configs/vaults3.yaml`:

```yaml
server:
  address: "0.0.0.0"
  port: 9000
  domain: ""  # set to enable virtual-hosted URLs (e.g. "s3.example.com")
  shutdown_timeout_secs: 30
  tls:
    enabled: false
    cert_file: ""
    key_file: ""

storage:
  data_dir: "./data"
  metadata_dir: "./metadata"

auth:
  admin_access_key: "vaults3-admin"
  admin_secret_key: ""   # empty: generated on first start and stored

encryption:
  enabled: false
  key: ""  # 64-character hex string (32 bytes) for SSE-S3
  kms:     # SSE-KMS (optional, overrides static key when enabled)
    enabled: false
    provider: "vault"          # "vault" or "local"
    vault_addr: ""
    vault_token: ""
    key_name: "vaults3-dek"
    local_key: ""

compression:
  enabled: false

logging:
  enabled: false
  file_path: "./access.log"
  level: "info"  # debug, info, warn, error

lifecycle:
  scan_interval_secs: 3600

security:
  ip_allowlist: []     # global CIDR allow list, empty = allow all
  ip_blocklist: []     # global CIDR deny list
  audit_retention_days: 90
  sts_max_duration_secs: 43200  # max STS token duration (12 hours)

# Distributed clustering (optional)
cluster:
  enabled: false
  node_id: "node-1"
  bind_addr: "0.0.0.0"
  raft_port: 9001
  api_port: 9000
  bootstrap: true          # true for the first node
  peers: []                # ["node-2@host2:9001", "node-3@host3:9001"]
  peer_apis:               # nodeID → "host:apiPort"
    node-2: "host2:9000"
    node-3: "host3:9000"
  metadata_shards: 1       # >1 splits object metadata across that many Raft groups
  metadata_replicas: 3     # nodes holding each shard
  placement:
    replica_count: 3
    read_quorum: 2
    write_quorum: 2
    virtual_nodes: 128
  detector:
    probe_interval_secs: 5
    suspect_after: 3
    down_after: 6
  rebalance:
    max_bandwidth_mbps: 50
    batch_size: 100

# Erasure coding (optional, works with or without clustering)
erasure:
  enabled: false
  data_shards: 4
  parity_shards: 2
  block_size: 4194304      # 4MB, objects smaller than this bypass EC
  data_dirs: []            # multiple disk paths for shard distribution
  heal_interval: 300       # seconds between heal scans

# Replication
replication:
  enabled: false
  mode: "push"             # "push" (one-way) or "active-active" (bidirectional)
  site_id: "site-1"        # unique site ID for active-active mode
  conflict_strategy: "last-writer-wins"  # "last-writer-wins", "largest-object", "site-preference"
  preferred_site: ""       # for site-preference strategy
  peers: []
  scan_interval_secs: 10
  max_retries: 5
  batch_size: 100
```

## TLS

Enable HTTPS by providing cert and key files:

```yaml
server:
  tls:
    enabled: true
    cert_file: "/path/to/cert.pem"
    key_file: "/path/to/key.pem"
```


## Encryption at Rest

VaultS3 supports two encryption modes:

**SSE-S3 (Static Key)**, Simple setup with a hex-encoded 32-byte key:

```yaml
encryption:
  enabled: true
  key: ""  # 64-char hex string: openssl rand -hex 32
```

**SSE-KMS (Key Management Service)**, Per-object encryption with KMS-managed data encryption keys:

```yaml
encryption:
  enabled: true
  kms:
    enabled: true
    provider: "vault"          # "vault" (HashiCorp Vault) or "local" (fallback)
    vault_addr: "http://vault:8200"
    vault_token: "hvs.xxx"
    key_name: "vaults3-dek"    # Transit engine key name
    local_key: ""              # hex-encoded fallback key (when provider: "local")
```

SSE-KMS fetches data encryption keys from HashiCorp Vault's Transit engine, caches them in memory, and supports key rotation.

**How objects are sealed.** From 4.4.53 an object is encrypted in 1 MiB chunks, each its own AES-256-GCM message with a nonce derived from a per-object random prefix, the chunk's index, and a flag marking the last chunk. That binding is what makes chunks impossible to reorder or move between objects and makes a truncated object fail to read rather than come back short. Each chunk is authenticated before any of its bytes are served, so a client never receives unverified plaintext.

The practical effect is on memory: a read costs one chunk, not one copy of the object. Before 4.4.53 an object was a single GCM message, which cannot be verified incrementally, so every concurrent reader of a large object held all of it (issue #49). Objects written by earlier versions keep the old format and are still read. Rewriting one migrates it. Reading an old-format object still costs about its own size, so rewrite large ones if pod memory is tight.

## Virtual-Hosted Style URLs

Set `server.domain` to enable virtual-hosted style access:

```yaml
server:
  domain: "s3.example.com"
```

This enables `bucket-name.s3.example.com/key` in addition to the default `s3.example.com/bucket-name/key` path-style.


## Health Checks

```bash
curl http://localhost:9000/health   # liveness: {"status":"ok","uptime":"5h23m"}
curl http://localhost:9000/ready    # readiness: checks BoltDB, returns 503 if unhealthy
```


## Prometheus Metrics

Access metrics at `GET /metrics`:

```bash
curl http://localhost:9000/metrics
```

Exposes: request counts by method, bytes in/out, per-bucket storage size and object counts, per-bucket request/bytes/error counters, quota usage, Go runtime stats (goroutines, memory, GC).

Storage is reported both ways, which is what makes growth diagnosable:

| Series | Measures |
|--------|----------|
| `vaults3_storage_size_bytes_total` | **Logical**: each object's current version, counted once. |
| `vaults3_disk_usage_bytes{dir="..."}` | **Physical**: what that directory actually occupies on disk, per data / metadata / erasure / cold-tier / Raft directory. Includes replicas, parity shards and non-current versions. |
| `vaults3_disk_usage_files{dir="..."}` | File count behind the figure above. |
| `vaults3_disk_usage_bytes_total` | Physical total for this node. |
| `vaults3_disk_usage_scanned_timestamp_seconds` | When the footprint was last measured, so a stale reading is detectable. |

Graphing physical against logical shows amplification directly. The `vaults3_disk_usage_*` series come from a cached background walk and are absent when `storage.usage_scan_interval_secs` is `0`.


## Per-Bucket Prometheus Metrics

The `/metrics` endpoint includes per-bucket counters (limited to top 100 buckets):

```
vaults3_bucket_requests_total{bucket="my-bucket",method="PUT"} 42
vaults3_bucket_requests_total{bucket="my-bucket",method="GET"} 156
vaults3_bucket_bytes_in_total{bucket="my-bucket"} 10485760
vaults3_bucket_bytes_out_total{bucket="my-bucket"} 52428800
vaults3_bucket_errors_total{bucket="my-bucket"} 3
```

These complement the existing global metrics and per-bucket storage metrics, enabling monitoring and alerting per bucket.

## Rate Limiting

Token bucket rate limiting is **on by default**, because an unauthenticated request is rate limited before it is authenticated, so the limiter is the only thing bounding what a flood costs the server:

```yaml
rate_limit:
  enabled: true
  requests_per_sec: 2000  # per client IP
  burst_size: 4000
  per_key_rps: 2000       # per access key
  per_key_burst: 4000
```

Each client IP and access key gets an independent token bucket. Requests exceeding the limit receive `429 Too Many Requests` with a `Retry-After: 1` header, which S3 SDKs retry automatically. Stale buckets are cleaned up after 5 minutes of inactivity.

The defaults sit far above what a real client sends: a saturating 8-thread `boto3` client measures around 1300 requests per second, well inside the ceiling, while a 64-thread flood is cut off. Set `enabled: false` to turn it off entirely.

**Behind a reverse proxy**, the per-IP bucket keys on the connection's real address rather than `X-Forwarded-For`, since a caller can forge that header. Every client arriving through nginx or a Kubernetes ingress therefore shares a single per-IP bucket. Raise `requests_per_sec` to cover your combined traffic in that setup, and rely on `per_key_rps` for per-tenant fairness.

```bash
# Check rate limiter status
curl http://localhost:9000/api/v1/ratelimit/status -H "Authorization: Bearer <token>"
```
