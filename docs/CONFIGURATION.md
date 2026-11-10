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
  shutdown_timeout_secs: 30  # how long running requests get to finish on shutdown; give the container runtime longer (docker --stop-timeout 35, Kubernetes terminationGracePeriodSeconds 35)
  idle_timeout_secs: 650     # close keep-alive connections idle this long; keep above any proxy's idle timeout (0 means 650)
  # `docker stop` waits only 10s before killing the process, so give it more:
  # `docker stop -t 35`, or `stop_grace_period: 35s` in Compose. Kubernetes waits
  # 30s by default (`terminationGracePeriodSeconds`).
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
  kms:     # SSE-KMS: a mode of its own, needs no static key
    enabled: false
    provider: "local"          # "local" ("vault" does not work yet, see SSE-KMS below)
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
  repair:
    interval_secs: 600       # restore replica counts after a node is lost, negative disables
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

VaultS3 has three encryption modes. **Pick one.** They are server-wide settings
and they do not combine: whichever is configured decides how every object on the
server is sealed. A bucket cannot ask for a mode the server is not running.

The same goes for a request: with encryption off (the default), an upload that
asks for SSE-S3 (`x-amz-server-side-encryption: AES256`, as Terraform's S3
backend with `encrypt = true` or `aws s3 cp --sse` send) is stored unencrypted.
It is not refused, so such clients keep working, and the answer does not claim
encryption: `x-amz-server-side-encryption` is returned only for a bucket that
encrypts at rest (not on UploadPart, since parts are stored as sent until the
upload completes). Turn encryption on below if objects must be.


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
    provider: "local"
    local_key: ""              # hex-encoded 32-byte key
```

SSE-KMS takes its data key from the provider and caches it in memory. It needs no `key` of its own.

**The `vault` provider does not work.** It reads Vault's data key in the wrong encoding, so a server configured with it refuses to start (`KMS key fetch failed: encoding/hex: invalid byte`). It also asks Vault for a new data key each time instead of keeping one, so fixing the encoding alone would make every restart lose the key. No data can have been written with it. Use `local` until it is reimplemented.

**Per-bucket keys**, each bucket sealed with its own data key, wrapped by a master key, so a bucket can be rotated or crypto-shredded on its own:

```yaml
encryption:
  enabled: true
  key: ""          # 64-char hex master key that wraps each bucket's data key
  per_bucket: true
```

In this mode a bucket stores plaintext until it opts in, with
`PutBucketEncryption` and `SSEAlgorithm: AES256`. That is the only algorithm this
mode can honour, and asking for `aws:kms` here is refused rather than accepted
and ignored. To use SSE-KMS instead, configure `encryption.kms` above and leave
`per_bucket` off. See [the design note](design/per-bucket-encryption.md).

**How objects are sealed.** From 4.4.53 an object is encrypted in 1 MiB chunks, each its own AES-256-GCM message with a nonce derived from a per-object random prefix, the chunk's index, and a flag marking the last chunk. That binding is what makes chunks impossible to reorder or move between objects and makes a truncated object fail to read rather than come back short. Each chunk is authenticated before any of its bytes are served, so a client never receives unverified plaintext.

The practical effect is on memory: a read costs one chunk, not one copy of the object. Before 4.4.53 an object was a single GCM message, which cannot be verified incrementally, so every concurrent reader of a large object held all of it (issue #49). Objects written by earlier versions keep the old format and are still read. Rewriting one migrates it. Reading an old-format object still costs about its own size, so rewrite large ones if pod memory is tight.

## Virtual-Hosted Style URLs

Set `server.domain` to enable virtual-hosted style access:

```yaml
server:
  domain: "s3.example.com"
```

This enables `bucket-name.s3.example.com/key` in addition to the default `s3.example.com/bucket-name/key` path-style.


## Free Disk Space Reserve

`storage.min_free_mb` (default `100`, env `VAULTS3_MIN_FREE_MB`) keeps that much
free on every disk the server writes to (`data_dir`, `metadata_dir`, and when
in use the Raft log, the cold tier and the erasure disks). Below it, new object
data is refused with `507 Insufficient Storage`: uploads, copies, multipart
uploads, dashboard uploads and rollbacks, batch jobs, migrations and copies from
other cluster nodes. Reads, deletes and settings go on, so a full disk can always
be emptied: delete objects, release a legal hold or retention, or add a lifecycle
rule that expires them. Without the reserve, a completely full metadata disk
could not even delete: the metadata database writes new pages for every change.
A write that runs out of space anyway is answered with 507 too, which S3 SDKs do
not retry.

Keep it at 32 or more: the metadata database grows its file 16 MiB at a time. On
a disk shared by data and metadata, deleting objects frees space and writes resume
on their own. A separate metadata disk does not shrink when records are deleted
(the space they leave is reused for new records), so give that disk room to grow.
`0` disables the reserve. On Windows free space is not measured and the reserve
does not apply.

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

## External Authorization

Delegate each access decision to an HTTP endpoint you run. Off by default:

```yaml
external_auth:
  enabled: false
  url: ""                # POST target, http or https
  timeout_ms: 2000
  cache_ttl_secs: 10     # 0 is 9x slower, see the guide before changing
  authoritative: false   # deny-only: narrows IAM, never widens it
  fail_open: false       # unreachable endpoint DENIES
  token: ""              # sent as "Authorization: Bearer <token>"
```

VaultS3 POSTs `{"accessKey","user","action","resource","sourceIP"}` and expects a
200 carrying `{"allow": true|false}`. The admin identity is never sent, so a
broken endpoint cannot lock you out. Set the URL with
`VAULTS3_EXTERNAL_AUTH_URL` and the shared secret with
`VAULTS3_EXTERNAL_AUTH_TOKEN`.

Full guide, including the two modes and what this costs on the request path:
[Access control](ACCESS-CONTROL.md#external-authorization-webhook).

## Migrating From Another S3 Server

The dashboard's Migrate page imports buckets from MinIO, SeaweedFS, Garage,
Ceph, AWS S3, R2, Wasabi, B2 or any S3-compatible source. The source is a URL
typed into the dashboard, so by default it may not be at a loopback or private
address: otherwise an admin session could make the server reach anything on the
internal network. To migrate from a source on your own network (a MinIO on the
same Docker network, the LAN, localhost), allow it:

```yaml
migration:
  allow_private_sources: true   # default false; env: VAULTS3_ALLOW_PRIVATE_MIGRATION_SOURCES=true
```

"Private" includes carrier-grade NAT space (100.64.0.0/10), so a source
reached over Tailscale needs the setting too. The cloud metadata services
(link-local addresses, and Alibaba Cloud's 100.100.100.200) stay refused
either way. Without the setting, a private source is refused with a message
that names it. Turn it off again once the migration is done if untrusted
people can sign in to the dashboard as admin.

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
