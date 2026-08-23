<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="assets/logo-with-text.svg">
    <img alt="VaultS3" src="assets/logo-with-text.svg" width="480">
  </picture>
</p>

<p align="center">
  <strong>Lightweight S3-compatible object storage. Single binary, idles in 17 MB of RAM, built-in dashboard.</strong>
</p>

<p align="center">
  <a href="https://github.com/Kodiqa-Solutions/VaultS3/actions"><img src="https://github.com/Kodiqa-Solutions/VaultS3/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://hub.docker.com/r/eniz1806/vaults3"><img src="https://img.shields.io/docker/pulls/eniz1806/vaults3?logo=docker&logoColor=white&color=2496ED" alt="Docker Pulls"></a>
  <a href="https://github.com/Kodiqa-Solutions/VaultS3/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-AGPL_v3-4F46E5" alt="License"></a>
  <a href="https://golang.org"><img src="https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white" alt="Go"></a>
  <a href="https://github.com/Kodiqa-Solutions/VaultS3"><img src="https://img.shields.io/badge/S3_Operations-80+-10B981" alt="S3 Ops"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick Start</a> &middot;
  <a href="#features">Features</a> &middot;
  <a href="#supported-s3-operations">S3 API</a> &middot;
  <a href="#web-dashboard">Dashboard</a> &middot;
  <a href="#cli-tool">CLI</a> &middot;
  <a href="#docker">Docker</a>
</p>

---

<p align="center">
  <img src="assets/demo.gif" alt="VaultS3 Dashboard Demo" width="800">
</p>

---

## Why VaultS3?

> [!NOTE]
> **This is a one-person project, and you should know that before you trust it with your data.**
>
> I built VaultS3 for myself. I was running MinIO, the pieces I depended on moved behind a paid tier, and I did not want to rent back something I already had. So I wrote my own.
>
> It is on GitHub because I doubt I was the only one in that position. What you get is a maintainer who runs this in production himself and fixes things because he runs into them too. What you do not get is a company standing behind it. That is the trade, and I would rather you made the call with your eyes open.
>
> If something breaks, [open an issue](https://github.com/Kodiqa-Solutions/VaultS3/issues). If you would rather fix it yourself, [pull requests are welcome](CONTRIBUTING.md).

Object storage has several good open-source options, and which one fits depends on what you are optimising for. Here are the facts, measured or taken from each project's own documentation, and below them an honest answer about when to pick something else.

| | VaultS3 | MinIO | Silo | RustFS | SeaweedFS | Garage |
|---|:---:|:---:|:---:|:---:|:---:|:---:|
| GitHub stars | 1.3k | 61k¹ | 2.5k | 31k | 34k | 4.4k |
| License | AGPL-3.0 | AGPL-3.0 | AGPL-3.0 | Apache-2.0 | Apache-2.0 | AGPL-3.0 |
| Maintained in the open | **Yes** | Archived¹ | Yes | Yes | Yes | Yes |
| RAM, idle² | **17 MiB** | 184 MiB | 101 MiB | 70 MiB | 98 MiB | 23 MiB |
| Components to run | **1** | 1 | 1 | 1 | 3 | 1 |

> ¹ MinIO removed the admin console from its Community Edition in 2025 and archived the open-source repository in February 2026, so its star count reflects a project that is no longer developed in the open. Full management now requires the paid AIStor product.
> ² Measured August 2026 on one host, all six idle with no traffic, Docker working set after a 110 second settle, second reading taken to confirm the figures had stopped moving. **Memory under load is higher for every one of them**: VaultS3 peaks around 185 MiB writing 64 MiB objects at concurrency 16. Reproduce it with `docker stats --no-stream`.

### When to pick something else

- **You already run MinIO** and want it to keep working: **[Silo](https://github.com/pgsty/silo)**. A community fork by Pigsty that keeps the line maintained and restores the console MinIO cut. Same on-disk format, same API, same `MINIO_*` variables, so an existing deployment carries on unchanged.
- **AGPL is a problem for you**: **[RustFS](https://github.com/rustfs/rustfs)** or **[SeaweedFS](https://github.com/seaweedfs/seaweedfs)**, both Apache-2.0. RustFS is also the largest of these by adoption, though its own README still marks distributed mode, lifecycle and KMS as under testing.
- **You need a distributed filesystem**, not only object storage: **SeaweedFS**, with a mature FUSE mount and a filer layer. It runs as master, volume and filer rather than one process.
- **You want the smallest possible replicated store** and do not need erasure coding or versioning: **[Garage](https://git.deuxfleurs.fr/Deuxfleurs/garage)**, which idles in 23 MiB and is deliberately narrow in scope.

### When VaultS3 is the answer

One binary, the smallest footprint of the six, and the things you would otherwise assemble or pay for already in the box: a full dashboard, IAM with policies and OIDC, versioning with diff and rollback, per-bucket encryption with rotation and crypto-shredding, erasure coding, Raft clustering, active-active replication, lifecycle, notifications, a FUSE mount, full-text and vector search, virus scanning, tiering and scheduled backups. No paid tier holds any of it back, and there is no telemetry.

### How this compares to Amazon S3

S3 is the API VaultS3 implements, not a competitor you would swap it for. If you are weighing self-hosting against S3 itself, these are the trade-offs, and most of them favour S3:

| | VaultS3 | Amazon S3 |
|---|---|---|
| Durability | Your disks and configuration | 11 nines, across 3+ availability zones by default |
| Scale | ~10M objects per node, a billion needs ~30 nodes | Effectively unlimited |
| Operations & compliance | Yours, including any certification | No infrastructure to run. AWS is certified, your configuration still isn't |
| Cost | Hardware. **No per-request or egress charges** | Per GB, per request, per GB egressed |
| Exit path | Ordinary files and a BoltDB index behind the S3 API | S3 API |

**Self-hosting wins when** egress or request charges dominate your bill, when the data has to stay on hardware you control, when clients are on the same network as the storage, or when you need a cost that does not move with traffic. **S3 wins** when you want durability and availability you do not have to engineer, scale you do not have to plan for, or an audit whose scope stops at your own configuration instead of reaching all the way down to the disks.

> S3 figures reflect publicly available AWS information as of **August 2026**, please open an issue if a cell is out of date. AWS changes storage classes and pricing more often than the projects in the table above change features.

```bash
make build && ./vaults3
# Server at http://localhost:9000
# Dashboard at http://localhost:9000/dashboard/
```

## Production Readiness

VaultS3 is honest about what's battle-tested versus still maturing. Pick the lane that matches your risk tolerance:

| Path | Maturity | Notes |
|---|---|---|
| **Single-node** (S3 API, versioning, IAM, dashboard) | ✅ Stable | The default deployment. Broad test coverage. Runs in production today. |
| **Erasure coding** (single-node, multi-disk) | ✅ Stable | Reed-Solomon encode/reconstruct and the background healer have fault-injection tests (lose disks → reconstruct → heal). |
| **Tiering & backup** | ✅ Stable | Hot/cold migration, transparent promotion, and full/incremental backup are tested. Restore is a manual file copy. |
| **Multi-node Raft clustering** | 🟡 Beta | Metadata writes replicate via Raft consensus (writes accepted on any node via leader-forwarding), object data is placed/served by a live-membership hash ring, inter-node calls are authenticated, and on Kubernetes the cluster auto-forms (leader bootstrap + auto-join + self-heal). Validated end-to-end on a real 3-node cluster: leader election & failover, node recovery with catch-up, cross-node reads, and concurrent load, including a 10,000-write list-then-write-then-read workload behind a gateway with a node restarted mid-run. Still operationally newer, not yet stress/scale/multi-region hardened, so validate against your workload before trusting it as the only copy of critical data. |
| **Sharded metadata** (`cluster.metadata_shards > 1`) | 🟠 New in 4.4.54 | Splits object metadata across independent Raft groups so metadata capacity grows with the cluster instead of every node holding the whole index. Validated on real three-node clusters, local and in containers: shard assignment, group membership reconciliation, routed reads and writes from a node holding no copy of a shard, and an unreachable shard reporting `503` rather than a phantom `404`. Newer than everything above it, and the shard count is fixed when the cluster first commits its assignment, so treat it as opt-in for new clusters you can validate. Off by default. |
| **Active-active replication** | 🟡 Beta | Vector-clock conflict resolution is unit-tested. The cross-site sync worker is less exercised in the wild. |

**Security:** VaultS3 was reviewed by an external white-box security assessment in August 2026, which reported 14 findings. All are fixed in 4.4.56. See [SECURITY.md](SECURITY.md) for the summary and [CHANGELOG.md](CHANGELOG.md) for each finding. If you run an earlier version, upgrade and read the [upgrade notes](#upgrading-to-4456-security-release).

**Recommendation:** run single-node (optionally with erasure coding across local disks) for production data you care about, and treat clustering/active-active as advanced opt-in features you validate first. Always keep an independent backup. See the **[Scaling & Operations Guide](docs/SCALING.md)** for redundancy layering and recovery runbooks, and the **[Benchmarks guide](docs/BENCHMARKS.md)** for a reproducible way to measure throughput and RAM on your own hardware.

## Project and support

VaultS3 is a [Kodiqa Solutions](https://www.vaults3.com) project.
Copyright (C) 2026 Kodiqa Solutions, licensed under the
[GNU Affero General Public License v3.0](LICENSE).

### What VaultS3 will never do

I built this because MinIO moved features I was already using behind a paid
tier. So here is what I am committing to, in public, with a date on it.

- **Nothing that is free today ever moves behind a paid tier.** Not the
  dashboard, not clustering, not erasure coding, not encryption, not versioning,
  not search. If it is in the box now, it stays in the box.
- **The core stays AGPL-3.0.** It does not get relicensed to something
  proprietary.
- **No telemetry.** VaultS3 never phones home. There is nothing to switch off,
  because there is nothing there to begin with.
- **No account to download**, and **no licence key to run the server**.
- **A public advisory for every security fix**, including the embarrassing ones.

**What is commercial, so this is not evasive.** There are paid add-ons for
organisations running VaultS3 at scale, listed at
[vaults3.com/enterprise](https://www.vaults3.com/enterprise/): a multi-cluster
Fleet Console, a Kubernetes Operator, a multi-tenant Gateway and a Compliance
Pack. They solve problems a single server does not have, they are built as
separate programs that talk to the core over its API, and they are how this
project gets funded. That is the arrangement, stated plainly: the storage engine
is free and stays free, and I sell tools around it. New paid products may be
built. Nothing is ever removed from the core to create one. Commercial
enquiries: **support@vaults3.com**.

**On the CLA.** Contributors sign one, which means I hold enough copyright to
relicense the core if I chose to. I am telling you that rather than hoping you
do not check. The first promise on this list is the one that matters, and it is
the one to hold me to.

**If ownership ever changes.** These promises are mine. If I ever transfer
VaultS3 to someone else, I will say so publicly and in advance, and I will not
pretend my promises bind a new owner. You would get to make your own decision
with notice, which is more than I got.

*Last updated: 23 August 2026. This section is in git, so every change to it is
in the history.*

### What happens if this project stops

A fair question to ask of any storage software before you put data in it, and it
deserves a concrete answer rather than reassurance:

- **The licence cannot be revoked.** VaultS3 is AGPL-3.0. Every release stays
  available, forkable and buildable by anyone, whatever happens to the people
  who wrote it.
- **There is nothing around it to go stale.** One static binary, no control
  plane, no external services, no account to keep, nothing that phones home. It
  keeps running on the machine you put it on.
- **Your data is not locked in a format anyone has to reverse-engineer.** Objects
  are ordinary files on disk and the index is a BoltDB file, served over the S3
  API. Migrating away is `rclone sync` or `aws s3 sync` to anything else that
  speaks S3, at whatever pace you like, with the source still serving reads the
  whole time.

That exit path is the reason to be comfortable adopting it, and it is worth
checking for any storage product you evaluate, including this one.

## Features

- **S3-compatible API**: Works with any S3 client (AWS CLI, mc, boto3, minio-js, s3fs), including directory-marker objects (`folder/` keys) so tools that represent folders as zero-byte objects work correctly
- **Single binary**: One file, no runtime dependencies, no Docker required
- **Low memory**: a small single-node deploy idles at about **17 MiB**, measured against MinIO's 184 MiB and Garage's 23 MiB on the same host (see the table above). A clustered node under sustained large-object load costs more, because it also forwards bodies to the owner and fans replicas out to peers, so size cluster pods from your own measurement, see the [benchmarks guide](docs/BENCHMARKS.md)
- **BoltDB metadata**: Embedded key-value store, no external database needed
- **S3 Signature V4**: Standard AWS authentication
- **AES-256-GCM encryption at rest**: SSE-S3 (static key) and SSE-KMS (HashiCorp Vault or local key provider) encryption modes. Objects are sealed in 1 MiB chunks, so encrypted reads stream and cost a chunk of memory rather than a copy of the object
- **Migrating older encrypted objects**: objects written before 4.4.53 were sealed as one AES-GCM message, which cannot be streamed on read because the authentication tag covers the whole object, so each read costs its own size in latency and memory. `vaults3-cli storage reencrypt` reports how many are affected and rewrites them in the current chunked format with `--apply`. Key rotation does not do this, it mints a new key version without touching object bodies
- **Per-bucket encryption keys**: For bucket-per-tenant setups, each bucket can be encrypted with its own key that is **not shared** with other tenants (or opt out and stay plaintext). Envelope encryption (master KEK wraps a per-bucket data key). Opt in per bucket via `PUT /{bucket}?encryption` or the dashboard. Supports key rotation and crypto-shredding. Enable with `encryption.per_bucket: true`, see [design doc](docs/design/per-bucket-encryption.md)
- **SSE-C (customer-provided keys)**: Operator-blind per-object encryption: the client supplies the key per request (`x-amz-server-side-encryption-customer-*`). The server encrypts/decrypts with it and stores only the key's MD5, never the key
- **Bucket policies**: Public-read, private, custom S3-compatible JSON policies. Supports the standard AWS `Principal` forms (`"*"`, `{"AWS": "*"}`, `{"AWS": ["*"]}`), wildcard actions, explicit `Deny` precedence, and per-bucket `Resource` matching. Granting `s3:GetObject` to everyone makes objects publicly readable and `s3:ListBucket` makes the listing public, as separate permissions; bucket sub-resources (`?policy`, `?acl`, ...) always require authentication. **Public Access Block** (`BlockPublicPolicy` / `RestrictPublicBuckets`) overrides any policy and blocks anonymous access
- **Quota management**: Per-bucket size and object count limits
- **Rate limiting**: Token bucket rate limiter per client IP and per access key to prevent abuse
- **S3 Select**: Execute SQL queries on CSV, JSON, and Parquet objects without downloading the full file
- **Multipart upload**: Full lifecycle (Create, UploadPart, UploadPartCopy, Complete, Abort, ListUploads, ListParts)
- **Bucket tagging**: S3-compatible tag sets with PUT/GET/DELETE
- **Bucket/Object ACL**: S3-compatible ACL responses (GET/PUT)
- **Multiple access keys**: Dynamic key management via BoltDB
- **Object tagging**: Up to 10 tags per object
- **Range requests**: Partial content downloads (206 responses)
- **Copy object**: Same-bucket and cross-bucket copies
- **Batch delete**: Multi-object delete with XML body
- **Virtual-hosted style URLs**: `bucket.domain/key` in addition to path-style
- **Bucket default retention**: Set default GOVERNANCE or COMPLIANCE retention on a bucket, auto-applied to new objects
- **Per-bucket Prometheus metrics**: Request counts, bytes in/out, and errors with bucket labels at `/metrics`
- **Prometheus metrics**: `/metrics` endpoint with storage, request, and runtime stats
- **Presigned URLs**: Pre-authenticated URL generation
- **Web dashboard**: Built-in React UI at `/dashboard/` with home overview page, file browser (grid or list layout with file-type icons, sortable columns, pagination, file preview, metadata panel, version history panel with diff viewer/rollback/tagging, multi-select, bulk delete, bulk zip download, breadcrumb navigation), drag-and-drop file and folder upload (streamed straight to storage so large files work, with subfolder structure preserved), copy-to-clipboard buttons, access key management (each key showing the user it was issued for, so a key can be traced to the application using it), activity log, storage stats with auto-refresh, read-only settings viewer, IAM management, audit trail viewer (sortable, paginated), search (sortable, paginated), notifications, replication status, lambda triggers, backup management, bucket config (versioning toggle with status indicator, lifecycle editor, CORS editor), keyboard shortcuts (`/` search, `?` help), toast notifications (success/error/info), dark/light theme, language switcher, collapsible sidebar, remember-me sign-in, responsive layout
- **Dashboard in your language**: The Web UI ships **English, German, French, and Simplified Chinese**, picked automatically from the browser's language and switchable from the top bar (the choice is remembered). Adding a language is one JSON file and no code, see [Translating the dashboard](CONTRIBUTING.md#translating-the-dashboard)
- **Health checks**: `/health` (liveness) and `/ready` (readiness) endpoints for load balancers and Kubernetes
- **Buckets on first start**: Declare the buckets a deployment needs (`VAULTS3_DEFAULT_BUCKETS=app-data,backups`, `storage.default_buckets`, or the chart's `defaultBuckets`) and the missing ones are created while the server starts, so a container needs no init container or one-off S3 client call to become usable. Existing buckets are never touched, and an invalid name or a failed create stops startup instead of coming up quietly incomplete
- **Graceful shutdown**: Drains in-flight requests on SIGTERM/SIGINT with configurable timeout
- **TLS support**: Optional HTTPS with configurable cert/key paths
- **Separate dashboard port**: Optionally serve the Web UI + its API on a dedicated port (`server.console_port`, e.g. 9001) apart from the S3 API, so each can have its own firewall rules / TLS / reverse proxy (MinIO-style)
- **Reverse-proxy subpath**: Host the whole app under a subpath (`server.base_path`, e.g. `/vaults3`) so both the dashboard (`https://example.com/vaults3/dashboard/`) and the S3 API work behind it — asset URLs and SPA routes are rewritten at serve time, and S3 SigV4 signatures are verified against the original (pre-strip) path (optionally auto-detect the subpath from the proxy's `X-Forwarded-Prefix` with `server.trust_forwarded_prefix`, off by default since that header is client-supplied)
- **Object versioning**: Per-bucket versioning with version IDs, delete markers, version-specific GET/DELETE/HEAD
- **Object locking (WORM)**: Legal hold and retention (GOVERNANCE/COMPLIANCE) to prevent deletion
- **Lifecycle rules**: Per-bucket object expiration (auto-delete after N days) and aborting incomplete multipart uploads after N days (S3 `AbortIncompleteMultipartUpload`, reclaims the parts left by killed/failed clients), run by a background worker
- **Zstandard compression**: Transparent compress-on-write, decompress-on-read with zstd (better ratio and speed than gzip). Reads **stream** the decoder, so GET time-to-first-byte stays flat regardless of object size (no whole-object buffering). Objects written by older gzip builds are still read transparently (codec auto-detected by magic number)
- **Small-file packing (experimental)**: Optionally pack objects up to a size threshold into large append-only **volume** files (each as an independent zstd frame) with byte-offset locations in BoltDB, plus background dead-space **compaction** (`POST /api/v1/compact`), avoids the per-file overhead (inodes, syscalls, disk blocks) of millions of tiny objects. Larger objects fall through to individual files. Not yet composable with encryption or erasure coding (skipped if either is enabled)
- **Scales to millions of objects**: Listing and storage stats are served from a sorted BoltDB metadata index with maintained per-bucket counters (size/count updated incrementally on every write), never a filesystem walk. So dashboard stats are O(1) and the object browser pages in milliseconds regardless of bucket size, verified at 1M+ objects (stats `13s → 0.4ms`). Metadata costs about 600 bytes per object and, in a cluster, is replicated to every node by default (object data is sharded, metadata is not), so roughly 10M objects per node is comfortable and 100M is workable with NVMe and enough RAM. Past that, `cluster.metadata_shards` splits the object metadata across independent Raft groups so each node holds only the shards it is a member of. See [docs/SCALING.md](docs/SCALING.md#11a-how-many-objects-a-cluster-can-hold)
- **Access logging**: Structured JSON lines log file of all S3 operations
- **Static website hosting**: Serve index/error documents from buckets, no auth required
- **IAM users, groups & policies**: Fine-grained access control with S3-compatible policy evaluation, default deny, wildcard matching
- **CORS per bucket**: S3-compatible CORS configuration with OPTIONS preflight support
- **STS temporary credentials**: Short-lived access keys with configurable TTL, auto-cleanup of expired keys
- **Audit trail**: Persistent audit log of allow and deny decisions with the principal, action, resource, status and **source IP**, filterable by user, bucket and time range. Behind a reverse proxy the forwarded client address is recorded rather than the proxy's own, so a denied request can be traced to where it came from. Auto-pruning via lifecycle worker (`security.audit_retention_days`, default 90)
- **IP allowlist/blocklist**: Global and per-user CIDR-based IP restrictions with IPv4/IPv6 support
- **S3 event notifications**: Per-bucket webhook notifications on object mutations with event type and key prefix/suffix filtering, plus Kafka, NATS, Redis, AMQP/RabbitMQ, PostgreSQL, and Elasticsearch backends
- **Raft clustering**: Multi-node cluster with Hashicorp Raft consensus for strongly consistent distributed metadata, automatic leader election, and node join/leave via HTTP API. Optionally shards the object metadata across independent Raft groups (`cluster.metadata_shards`) so metadata capacity grows with the cluster instead of every node holding a copy of the whole index. See [docs/design/sharded-metadata.md](docs/design/sharded-metadata.md)
- **Consistent hashing**: xxhash64-based hash ring with virtual nodes for automatic data placement and request routing across cluster nodes via reverse proxy. A read whose data has not yet been copied to the node serving it is fetched from a holder that has it, so a `GET` never reports "not found" for an object that was just written, and a hop that fails before any response reaches the client is retried against the object's other holders instead of surfacing as a gateway error
- **Erasure coding**: Reed-Solomon encoding (configurable data/parity shards) for disk-failure protection with background healer that auto-reconstructs degraded objects. Reads **and writes stream**, so neither holds the object in memory: a GET's time-to-first-byte stays flat regardless of object size, and a PUT costs a fixed number of stripe buffers rather than the object plus its parity, which is what makes large uploads at high concurrency safe. A degraded read, where a shard is actually missing, streams too: parity recovery runs one aligned stripe at a time rather than rebuilding the whole object first, so first-byte cost stays constant and a degraded read does not hold the object in memory. **Settable per bucket** alongside replica count (`vaults3-cli bucket durability`), so scratch data can be stored once while the buckets that matter keep their parity and copies: on a 3-node cluster with 4+2 coding and 3 replicas, the same data costs 4.52x with the defaults and 1.00x with both turned off
- **High availability**: Automatic failure detection (health probes with suspect/down state machine), failover proxy routing to healthy replicas, and background rebalancer for membership changes. Inter-node traffic shares a pooled HTTP transport (connection reuse instead of a new socket per call), and a node that genuinely cannot serve a request answers `503 SlowDown` with an S3 error document, which every mainstream SDK retries on its own
- **Scalable listing**: Object listing is served from the sorted BoltDB metadata index (seek to the page, `O(log n + page_size)`), so `ListObjectsV2` page latency stays flat (~0.7 ms per 1000-key page) whether a prefix holds a thousand or **a hundred million** objects (measured, not extrapolated), no full-bucket scan
  - 📖 See the **[Scaling & Operations Guide](docs/SCALING.md)** for multi-disk erasure coding, multi-node cluster setup, large-prefix listing, and lost-disk / lost-server recovery runbooks
- **Active-active replication**: Bidirectional site-to-site sync with vector clocks for causal ordering, pluggable conflict resolution (last-writer-wins, largest-object, site-preference), and change log for efficient delta sync
- **Async replication**: One-way async replication to peer VaultS3 instances with BoltDB-backed queue, retry with exponential backoff, and loop prevention
- **CLI tool**: Standalone `vaults3-cli` binary for bucket, object, user, and replication management without AWS CLI, plus `vaults3-cli info` for server version and storage capacity (used / free / total), `vaults3-cli cluster` for day-2 cluster operations (status, join, leave, drain/undrain a member, rebalance, decommission — see [docs/SCALING.md](docs/SCALING.md)), and `vaults3-cli storage reclaim` to free data files that no metadata refers to any more
- **Capacity overview**: `GET /api/v1/system` and the dashboard Stats page report the version and storage usage; in a cluster, `GET /api/v1/cluster/info` (the same dashboard panel, and `vaults3-cli info`) roll it up across all nodes with a per-node breakdown, an `mc admin info`-style view. Three sizes are reported separately because they answer different questions and are not meant to match: **logical** (each object's current version, counted once cluster-wide, since object metadata is the same on every node), **VaultS3 on disk** (what its data, metadata, erasure, cold-tier and Raft directories actually occupy, summed per node, so it includes replicas, parity shards and non-current versions), and **filesystems** (statfs of the whole volumes, which usually also hold the OS, container images and logs). The middle figure is the one to compare against logical for a real amplification ratio, with a per-directory split to tell object data apart from metadata and Raft logs. It comes from a cached background walk, `storage.usage_scan_interval_secs` (default 300, `0` disables it), and is also exported as `vaults3_disk_usage_bytes{dir=...}`
- **Presigned upload restrictions**: Enforce max file size, content type whitelist, and key prefix on presigned PUT URLs
- **Full-text search**: In-memory search index over object metadata, tags, content type, and key patterns with incremental updates
- **Semantic / vector search (optional)**: Embeds object text via any OpenAI-compatible endpoint (Ollama, llama.cpp, OpenAI…) and serves similarity search + RAG retrieval from `POST /api/v1/vectors/query`, all in the single binary, no external vector database. Searchable from the dashboard (Keyword / Semantic toggle)
- **Migrate from S3**: Import buckets and objects from MinIO, SeaweedFS, Garage, Ceph, AWS S3, Cloudflare R2, Wasabi, Backblaze B2, or any S3-compatible source via a dashboard wizard (pick a source preset that pre-fills the endpoint and SigV4 region, test connection, pick buckets, live progress, cancel in-flight jobs). **preserves each object's original modified date, user metadata, and content headers** plus **bucket policies and tags** (a faithful copy, not a same-day re-upload, something `mc mirror` can't do since it writes via PutObject). Streams objects (no in-memory buffering, so arbitrarily large objects transfer without a request-size timeout), copies objects in parallel (bounded worker pool), and is **resumable**: a restarted or crashed migration skips objects already copied and continues instead of re-transferring the whole bucket. Retries transient errors, no AWS SDK required
- **Cost estimator**: A dashboard panel that estimates what your stored data would cost on AWS S3, GCS, Cloudflare R2, Backblaze B2, and Wasabi (storage + egress) vs. self-hosting, egress-free, for free (`GET /api/v1/tco`)
- **Bucket snapshots ("git-for-buckets")**: Capture a bucket's state, diff against the live bucket, and roll back in one click (commit / diff / restore), git-style history for your data, built on versioning with no external stack. Restore even resurrects deleted objects, and is itself reversible
- **Webhook virus scanning**: POST uploaded objects to a configurable scan endpoint (ClamAV, VirusTotal, etc.) with quarantine bucket for infected files
- **Data tiering**: Automatic hot/cold storage migration based on access patterns with transparent reads and manual migration API
- **Backup scheduler**: Scheduled full/incremental backups to local directory targets with cron-like scheduling and backup history
- **Git-like versioning**: Visual diff between object versions (text and binary), version tagging with labels, one-click rollback to any version
- **FUSE mount**: Mount VaultS3 buckets as local filesystem directories with read/write support, lazy loading, and SigV4 authentication. LRU block cache (256KB blocks, configurable size), metadata cache with TTL, kernel attribute caching, and SigV4 derived key caching for fast repeated reads
- **OIDC/JWT SSO**: Sign in to the dashboard with external identity providers (Google, Keycloak, Auth0, Authentik) via OpenID Connect, using the **authorization-code flow with PKCE** (validated end-to-end against a real Keycloak and a real Authentik). The PKCE verifier, nonce and client secret never leave the server, and the login state is sealed so a login started on one cluster node can finish on another. The authorization endpoint, expected issuer and requested scopes all come from the provider's discovery document, so providers that serve a global authorization endpoint separate from each application's issuer need no rewrite rules, and a scope the provider does not define is never requested. RS256 JWT verification with JWKS auto-discovery and caching. Email domain filtering, auto-create users, OIDC group to policy mapping.
- **Lambda compute triggers**: Webhook-based function triggers on S3 events. Call external URLs with event payload and optional object body, optionally store the response as a new object. Per-bucket trigger configuration with event type and key prefix/suffix filtering. Worker pool with non-blocking dispatch.
- **SVG dashboard charts**: Pure SVG bar chart (per-bucket sizes), donut chart (request method distribution), and sparkline (request activity) on the stats page, zero dependencies
- **GitHub Actions CI**: Automated build, test, lint, and coverage on push/PR
- **pprof debug endpoint**: `/debug/pprof/*` available when `debug: true` in config for CPU/memory profiling
- **Structured logging (slog)**: All server logs use Go's `log/slog` with key-value pairs. Configurable log level (`debug`, `info`, `warn`, `error`) via `logging.level` in config
- **Request ID middleware**: Every response includes an `X-Request-Id` header for request tracing
- **Panic recovery middleware**: Catches panics, logs full stack trace, returns 500 without crashing the server
- **Request latency histogram**: `vaults3_request_duration_seconds_bucket` Prometheus histogram with 11 bucket boundaries (5ms to 10s)
- **Security headers**: CSP, X-Frame-Options (DENY), X-Content-Type-Options (nosniff), HSTS (1 year), Referrer-Policy on all dashboard responses
- **CORS origin validation**: Dashboard API restricts Access-Control-Allow-Origin to same-origin and localhost (replaces wildcard `*`)
- **Dashboard API rate limiting**: Uses existing token bucket rate limiter on `/api/v1/` endpoints, returns 429 when exceeded
- **Input validation**: DNS-compatible bucket name validation (3-63 chars, lowercase, no leading/trailing hyphen) and object key validation (max 1024 chars, no null bytes)
- **RAM optimization**: Slim search index with LRU eviction cap (50K entries default), batched last-access updates (30s flush interval), configurable Go memory limit (`GOMEMLIMIT`). Uploads stream rather than buffer, so peak memory scales with concurrency and not with object size, and a clustered node restores its metadata snapshot in bounded batches, so **startup memory is flat in the size of the cluster** (restoring 1.6M objects peaks at ~66 MB). When measuring in a container, read `anon` from `memory.stat`: `memory.current` includes page cache and will sit near the limit under write load by design ([benchmarks guide](docs/BENCHMARKS.md))
- **Streaming uploads**: A `PUT` streams to disk while its checksums are computed in passing, and compression encodes as the object flows through, so peak memory scales with concurrency rather than with concurrency multiplied by object size. A 64 MiB object at 32 concurrent uploads no longer costs gigabytes of buffered copies (SSE-C and uploads with no declared length still buffer by necessity)
- **GetObjectAttributes**: Returns object size, ETag, and storage class. Used internally by AWS SDK v2
- **Bucket encryption config**: Per-bucket server-side encryption configuration (AES256, aws:kms) via `PUT/GET/DELETE /{bucket}?encryption`
- **Public access block**: Per-bucket public access block with 4 boolean flags (BlockPublicAcls, IgnorePublicAcls, BlockPublicPolicy, RestrictPublicBuckets). `BlockPublicPolicy` and `RestrictPublicBuckets` are enforced: either one blocks anonymous access to the bucket regardless of its policy. The two ACL flags are accepted and stored for API compatibility but have no effect, since VaultS3 uses policies rather than ACLs (`PUT ?acl` is a no-op)
- **Bucket logging config**: Per-bucket access logging configuration with target bucket and prefix
- **User metadata**: Custom `x-amz-meta-*` headers on PUT/GET/HEAD
- **Conditional requests**: `If-Modified-Since`, `If-None-Match` (304), `If-Match`, `If-None-Match` (412) on GET and PUT
- **Content-MD5 validation**: Server-side integrity check on PUT with `Content-MD5` header
- **Metadata-only copy**: `x-amz-metadata-directive: REPLACE` for updating metadata without re-uploading
- **Conditional copy**: `x-amz-copy-source-if-*` headers for conditional CopyObject
- **Response header overrides**: `?response-content-type`, `?response-content-disposition`, etc. on GET
- **Inline tagging on PUT**: `x-amz-tagging` header to set tags during object upload
- **Inline retention on PUT**: `x-amz-object-lock-mode` header to set retention during upload
- **Canned ACL headers**: `x-amz-acl` and `x-amz-grant-*` headers on PUT
- **Replication status header**: `x-amz-replication-status` on GET/HEAD responses
- **Website redirect**: `x-amz-website-redirect-location` header for per-object redirects
- **S3 Checksum API**: CRC32, CRC32C, SHA1, SHA256 checksums on upload and download
- **Parts count header**: `x-amz-mp-parts-count` on HEAD for multipart objects
- **ListObjectsV1**: Marker-based pagination (`GET /{bucket}?marker=`) for legacy client compatibility
- **ListBuckets with prefix filter**: Filter bucket listing by name prefix
- **Versioning suspend**: Suspend versioning on a bucket while preserving existing versions
- **GetObject by part number**: `?partNumber=N` to retrieve individual parts of multipart objects
- **Multiple lifecycle rules**: Multiple rules per bucket with prefix, tag, and size filters
- **NoncurrentVersionExpiration**: Auto-expire non-current object versions after N days
- **AbortIncompleteMultipartUpload**: Auto-cleanup stale multipart uploads after N days
- **MaxNoncurrentVersions**: Cap retained non-current versions per object
- **ExpiredObjectDeleteMarker cleanup**: Remove orphaned delete markers automatically
- **Object size filter**: Lifecycle rules with `ObjectSizeGreaterThan` / `ObjectSizeLessThan` conditions
- **IAM policy conditions**: `StringEquals`, `StringLike`, `IpAddress`, `DateLessThan` condition operators
- **Policy variables**: `${aws:username}`, `${aws:userid}` substitution in policy resources
- **LDAP authentication**: Bind-based LDAP/LDAPS authentication with group mapping
- **STS AssumeRole**: Federated access via STS AssumeRole with session policies
- **External auth webhook**: Delegate authentication to an external HTTP endpoint
- **Access key description & status**: Metadata fields and active/inactive status on access keys
- **Governance bypass**: `x-amz-bypass-governance-retention` header for admin override of GOVERNANCE locks
- **Per-bucket replication rules**: Replication rules with prefix and tag filters per bucket
- **Replication Config API**: S3-compatible `PUT/GET/DELETE /{bucket}?replication` endpoints
- **Real-time event-driven replication**: Replicate objects immediately on mutation events
- **Existing object replication**: Replicate pre-existing objects when enabling replication rules
- **Delete marker replication**: Optionally replicate delete markers to target buckets
- **Site replication**: IAM and bucket configuration sync across sites
- **KMS integration**: HashiCorp Vault and local key provider for envelope encryption
- **Remote tiering**: Tier cold objects to an S3-compatible remote backend
- **RestoreObject API**: `POST /{bucket}/{key}?restore` to initiate restore from cold tier
- **Storage classes**: STANDARD and REDUCED_REDUNDANCY storage class support
- **Compression exclusions**: Skip compression for already-compressed file types (GZIP, JPEG, MP4, etc.)
- **Real-time event streaming**: Server-Sent Events at `/api/v1/events` for live S3 event monitoring
- **Real-time log streaming**: Server-Sent Events at `/api/v1/logs` for live access log tailing
- **Request tracing**: Server-Sent Events at `/api/v1/trace` for per-request latency tracing
- **Health diagnostics**: Detailed system diagnostics at `/api/v1/diagnostics` (disk, memory, goroutines, DB stats)
- **Manual heal API**: `POST /api/v1/heal` to trigger erasure-coded object repair on demand
- **Orphan reclaim**: `POST /api/v1/reclaim` (or `vaults3-cli storage reclaim`) finds data files that no metadata refers to any more and frees them, scanning every node in a cluster. Reports by default; `?apply=true` deletes, and nothing written in the last 24h is ever touched. A file is deleted only when metadata positively says it is gone: if a lookup cannot be answered at all, the whole bucket is reported `incomplete` and nothing in it is touched
- **Speedtest**: `POST /api/v1/speedtest` to benchmark storage throughput
- **Batch operations**: Bulk delete and copy processor for large-scale object operations
- **PROXY protocol v1**: Accept PROXY protocol connections for real client IP behind load balancers
- **Auto-TLS**: Automatic Let's Encrypt certificates with self-signed fallback
- **Inter-node network separation**: Bind cluster traffic to a dedicated network interface
- **Bucket bandwidth throttling**: Per-bucket upload/download rate limits
- **S3 Select on compressed files**: Query GZIP and BZIP2 compressed CSV/JSON objects with S3 Select
- **S3 POST policy**: HTML form-based upload with policy document validation
- **S3 Inventory reports**: Periodic CSV inventory of bucket contents
- **Snowball/TAR bulk upload**: Upload TAR archives that are automatically extracted into objects
- **FIFO quota**: Automatically delete oldest objects when bucket quota is exceeded
- **AMQP/RabbitMQ notifications**: Publish S3 events to RabbitMQ exchanges
- **PostgreSQL notifications**: Insert S3 events into a PostgreSQL table
- **Elasticsearch notifications**: Index S3 events in Elasticsearch
- **Docker image**: Multi-stage Dockerfile with built-in health check
- **YAML config**: Simple configuration, sensible defaults

## Supported S3 Operations

| Operation | Endpoint | Status |
|-----------|----------|--------|
| List Buckets | `GET /` | Done |
| Create Bucket | `PUT /{bucket}` | Done |
| Delete Bucket | `DELETE /{bucket}` | Done |
| Head Bucket | `HEAD /{bucket}` | Done |
| Put Object | `PUT /{bucket}/{key}` | Done |
| Get Object | `GET /{bucket}/{key}` | Done |
| Delete Object | `DELETE /{bucket}/{key}` | Done |
| Head Object | `HEAD /{bucket}/{key}` | Done |
| List Objects V2 | `GET /{bucket}?prefix=&max-keys=` | Done |
| Copy Object | `PUT /{bucket}/{key}` + `x-amz-copy-source` | Done |
| Batch Delete | `POST /{bucket}?delete` | Done |
| Multipart Upload | `POST/PUT/DELETE /{bucket}/{key}?uploads&uploadId` | Done |
| UploadPartCopy | `PUT /{bucket}/{key}?partNumber&uploadId` + `x-amz-copy-source` | Done |
| S3 Select | `POST /{bucket}/{key}?select&select-type=2` | Done |
| Object Tagging | `PUT/GET/DELETE /{bucket}/{key}?tagging` | Done |
| Bucket Policy | `PUT/GET/DELETE /{bucket}?policy` | Done |
| Bucket Quota | `PUT/GET /{bucket}?quota` | Done |
| Bucket Durability (erasure + replicas) | `PUT/GET /{bucket}?durability` | Done |
| Bucket Versioning | `PUT/GET /{bucket}?versioning` | Done |
| List Object Versions | `GET /{bucket}?versions` | Done |
| Object Locking (Legal Hold) | `PUT/GET /{bucket}/{key}?legal-hold` | Done |
| Object Locking (Retention) | `PUT/GET /{bucket}/{key}?retention` | Done |
| Bucket Default Retention | `PUT/GET /{bucket}?object-lock` | Done |
| Lifecycle Rules | `PUT/GET/DELETE /{bucket}?lifecycle` | Done |
| Website Hosting | `PUT/GET/DELETE /{bucket}?website` | Done |
| Bucket CORS | `PUT/GET/DELETE /{bucket}?cors` | Done |
| Presigned URLs |, | Done |
| Get Bucket Location | `GET /{bucket}?location` | Done |
| Bucket Tagging | `PUT/GET/DELETE /{bucket}?tagging` | Done |
| Bucket ACL | `GET/PUT /{bucket}?acl` | Done |
| Object ACL | `GET/PUT /{bucket}/{key}?acl` | Done |
| Get Object Attributes | `GET /{bucket}/{key}?attributes` | Done |
| Bucket Encryption | `PUT/GET/DELETE /{bucket}?encryption` | Done |
| Public Access Block | `PUT/GET/DELETE /{bucket}?publicAccessBlock` | Done |
| Bucket Logging | `PUT/GET /{bucket}?logging` | Done |
| List Multipart Uploads | `GET /{bucket}?uploads` | Done |
| List Parts | `GET /{bucket}/{key}?uploadId=X` | Done |
| Metrics | `GET /metrics` | Done |
| IAM (Users/Groups/Policies) | Dashboard API `/api/v1/iam/*` | Done |
| STS Temporary Credentials | `POST /api/v1/sts/session-token` | Done |
| Audit Trail | `GET /api/v1/audit` | Done |
| IP Restrictions | `PUT /api/v1/iam/users/{name}/ip-restrictions` | Done |
| Bucket Notifications | `PUT/GET/DELETE /{bucket}?notification` | Done |
| Notification Configs | `GET /api/v1/notifications` | Done |
| Replication Status | `GET /api/v1/replication/status` | Done |
| Replication Queue | `GET /api/v1/replication/queue` | Done |
| Presigned URL Generation | `POST /api/v1/presign` | Done |
| Full-Text Search | `GET /api/v1/search?q=...` | Done |
| Scanner Status | `GET /api/v1/scanner/status` | Done |
| Quarantine List | `GET /api/v1/scanner/quarantine` | Done |
| Tiering Status | `GET /api/v1/tiering/status` | Done |
| Tiering Migrate | `POST /api/v1/tiering/migrate` | Done |
| Backup List | `GET /api/v1/backups` | Done |
| Backup Trigger | `POST /api/v1/backups/trigger` | Done |
| Backup Status | `GET /api/v1/backups/status` | Done |
| Version Diff | `GET /api/v1/versions/diff` | Done |
| Version Tags | `GET/POST/DELETE /api/v1/versions/tags` | Done |
| Version Rollback | `POST /api/v1/versions/rollback` | Done |
| Rate Limit Status | `GET /api/v1/ratelimit/status` | Done |
| OIDC Config | `GET /api/v1/auth/oidc/config` | Done |
| OIDC Login (code flow) | `POST /api/v1/auth/oidc/start`, `POST /api/v1/auth/oidc/callback` | Done |
| OIDC Login (implicit) | `POST /api/v1/auth/oidc` | Done |
| Lambda Triggers | `PUT/GET/DELETE /{bucket}?lambda` | Done |
| Lambda Trigger List | `GET /api/v1/lambda/triggers` | Done |
| Lambda Trigger CRUD | `GET/PUT/DELETE /api/v1/lambda/triggers/{bucket}` | Done |
| Lambda Status | `GET /api/v1/lambda/status` | Done |
| Bucket Versioning (Dashboard) | `GET/PUT /api/v1/buckets/{name}/versioning` | Done |
| Bucket Lifecycle (Dashboard) | `GET/PUT/DELETE /api/v1/buckets/{name}/lifecycle` | Done |
| Bucket CORS (Dashboard) | `GET/PUT/DELETE /api/v1/buckets/{name}/cors` | Done |
| Bulk Delete (Dashboard) | `POST /api/v1/buckets/{name}/bulk-delete` | Done |
| Bulk Download Zip | `GET /api/v1/buckets/{name}/download-zip?keys=...` | Done |
| Version List (Dashboard) | `GET /api/v1/versions?bucket=X&key=Y` | Done |
| Settings | `GET /api/v1/settings` | Done |
| System / Capacity | `GET /api/v1/system` | Done |
| Cluster Capacity | `GET /api/v1/cluster/info` | Done |
| Cluster Status | `GET /cluster/status`, `GET /api/v1/cluster/status` | Done |
| Cluster Join | `POST /cluster/join`, `POST /api/v1/cluster/join` | Done |
| Cluster Leave | `POST /cluster/leave`, `POST /api/v1/cluster/leave` | Done |
| Cluster Drain / Undrain | `POST /api/v1/cluster/{drain,undrain}` | Done |
| Cluster Rebalance | `POST /api/v1/cluster/rebalance` | Done |
| Replication Sync | `POST /_replication/sync` | Done |
| List Objects V1 | `GET /{bucket}?marker=` | Done |
| Replication Config | `PUT/GET/DELETE /{bucket}?replication` | Done |
| Restore Object | `POST /{bucket}/{key}?restore` | Done |
| POST Upload (Form) | `POST /{bucket}` (multipart/form-data) | Done |
| Get Object (Part) | `GET /{bucket}/{key}?partNumber=N` | Done |
| Event Stream | `GET /api/v1/events` (SSE) | Done |
| Log Stream | `GET /api/v1/logs` (SSE) | Done |
| Request Trace | `GET /api/v1/trace` (SSE) | Done |
| Health Diagnostics | `GET /api/v1/diagnostics` | Done |
| Manual Heal | `POST /api/v1/heal` | Done |
| Speedtest | `POST /api/v1/speedtest` | Done |
| Batch Operations | `POST /api/v1/batch` | Done |
| STS AssumeRole | `POST /api/v1/sts/assume-role` | Done |
| Inventory Reports | `GET /api/v1/inventory` | Done |

## Quick Start

### Install a package

RPM, DEB and APK packages are attached to every [release](https://github.com/Kodiqa-Solutions/VaultS3/releases). They install the binary, a config file, a systemd unit and an unprivileged `vaults3` account, and leave `/var/lib/vaults3` alone on upgrade and on removal.

```bash
# Debian or Ubuntu
sudo apt install ./vaults3_4.4.63_amd64.deb
# RHEL, Rocky or Fedora
sudo rpm -i vaults3-4.4.63-1.x86_64.rpm
# Alpine
sudo apk add --allow-untrusted vaults3_4.4.63_x86_64.apk

sudo systemctl enable --now vaults3
journalctl -u vaults3 --no-pager | head -40   # the admin secret is printed once
```

Every release also ships an SPDX SBOM per platform, generated from the binary so it lists the modules actually compiled in, and a Sigstore provenance bundle attached as an asset, so a download can be verified against the workflow run and commit that produced it, offline or from a mirror:

```bash
gh attestation verify vaults3_4.4.63_amd64.deb --repo Kodiqa-Solutions/VaultS3
```

### Build from source

```bash
make build
```

### Run

```bash
./vaults3
```

That is the whole first run. With no config file present VaultS3 starts on its
built-in defaults, creates the directories it needs, and generates an admin
secret for this installation, which it prints once:

```
──────────────────────────────────────────────────────────────
 VaultS3 generated an admin secret for this new installation.
 It is shown once. Store it somewhere safe.

   Access key:  vaults3-admin
   Secret key:  4936c03e56b8ab52579aea4ab24e2eb24ac652788fb741c8

   Dashboard:   http://127.0.0.1:9000/dashboard/
──────────────────────────────────────────────────────────────
```

The secret is stored with the metadata, so later starts reuse it. Set
`VAULTS3_ACCESS_KEY` and `VAULTS3_SECRET_KEY` to use credentials of your own,
or change them from the dashboard.

### Write a config file

`vaults3 setup` asks a handful of questions, creates the directories, and writes
a config containing only what you chose:

```bash
./vaults3 setup                  # interactive
./vaults3 setup --non-interactive --data-dir ./data --default-bucket local
./vaults3 -config vaults3.yaml   # then start with it
```

It writes the file `0600` because it holds the admin secret, and refuses to
overwrite an existing config unless you pass `--force`.

### Configure

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

### Encryption at Rest

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

The practical effect is on memory: a read costs one chunk, not one copy of the object. Before 4.4.53 an object was a single GCM message, which cannot be verified incrementally, so every concurrent reader of a large object held all of it (issue #49). Objects written by earlier versions keep the old format and are still read; rewriting one migrates it. Reading an old-format object still costs about its own size, so rewrite large ones if pod memory is tight.

### Virtual-Hosted Style URLs

Set `server.domain` to enable virtual-hosted style access:

```yaml
server:
  domain: "s3.example.com"
```

This enables `bucket-name.s3.example.com/key` in addition to the default `s3.example.com/bucket-name/key` path-style.

### Prometheus Metrics

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

### Web Dashboard

The built-in dashboard is available at `http://localhost:9000/dashboard/`. Login with your admin credentials. Features:

- Bucket browser, list, create, delete buckets
- Bucket detail, view/edit policies and quotas
- File browser, list, upload (drag & drop files and folders), download, delete objects with folder navigation, multi-select with bulk delete and bulk zip download, copy-to-clipboard for S3 URIs and keys
- Access key management, create, list, revoke S3 API keys
- IAM management, users, groups, policies CRUD with attach/detach operations
- Audit trail, filter by user, bucket, time range with auto-refresh
- Search, full-text search across all buckets by key, content type, tags
- Notifications, view webhook notification configurations
- Replication, peer status cards, pending queue table
- Lambda triggers, status overview, trigger table with event filtering
- Backups, status cards, history table, manual trigger button
- Activity log, real-time S3 operation feed with auto-refresh
- Storage stats, logical size, VaultS3's measured on-disk footprint and total filesystem usage side by side (with a per-directory and per-node breakdown), per-bucket breakdown, runtime metrics, auto-refresh toggle (30s)
- Migrate, import buckets from any S3-compatible source with live progress and a Cancel button for in-flight jobs
- Version indicator, the running version is shown at the bottom of the sidebar, with an "update available" hint linking to releases
- Dark/light theme, toggle with system preference detection
- Language, English, German, French, Simplified Chinese, detected from the browser and switchable in the top bar
- Responsive layout, mobile-friendly with collapsible sidebar
- JWT-based authentication (24h tokens)

The dashboard is embedded into the binary, no separate web server needed.

#### Language

The dashboard picks a language from the browser on first load and falls back to
English. Change it with the selector in the top bar; the choice is stored per
browser, so different people using the same server can each read it in their own
language. There is no server-side setting.

Shipping today: **English, Deutsch, Francais, and simplified Chinese**. The
non-English files were drafted without a native-speaker review, so corrections
are welcome.

**Adding a language takes one JSON file and no code**: copy
`web/src/i18n/locales/en.json`, translate the values, and add one entry to
`LOCALES` in `web/src/i18n/index.tsx`. Any key you leave out falls back to
English, so a partial translation is fine to send. See
[Translating the dashboard](CONTRIBUTING.md#translating-the-dashboard) for the
full steps and the test that checks a locale file.

Server-side output (S3 API error codes, log lines) is English only.

#### Screenshots

| Buckets | File Browser |
|:---:|:---:|
| ![Buckets](assets/screenshots/dashboard-buckets.png) | ![File Browser](assets/screenshots/dashboard-file-browser.png) |

| Bucket Detail | Search |
|:---:|:---:|
| ![Bucket Detail](assets/screenshots/dashboard-bucket-detail.png) | ![Search](assets/screenshots/dashboard-search.png) |

| Dark Mode | Settings |
|:---:|:---:|
| ![Dark Mode](assets/screenshots/dashboard-home-dark.png) | ![Settings](assets/screenshots/dashboard-settings.png) |

### Health Checks

```bash
curl http://localhost:9000/health   # liveness: {"status":"ok","uptime":"5h23m"}
curl http://localhost:9000/ready    # readiness: checks BoltDB, returns 503 if unhealthy
```

### TLS

Enable HTTPS by providing cert and key files:

```yaml
server:
  tls:
    enabled: true
    cert_file: "/path/to/cert.pem"
    key_file: "/path/to/key.pem"
```

### Docker

```bash
# From Docker Hub
docker pull eniz1806/vaults3
docker run -p 9000:9000 \
  -e VAULTS3_ACCESS_KEY=myadmin \
  -e VAULTS3_SECRET_KEY=mysupersecret \
  -v ./data:/data -v ./metadata:/metadata \
  eniz1806/vaults3

# Or build locally
docker build -t vaults3 .
docker run -p 9000:9000 -v ./data:/data -v ./metadata:/metadata vaults3
```

Images are automatically published to [Docker Hub](https://hub.docker.com/r/eniz1806/vaults3) on every push to `main`.

#### Buckets on First Start

A fresh container has no buckets, which normally means an init container or a
one-off S3 client call before the app can write anything. Name the buckets you
need instead and VaultS3 creates the missing ones while it starts:

```bash
docker run -p 9000:9000 \
  -e VAULTS3_DEFAULT_BUCKETS=app-data,backups \
  -v ./data:/data -v ./metadata:/metadata \
  eniz1806/vaults3
```

or in `vaults3.yaml`:

```yaml
storage:
  default_buckets: ["app-data", "backups"]
```

- Buckets that already exist are left completely alone: no data, policy,
  versioning, or lifecycle setting is touched, so the variable is safe to keep in
  place across restarts and upgrades.
- Removing a name from the list never deletes anything.
- The setting means "these buckets must exist", so if you delete one while its
  name is still listed, the next restart creates it again, empty. Take the name
  out of the list first if you mean the deletion to stick.
- An invalid bucket name, or a bucket that cannot be created, stops startup with
  an error naming the bucket, rather than starting up quietly incomplete.
- On a cluster, creation is a replicated write like any other, so the nodes agree
  on one bucket no matter how many of them boot with the same setting.

#### Environment Variables

All settings can be overridden via environment variables (takes precedence over config file):

| Variable | Description | Default |
|----------|-------------|---------|
| `VAULTS3_ACCESS_KEY` | Admin access key | `vaults3-admin` |
| `VAULTS3_SECRET_KEY` | Admin secret key | `vaults3-secret-change-me` |
| `VAULTS3_PORT` | Server port | `9000` |
| `VAULTS3_ADDRESS` | Bind address | `0.0.0.0` |
| `VAULTS3_DOMAIN` | Domain for virtual-hosted URLs | _(empty)_ |
| `VAULTS3_BASE_PATH` | Reverse-proxy subpath (e.g. `/vaults3`) | _(empty)_ |
| `VAULTS3_TRUST_FORWARDED_PREFIX` | Auto-detect subpath from `X-Forwarded-Prefix` | `false` |
| `VAULTS3_DATA_DIR` | Object storage directory | `./data` |
| `VAULTS3_METADATA_DIR` | BoltDB metadata directory | `./metadata` |
| `VAULTS3_DEFAULT_BUCKETS` | Comma-separated buckets to create on startup if missing | _(none)_ |
| `VAULTS3_USAGE_SCAN_INTERVAL_SECS` | How often VaultS3 may re-measure its own on-disk footprint (0 disables) | `300` |
| `VAULTS3_ENCRYPTION_KEY` | 64-char hex key (enables encryption) | _(disabled)_ |
| `VAULTS3_TLS_CERT` | TLS certificate file path | _(disabled)_ |
| `VAULTS3_TLS_KEY` | TLS private key file path | _(disabled)_ |
| `VAULTS3_LOG_LEVEL` | Log level (`debug`, `info`, `warn`, `error`) | `info` |
| `VAULTS3_TRACE_FORWARD` | Log per-hop latency (DNS/connect/reuse/TTFB) for proxied cluster reads | `0` |
| `VAULTS3_TRACE_READS` | Log the cause (`meta_nil` vs `data_missing`) of cluster `GET`/`HEAD` 404s | `0` |
| `VAULTS3_OIDC_CLIENT_SECRET` | OAuth client secret for SSO (keeps it out of the config file) | _(public client)_ |

### Storage requirements

VaultS3 stores each object as a regular **file** under `data_dir` and keeps
metadata in a **BoltDB** file under `metadata_dir`, so both must point at a
**mounted filesystem**, not a raw block device. Format the disk first (**XFS
recommended**. `ext4` also works) and mount it, then point `data_dir` at a
directory on the mount. This is the same model as MinIO.

- One file per object means a filesystem with plenty of inodes (XFS handles this
  well). For workloads with millions of tiny objects, enable the experimental
  [small-file packing](#small-file-packing-experimental) mode to pack them into
  large volume files and cut per-file overhead.
- On Kubernetes, a CSI driver like **DirectPV** is a good fit, it formats disks
  with XFS and presents them as mounted PVCs, which is exactly what VaultS3 wants.

### Kubernetes

Deploy with the bundled **Helm chart** or a single **plain-manifest** quickstart
(both under [`deploy/`](deploy/)):

```bash
# Helm (configurable, production-grade)
helm install vaults3 ./deploy/helm/vaults3 \
  --namespace vaults3 --create-namespace \
  --set auth.secretKey="$(openssl rand -hex 20)" \
  --set defaultBuckets="{app-data,backups}"

# Or plain manifests (single-node, no Helm)
kubectl apply -f deploy/k8s/quickstart.yaml
```

`defaultBuckets` creates those buckets on startup if they are missing, so the
release needs no init container to become usable. See
[Buckets on First Start](#buckets-on-first-start).

Both deploy a StatefulSet (S3 API + dashboard on port `9000`), admin keys via a
Secret, `vaults3.yaml` via a ConfigMap, persistent volumes for `/data` and
`/metadata`, liveness/readiness probes (`/health`, `/ready`), and an optional
Ingress + Prometheus ServiceMonitor. See [`deploy/README.md`](deploy/README.md)
and the [chart reference](deploy/helm/vaults3/README.md).

The Helm chart can also **auto-form a Raft cluster** (Beta) with
`--set cluster.enabled=true --set replicaCount=3`: pod-0 bootstraps as leader and
the others auto-join, with self-healing on pod restart. Metadata writes replicate
across nodes via Raft consensus (a write to any node is forwarded to the leader),
so the cluster stays consistent. Clustering is functional but newer than
single-node + erasure coding, which remains the recommendation for maximum
production durability.

For backup/restore workflows, the chart also supports a single-node **Deployment**
mode (`controller.kind=Deployment`) and **existing PVCs**
(`persistence.data.existingClaim`), so you can mount a claim restored from Velero,
k8up, or a CSI snapshot, see the [chart reference](deploy/helm/vaults3/README.md#backups--restore).

### Object Versioning

Enable versioning on a bucket to keep multiple versions of objects:

```python
import boto3

s3 = boto3.client('s3', endpoint_url='http://localhost:9000',
    aws_access_key_id='vaults3-admin',
    aws_secret_access_key='vaults3-secret-change-me')

# Enable versioning
s3.put_bucket_versioning(Bucket='my-bucket',
    VersioningConfiguration={'Status': 'Enabled'})

# Upload creates a new version each time
s3.put_object(Bucket='my-bucket', Key='file.txt', Body=b'v1')
s3.put_object(Bucket='my-bucket', Key='file.txt', Body=b'v2')

# Get specific version
s3.get_object(Bucket='my-bucket', Key='file.txt', VersionId='...')

# Delete creates a delete marker (versions preserved)
s3.delete_object(Bucket='my-bucket', Key='file.txt')

# Permanently delete a specific version
s3.delete_object(Bucket='my-bucket', Key='file.txt', VersionId='...')
```

### Object Locking (WORM)

Protect objects from deletion with legal holds or retention policies:

```python
# Legal hold — prevents deletion regardless
s3.put_object_legal_hold(Bucket='my-bucket', Key='file.txt', VersionId='...',
    LegalHold={'Status': 'ON'})

# Retention — prevents deletion until date
s3.put_object_retention(Bucket='my-bucket', Key='file.txt', VersionId='...',
    Retention={'Mode': 'COMPLIANCE', 'RetainUntilDate': '2030-01-01T00:00:00Z'})
```

### Lifecycle Rules

Auto-delete objects after a specified number of days:

```python
s3.put_bucket_lifecycle_configuration(Bucket='my-bucket',
    LifecycleConfiguration={
        'Rules': [{
            'ID': 'expire-logs',
            'Expiration': {'Days': 30},
            'Filter': {'Prefix': 'logs/'},
            'Status': 'Enabled',
        }]
    })
```

Abort incomplete multipart uploads (from killed or failed clients) after a number of days, reclaiming the uploaded parts. A rule may specify only this action, with no object expiration:

```python
s3.put_bucket_lifecycle_configuration(Bucket='my-bucket',
    LifecycleConfiguration={
        'Rules': [{
            'ID': 'abort-stale-uploads',
            'AbortIncompleteMultipartUpload': {'DaysAfterInitiation': 7},
            'Filter': {'Prefix': ''},
            'Status': 'Enabled',
        }]
    })
```

The background worker scans periodically (configurable interval, default 1 hour) and deletes expired objects and aborts stale multipart uploads (removing both their metadata and their part files on disk). Locked objects (legal hold or retention) are skipped.

### Compression

Enable zstd compression to reduce storage usage:

```yaml
compression:
  enabled: true
```

All objects are transparently compressed (zstd) on write and decompressed on read. Objects written by older gzip builds are still read correctly.

**Compression currently has no effect when encryption at rest is enabled.** Encryption wraps compression, so the compressor is handed ciphertext, which does not compress: measured on a 1.12 MB highly repetitive payload with both enabled, the stored object is 1.00x the plaintext. Enabling both costs CPU for no saving. Pick one until this is addressed.

Both directions stream, so a large object costs a compression window rather than a copy of itself: peak memory scales with concurrency, not with concurrency multiplied by object size. An upload that does not declare its length falls back to buffering, because the decompressed size has to be recorded in the frame header for reads to stream.

### Small-file packing (experimental)

For workloads with huge numbers of tiny objects, packing stores small objects as
independent zstd frames inside large append-only **volume** files (with byte-offset
locations in BoltDB) instead of one file per object, avoiding per-file overhead.
Objects larger than `max_object_size` are stored as individual files as usual.

```yaml
packing:
  enabled: true
  max_object_size: 1048576       # objects this size (bytes) or smaller are packed
  volume_max_size: 1073741824    # roll to a new volume past this size
  compact_interval_hours: 24     # background dead-space reclamation; 0 = disabled
  compact_min_dead_ratio: 0.5    # compact a volume once half of it is dead space
```

Deleted/overwritten objects leave dead space in volumes. It is reclaimed by
background compaction (or on demand via `POST /api/v1/compact`). Packing is
**experimental** and does not yet compose with encryption or erasure coding (it is
skipped, with a warning, if either is enabled).

### Access Logging

Enable structured JSON access logs:

```yaml
logging:
  enabled: true
  file_path: "./access.log"
```

Each S3 operation is logged as a JSON line with timestamp, method, bucket, key, status code, bytes, and client IP.

### Static Website Hosting

Serve static websites directly from buckets:

```python
s3.put_bucket_website(Bucket='my-site',
    WebsiteConfiguration={
        'IndexDocument': {'Suffix': 'index.html'},
        'ErrorDocument': {'Key': 'error.html'}
    })
```

Website-enabled buckets serve `index.html` for directory paths and a custom error page for missing objects. No authentication required for GET/HEAD requests.

### IAM (Users, Groups & Policies)

Fine-grained access control with S3-compatible IAM policies. `Action` and `Resource` accept both the bare-string form (`"Action": "s3:GetObject"`) and the array form, as AWS does, so policies copied from AWS documentation work unchanged. Operations on a specific object version are authorized as their own actions, `s3:DeleteObjectVersion` and `s3:GetObjectVersion`, so allowing `s3:DeleteObject` while denying `s3:DeleteObjectVersion` lets people delete recoverably without ever permanently destroying a version. Multi-object deletes are authorized per entry against the same rules, so the batch route is not a way around them:

```python
import requests, json

API = "http://localhost:9000/api/v1"
headers = {"Authorization": "Bearer <jwt-token>", "Content-Type": "application/json"}

# Create an IAM user
requests.post(f"{API}/iam/users", headers=headers, json={"name": "alice"})

# Attach a built-in policy (ReadOnlyAccess, ReadWriteAccess, FullAccess)
requests.post(f"{API}/iam/users/alice/policies", headers=headers,
    json={"policyName": "ReadOnlyAccess"})

# Create an access key for the user
resp = requests.post(f"{API}/keys", headers=headers, json={"userId": "alice"})
key = resp.json()  # {"accessKey": "...", "secretKey": "..."}

# Create groups and attach policies
requests.post(f"{API}/iam/groups", headers=headers, json={"name": "developers"})
requests.post(f"{API}/iam/groups/developers/policies", headers=headers,
    json={"policyName": "ReadWriteAccess"})

# Add user to group
requests.post(f"{API}/iam/users/alice/groups", headers=headers,
    json={"groupName": "developers"})

# Create custom policies
custom_policy = json.dumps({
    "Version": "2012-10-17",
    "Statement": [{
        "Effect": "Allow",
        "Action": ["s3:GetObject"],
        "Resource": ["arn:aws:s3:::my-bucket/*"]
    }]
})
requests.post(f"{API}/iam/policies", headers=headers,
    json={"name": "MyBucketReadOnly", "document": custom_policy})
```

Policy evaluation follows AWS IAM semantics: default deny, explicit Allow required, explicit Deny always wins. Admin keys and legacy keys (without a user) retain full access.

### CORS per Bucket

Configure Cross-Origin Resource Sharing on a per-bucket basis:

```python
s3.put_bucket_cors(Bucket='my-bucket', CORSConfiguration={
    'CORSRules': [{
        'AllowedOrigins': ['https://example.com'],
        'AllowedMethods': ['GET', 'PUT'],
        'AllowedHeaders': ['*'],
        'MaxAgeSeconds': 3600,
    }]
})
```

The server responds to `OPTIONS` preflight requests with the configured CORS headers. Unknown origins are rejected with 403.

### STS Temporary Credentials

Issue short-lived access keys for temporary access:

```python
import requests, boto3

API = "http://localhost:9000/api/v1"
headers = {"Authorization": "Bearer <jwt-token>", "Content-Type": "application/json"}

# Create temporary credentials for an IAM user (max 12 hours)
resp = requests.post(f"{API}/sts/session-token", headers=headers,
    json={"durationSecs": 3600, "userId": "alice"})
creds = resp.json()  # {"accessKey", "secretKey", "sessionToken", "expiration"}

# Use temporary credentials with any S3 client
s3 = boto3.client("s3", endpoint_url="http://localhost:9000",
    aws_access_key_id=creds["accessKey"],
    aws_secret_access_key=creds["secretKey"])
```

Temporary keys inherit the IAM user's policies. Expired keys are automatically cleaned up by the lifecycle worker.

### Audit Trail

Query the persistent audit log of all S3 operations:

```python
# List recent audit entries
requests.get(f"{API}/audit?limit=50", headers=headers)

# Filter by user, time range, or bucket
requests.get(f"{API}/audit?user=alice&limit=10", headers=headers)
requests.get(f"{API}/audit?from=1700000000&to=1700100000", headers=headers)
requests.get(f"{API}/audit?bucket=my-bucket", headers=headers)
```

Each entry records: timestamp, principal, user ID, action, resource, effect (Allow/Deny), source IP, and status code. Old entries are automatically pruned based on `security.audit_retention_days`.

### IP Restrictions

Control access by IP address at global or per-user level:

```yaml
# Global restrictions in config
security:
  ip_allowlist: ["10.0.0.0/8", "192.168.0.0/16"]  # empty = allow all
  ip_blocklist: ["10.0.0.99/32"]  # deny always wins
```

```python
# Per-user IP restrictions via API
requests.put(f"{API}/iam/users/alice/ip-restrictions", headers=headers,
    json={"allowedCidrs": ["10.0.0.0/8", "::1/128"]})

# Clear restrictions (allow from anywhere)
requests.put(f"{API}/iam/users/alice/ip-restrictions", headers=headers,
    json={"allowedCidrs": []})
```

Evaluation order: global blocklist (deny wins) → global allowlist → per-user allowlist. Admin keys are exempt from IP restrictions. Supports both IPv4 and IPv6 CIDR notation.

### S3 Event Notifications

Configure webhooks on buckets to receive notifications when objects are created or deleted:

```python
from botocore.auth import SigV4Auth
from botocore.credentials import Credentials
from botocore.awsrequest import AWSRequest
import requests

# PUT notification configuration (S3-compatible XML)
notif_xml = b"""<?xml version="1.0" encoding="UTF-8"?>
<NotificationConfiguration>
  <TopicConfiguration>
    <Id>my-webhook</Id>
    <Topic>https://example.com/webhook</Topic>
    <Event>s3:ObjectCreated:*</Event>
    <Event>s3:ObjectRemoved:*</Event>
    <Filter>
      <S3Key>
        <FilterRule>
          <Name>prefix</Name>
          <Value>images/</Value>
        </FilterRule>
      </S3Key>
    </Filter>
  </TopicConfiguration>
</NotificationConfiguration>"""

# Sign and send (using botocore for SigV4)
url = "http://localhost:9000/my-bucket?notification"
creds = Credentials("vaults3-admin", "vaults3-secret-change-me")
req = AWSRequest(method="PUT", url=url, data=notif_xml,
    headers={"Content-Type": "application/xml"})
SigV4Auth(creds, "s3", "us-east-1").add_auth(req)
requests.put(url, headers=dict(req.headers), data=notif_xml)
```

Supported events: `s3:ObjectCreated:Put`, `s3:ObjectCreated:Copy`, `s3:ObjectCreated:CompleteMultipartUpload`, `s3:ObjectRemoved:Delete`. Use wildcards like `s3:ObjectCreated:*`. Webhook payloads follow the AWS S3 event notification JSON format.

Configure webhook delivery in `configs/vaults3.yaml`:

```yaml
notifications:
  max_workers: 4       # concurrent webhook delivery goroutines
  queue_size: 256      # buffered event queue size
  timeout_secs: 10     # webhook HTTP timeout
  max_retries: 3       # retry attempts for failed webhooks
  kafka:
    enabled: true
    brokers: ["localhost:9092"]
    topic: "vaults3-events"
  nats:
    enabled: true
    url: "nats://localhost:4222"
    subject: "vaults3.events"
  redis:
    enabled: true
    addr: "localhost:6379"
    channel: "vaults3-events"   # pub/sub mode
    list_key: ""                # set for LPUSH queue mode
  amqp:
    enabled: true
    url: "amqp://guest:guest@localhost:5672/"
    exchange: "vaults3-events"
    routing_key: "s3.events"
  postgres:
    enabled: true
    dsn: "postgres://user:pass@localhost:5432/vaults3?sslmode=disable"
    table: "s3_events"
  elasticsearch:
    enabled: true
    urls: ["http://localhost:9200"]
    index: "vaults3-events"
```

Additional backends: **AMQP/RabbitMQ** (publish to exchanges), **PostgreSQL** (insert into table), **Elasticsearch** (index events). In addition to per-bucket webhooks, you can enable global notification backends. All S3 events are published to every enabled backend. Multiple backends can be active simultaneously. Disabled backends add zero overhead.

### Async Replication

Replicate objects to a peer VaultS3 instance automatically:

```yaml
replication:
  enabled: true
  peers:
    - name: "dc2"
      url: "http://peer-vaults3:9000"
      access_key: "peer-admin"
      secret_key: "peer-secret"
  scan_interval_secs: 30   # queue processing interval
  max_retries: 5           # retry before dead-letter
  batch_size: 100          # events per scan cycle
```

Objects created, copied, or deleted on the primary, whether through the S3 API **or the web dashboard**, are asynchronously pushed to all configured peers over the S3 protocol. Buckets are auto-created on peers. Failed deliveries retry with exponential backoff (5s, 15s, 45s, 135s, 405s). The `X-VaultS3-Replication` header prevents infinite loops. Monitor via dashboard API:

```bash
curl http://localhost:9000/api/v1/replication/status   # per-peer sync stats
curl http://localhost:9000/api/v1/replication/queue     # pending queue entries
```

For one-way push, replication only needs to be enabled on the **source**. The **target** does not need `replication.enabled`, it just needs the peer `access_key`/`secret_key` (from the source's config) to be valid credentials on it. Enable replication on both sides only for `mode: active-active`.

### CLI Tool

VaultS3 includes a standalone CLI binary (`vaults3-cli`) for managing the server:

```bash
# Set credentials via environment or flags
export VAULTS3_ENDPOINT=http://localhost:9000
export VAULTS3_ACCESS_KEY=vaults3-admin
export VAULTS3_SECRET_KEY=vaults3-secret-change-me

# Server version and storage capacity (used / free / total)
vaults3-cli info

# Bucket operations
vaults3-cli bucket list
vaults3-cli bucket create my-bucket
vaults3-cli bucket info my-bucket
vaults3-cli bucket delete my-bucket

# Object operations
vaults3-cli object put my-bucket docs/readme.md ./README.md
vaults3-cli object ls my-bucket                       # folder view (objects + prefixes)
vaults3-cli object ls my-bucket --prefix=docs/        # list inside a prefix
vaults3-cli object ls my-bucket --recursive           # all nested objects (paginates past 1000)
vaults3-cli object get my-bucket docs/readme.md ./downloaded.md
vaults3-cli object cp my-bucket/file.txt my-bucket/copy.txt
vaults3-cli object rm my-bucket docs/readme.md
vaults3-cli object presign my-bucket file.txt --expires=3600
vaults3-cli object verify my-bucket                   # find objects that list but cannot be read (metadata/data desync)
vaults3-cli object verify my-bucket --repair          # remove orphaned metadata for unreadable objects

# Storage maintenance
vaults3-cli storage reclaim                    # report data files no metadata refers to (dry run)
vaults3-cli storage reclaim --apply            # delete them and free the space

# IAM user operations
vaults3-cli user list
vaults3-cli user create alice --access-key=ak --secret-key=sk
vaults3-cli user attach-policy alice ReadWriteAccess
vaults3-cli user delete alice

# Replication monitoring
vaults3-cli replication status
vaults3-cli replication queue

# Cluster operations (see docs/SCALING.md)
vaults3-cli bucket durability scratch --erasure=off --replicas=1  # store this bucket once
vaults3-cli cluster status                     # members, leader, drain state
vaults3-cli cluster join node-3 10.0.0.4:7000  # add a member (against the leader)
vaults3-cli cluster drain node-2               # stop a node accepting writes (reads continue)
vaults3-cli cluster rebalance                  # move objects to their correct owner
vaults3-cli cluster decommission node-2        # guided drain + rebalance before replacing a node
vaults3-cli cluster shards                     # how object metadata is distributed across the cluster
```

Build both binaries with `make build` or just the CLI with `make cli`.

### Presigned Upload Restrictions

Generate presigned PUT URLs with server-enforced restrictions:

```python
import requests

API = "http://localhost:9000/api/v1"
headers = {"Authorization": "Bearer <jwt-token>", "Content-Type": "application/json"}

# Generate restricted presigned PUT URL
resp = requests.post(f"{API}/presign", headers=headers, json={
    "bucket": "uploads",
    "key": "images/photo.jpg",
    "method": "PUT",
    "expires": 3600,
    "maxSize": 10485760,               # 10MB max
    "allowTypes": "image/jpeg,image/png",  # only images
    "requirePrefix": "images/"         # must upload to images/
})
url = resp.json()["url"]

# Upload within restrictions — succeeds
requests.put(url, data=image_data, headers={"Content-Type": "image/jpeg"})

# Upload too large / wrong type / wrong prefix — 403 Forbidden
```

Restriction parameters (`X-Vault-MaxSize`, `X-Vault-AllowTypes`, `X-Vault-RequirePrefix`) are embedded in the signed URL and validated server-side.

### Full-Text Search

Search objects by key, content type, and tags across all buckets:

```bash
# Search by key substring
curl "http://localhost:9000/api/v1/search?q=readme" -H "Authorization: Bearer <token>"

# Search by content type
curl "http://localhost:9000/api/v1/search?q=type:image" -H "Authorization: Bearer <token>"

# Search by tag
curl "http://localhost:9000/api/v1/search?q=tag:project=vaults3" -H "Authorization: Bearer <token>"

# Filter by bucket and limit results
curl "http://localhost:9000/api/v1/search?q=docs&bucket=my-bucket&limit=10" -H "Authorization: Bearer <token>"
```

The search index is built on startup from BoltDB metadata and updated incrementally on every object put, delete, copy, and tag change. Supports plain text (substring match), `type:` prefix for content-type filtering, and `tag:key=value` for tag matching.

### Webhook Virus Scanning

Scan uploaded objects with an external virus scanner (ClamAV REST, VirusTotal, etc.):

```yaml
scanner:
  enabled: true
  webhook_url: "http://localhost:3310/scan"
  timeout_secs: 30
  quarantine_bucket: "vaults3-quarantine"
  fail_closed: false          # false=fail-open (keep file), true=quarantine on error
  max_scan_size_bytes: 104857600  # 100MB
  workers: 2
```

When enabled, every uploaded object is POSTed to the webhook URL as multipart/form-data. If the scanner returns 406/403 (infected), the object is moved to the quarantine bucket and deleted from the original. Monitor via dashboard API:

```bash
curl http://localhost:9000/api/v1/scanner/status       # queue depth + recent scans
curl http://localhost:9000/api/v1/scanner/quarantine    # quarantined objects
```

### Data Tiering

Automatically migrate infrequently accessed objects to a cold storage directory:

```yaml
tiering:
  enabled: true
  cold_data_dir: "./cold_data"
  migrate_after_days: 30
  scan_interval_secs: 3600
```

Objects not accessed for `migrate_after_days` are moved to the cold data directory. On read, cold objects are transparently served and promoted back to hot storage. Manual migration is available via API:

```bash
# Check tiering status (hot/cold counts and sizes)
curl http://localhost:9000/api/v1/tiering/status -H "Authorization: Bearer <token>"

# Manually migrate an object to cold tier
curl -X POST http://localhost:9000/api/v1/tiering/migrate \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"bucket":"my-bucket","key":"archive/old-file.zip","direction":"cold"}'
```

### Backup Scheduler

Schedule automatic backups to local directories:

```yaml
backup:
  enabled: true
  targets:
    - name: "local-backup"
      type: "local"
      path: "/backups/vaults3"
  schedule_cron: "0 2 * * *"   # daily at 2am
  retention_days: 30
  incremental: false            # true for incremental backups
```

Monitor and trigger backups via API:

```bash
# Check backup status
curl http://localhost:9000/api/v1/backups/status -H "Authorization: Bearer <token>"

# List backup history
curl http://localhost:9000/api/v1/backups -H "Authorization: Bearer <token>"

# Trigger immediate backup
curl -X POST http://localhost:9000/api/v1/backups/trigger -H "Authorization: Bearer <token>"
```

Incremental backups only copy objects modified since the last successful backup. Full backups mirror the complete object store.

### Git-like Versioning

Compare, tag, and rollback object versions:

```bash
# Diff two versions (text files show line-by-line diff, binary shows metadata only)
curl "http://localhost:9000/api/v1/versions/diff?bucket=my-bucket&key=file.txt&v1=VERSION_A&v2=VERSION_B" \
  -H "Authorization: Bearer <token>"

# Tag a version with a label
curl -X POST http://localhost:9000/api/v1/versions/tags \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"bucket":"my-bucket","key":"file.txt","versionId":"VERSION_ID","tag":"v1.0"}'

# List tags for an object
curl "http://localhost:9000/api/v1/versions/tags?bucket=my-bucket&key=file.txt" \
  -H "Authorization: Bearer <token>"

# Delete a tag
curl -X DELETE "http://localhost:9000/api/v1/versions/tags?bucket=my-bucket&key=file.txt&tag=v1.0" \
  -H "Authorization: Bearer <token>"

# Rollback to a specific version (copies old version content as latest)
curl -X POST http://localhost:9000/api/v1/versions/rollback \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"bucket":"my-bucket","key":"file.txt","versionId":"VERSION_ID"}'
```

Text diffs use LCS (Longest Common Subsequence) to produce unified diffs with add/remove/equal lines. Binary files show only size and metadata differences.

### FUSE Mount

Mount a VaultS3 bucket as a local filesystem directory:

```bash
# Mount a bucket (requires macFUSE on macOS or FUSE on Linux)
vaults3-cli mount my-bucket /mnt/vaults3

# Browse files
ls /mnt/vaults3
cat /mnt/vaults3/docs/readme.txt

# Write files (creates objects in VaultS3)
echo "hello" > /mnt/vaults3/new-file.txt

# Unmount
# Press Ctrl+C in the mount terminal, or:
fusermount -u /mnt/vaults3
```

FUSE mount uses range requests for lazy loading, only the requested bytes are fetched from the server. Write support buffers data and uploads on file close.

### S3 Select (SQL on Objects)

Execute SQL queries on CSV and JSON objects without downloading the full file:

```python
from botocore.auth import SigV4Auth
from botocore.credentials import Credentials
from botocore.awsrequest import AWSRequest
import requests

# Query a CSV file
url = "http://localhost:9000/my-bucket/data.csv?select&select-type=2"
body = b"""<?xml version="1.0"?>
<SelectObjectContentRequest>
    <Expression>SELECT name, age FROM s3object WHERE city = 'New York' AND age > '25'</Expression>
    <ExpressionType>SQL</ExpressionType>
    <InputSerialization><CSV><FileHeaderInfo>USE</FileHeaderInfo></CSV></InputSerialization>
    <OutputSerialization><JSON/></OutputSerialization>
</SelectObjectContentRequest>"""

creds = Credentials("vaults3-admin", "vaults3-secret-change-me")
req = AWSRequest(method="POST", url=url, data=body, headers={"Content-Type": "application/xml"})
SigV4Auth(creds, "s3", "us-east-1").add_auth(req)
r = requests.post(url, headers=dict(req.headers), data=body)
# Returns JSON lines: {"name":"Alice","age":"30"}\n{"name":"Charlie","age":"35"}
```

Supported SQL features:
- `SELECT *` or `SELECT col1, col2` (column projection)
- `FROM s3object` (required table name)
- `WHERE col = 'value'`, `!=`, `<`, `>`, `<=`, `>=` (comparisons, numeric-aware)
- `AND` / `OR` (logical operators)
- `LIKE 'pattern%'` (SQL wildcards: `%` = any chars, `_` = single char)
- `IS NULL` / `IS NOT NULL`
- `LIMIT N`
- Column references: `name`, `s3object.name`, `s.name`, `_1` (positional for CSV without headers)

Input formats: CSV (with/without headers, custom delimiters), JSON Lines, JSON Document (array), Parquet (columnar format via parquet-go).
Compressed input: GZIP and BZIP2 compressed CSV/JSON files are transparently decompressed before query execution.
Output formats: JSON (one object per line) or CSV.

### Bucket Default Retention

Set default object retention on a versioned bucket, all new objects automatically inherit the retention policy:

```python
from botocore.auth import SigV4Auth
from botocore.credentials import Credentials
from botocore.awsrequest import AWSRequest
import requests

# Set default retention (requires versioning enabled)
url = "http://localhost:9000/my-bucket?object-lock"
body = b"""<?xml version="1.0" encoding="UTF-8"?>
<ObjectLockConfiguration>
  <Rule>
    <DefaultRetention>
      <Mode>GOVERNANCE</Mode>
      <Days>30</Days>
    </DefaultRetention>
  </Rule>
</ObjectLockConfiguration>"""

creds = Credentials("vaults3-admin", "vaults3-secret-change-me")
req = AWSRequest(method="PUT", url=url, data=body, headers={"Content-Type": "application/xml"})
SigV4Auth(creds, "s3", "us-east-1").add_auth(req)
requests.put(url, headers=dict(req.headers), data=body)

# All new objects now get 30-day GOVERNANCE retention automatically
s3.put_object(Bucket='my-bucket', Key='file.txt', Body=b'protected')
# file.txt cannot be deleted for 30 days
```

Modes: `GOVERNANCE` (admin can bypass with special header) or `COMPLIANCE` (nobody can shorten/remove until expiry). Requires versioning to be enabled on the bucket.

### Per-Bucket Prometheus Metrics

The `/metrics` endpoint includes per-bucket counters (limited to top 100 buckets):

```
vaults3_bucket_requests_total{bucket="my-bucket",method="PUT"} 42
vaults3_bucket_requests_total{bucket="my-bucket",method="GET"} 156
vaults3_bucket_bytes_in_total{bucket="my-bucket"} 10485760
vaults3_bucket_bytes_out_total{bucket="my-bucket"} 52428800
vaults3_bucket_errors_total{bucket="my-bucket"} 3
```

These complement the existing global metrics and per-bucket storage metrics, enabling monitoring and alerting per bucket.

### Rate Limiting

Protect against abuse and DDoS with token bucket rate limiting:

```yaml
rate_limit:
  enabled: true
  requests_per_sec: 100   # per client IP
  burst_size: 200
  per_key_rps: 50         # per access key
  per_key_burst: 100
```

When enabled, each client IP and access key gets an independent token bucket. Requests exceeding the limit receive `429 Too Many Requests` with a `Retry-After: 1` header. Stale buckets are cleaned up after 5 minutes of inactivity.

```bash
# Check rate limiter status
curl http://localhost:9000/api/v1/ratelimit/status -H "Authorization: Bearer <token>"
```

### Test with mc (MinIO Client)

```bash
mc alias set vaults3 http://localhost:9000 vaults3-admin vaults3-secret-change-me
mc mb vaults3/my-bucket
mc cp file.txt vaults3/my-bucket/
mc ls vaults3/my-bucket/
mc cat vaults3/my-bucket/file.txt
```

## Security

VaultS3 is designed with security in mind:

- **S3 Signature V4**: full signature verification including presigned URLs
- **Presigned URL validation**: signature, expiry, and restrictions enforced server-side
- **Constant-time credential comparison**: `crypto/hmac.Equal` prevents timing attacks on login
- **Path traversal protection**: `..` segments rejected at S3, API, versioning API, CopyObject/UploadPartCopy source, and filesystem layers
- **SSRF prevention**: webhook, lambda, and notification URLs blocked from targeting localhost, private IPs, and cloud metadata endpoints
- **Upload size limits**: 5GB per PUT (S3 spec), enforced with `http.MaxBytesReader`
- **Rate limiting**: per-IP token bucket using `RemoteAddr` (not spoofable via `X-Forwarded-For`)
- **AES-256-GCM encryption at rest**: SSE-S3 (static key) and SSE-KMS (HashiCorp Vault / local key) modes
- **IAM with default-deny**: policy evaluation engine with wildcard matching
- **Security headers**: CSP, HSTS, X-Frame-Options, X-Content-Type-Options, Referrer-Policy
- **Non-root Docker**: container runs as `vaults3` user (UID 1000)
- **Default credential warning**: startup log warns if admin credentials haven't been changed
- **Error message sanitization**: OIDC and health check errors return generic messages, preventing internal detail leaking
- **Race condition safety**: Replication handler creates per-request struct copy instead of mutating shared state
- **UploadID validation**: Hex-only regex validation prevents path traversal via crafted multipart upload IDs
- **Bounded request bodies**: All JSON API endpoints use `readJSON()` with 1MB `io.LimitReader`. Bucket policy body capped at 1MB
- **OIDC SSRF prevention**: Issuer URL validated against loopback, private, and link-local addresses before JWKS discovery
- **IPv6-safe rate limiting**: Uses `net.SplitHostPort` for correct IP extraction from IPv6 `[::1]:port` addresses
- **OIDC authorization layer**: Dashboard admin routes (IAM, keys, STS, audit, settings, lambda, backups) restricted to admin user. OIDC users get read-only access
- **Chunked encryption**: encrypted objects are sealed a chunk at a time (AES-256-GCM per chunk, nonce bound to chunk index and end-of-stream), so a read is authenticated before any byte is served and costs a chunk of memory rather than a copy of the object. The 1GB cap now applies only to objects still stored in the pre-4.4.53 whole-object format
- **Compression size cap**: 1GB max decompressed size prevents decompression-bomb DoS (gzip/zstd)
- **Version path traversal protection**: `versionId` parameter validated against directory escape in version storage
- **BatchDelete lock enforcement**: Batch delete respects WORM/legal-hold and validates keys against path traversal
- **SigV4 timestamp validation**: Requests with `X-Amz-Date` skewed more than 15 minutes are rejected (prevents replay)
- **Presigned URL expiry cap**: Maximum 7 days (604800 seconds), matching AWS behavior
- **Atomic file writes**: PutObject writes to temp file then renames, preventing corruption from concurrent writes
- **Backup scheduler thread safety**: Atomic bool prevents concurrent backup races
- **OIDC admin name reservation**: OIDC users cannot claim the "admin" username
- **OIDC domain validation enforcement**: Tokens without email are rejected when domain filtering is enabled
- **OIDC code flow hardening**: The authorization code is redeemed server-side, so the ID token never travels through the browser. The PKCE verifier, nonce and client secret stay on the server; the CSRF state is sealed with AES-GCM and expires after 15 minutes, so it cannot be read, forged, or replayed from another deployment. The ID token's nonce is checked against the login that requested it
- **CORS port restriction**: Localhost CORS only allowed on the server's own port
- **Presigned URL credential isolation**: Presigned URLs use a dedicated non-admin key, preventing privilege escalation
- **CORS Host header protection**: Origin validation uses configured server address, not attacker-controlled Host header
- **Admin-only route expansion**: Backup, replication, scanner, and tiering endpoints restricted to admin users
- **Replication queue limit cap**: Queue listing capped at 1000 entries to prevent memory exhaustion
- **Backup path traversal protection**: Backup target validates resolved paths stay within base directory
- **LIKE pattern O(n*m) matching**, Iterative DP algorithm replaces recursive backtracking, preventing ReDoS
- **Tiering promotion safety**: Async cold-to-hot promotion re-checks tier state and orders operations safely
- **Lambda output key validation**: Output key template expansion validated against path traversal
- **S3 Select record cap**: JSON/CSV parsing capped at 1M records to prevent memory exhaustion
- **FUSE cache size caps**: Signature cache, HEAD cache, and LIST cache bounded to prevent unbounded memory growth
- **GetObjectAttributes version support**: Respects `versionId` parameter and handles delete markers
- **LDAP authentication**: Bind-based LDAP/LDAPS with group-to-policy mapping and TLS support
- **External auth webhook**: Delegate authentication to an external HTTP endpoint with request validation
- **KMS envelope encryption**: HashiCorp Vault and local key provider for data encryption key management
- **Auto-TLS**: Automatic Let's Encrypt certificate provisioning with self-signed fallback
- **PROXY protocol v1**: Real client IP extraction behind PROXY protocol-aware load balancers
- **Governance bypass protection**: `x-amz-bypass-governance-retention` restricted to authorized principals
- **IAM policy conditions**: `StringEquals`, `StringLike`, `IpAddress`, `DateLessThan` condition evaluation
- **Bucket bandwidth throttling**: Per-bucket upload/download rate limits prevent resource monopolization
- **POST policy validation**: HTML form upload policies validated for expiration, conditions, and signature
- **Content-MD5 validation**: Server-side integrity verification on PUT rejects corrupted uploads
- **S3 Checksum API**: CRC32, CRC32C, SHA1, SHA256 checksums verified on upload and returned on download
- **Conditional request handling**: `If-Match`/`If-None-Match` ETag checks prevent lost updates (412 Precondition Failed)
- **Dependency hygiene**: Dashboard dependencies kept current against Dependabot advisories (latest: `react-router` 7.18.1 and `postcss` 8.5.24, closing a backslash open redirect in `<Link>`/`useNavigate`, an unauthenticated route-matching DoS, an SSR hydration constructor injection, an RSCErrorHandler XSS, and a postcss path traversal; earlier: `react-router` 7.17.0 closing 6 alerts, turbo-stream RCE, RSC/Location XSS, `__manifest`/single-fetch DoS, protocol-relative open redirect). The one advisory left open is a **React Server Components CSRF bypass** that is only patched in `react-router` 8.x: the dashboard is a client-rendered SPA and never uses RSC mode, so it is not affected, and 8.x would additionally require Node 22.22+ and dropping `react-router-dom`

See [SECURITY.md](SECURITY.md) for vulnerability reporting policy and deployment best practices.

## Project Structure

```
VaultS3/
├── cmd/vaults3/main.go        — Server entry point
├── cmd/vaults3-cli/           — CLI tool (bucket, object, user, replication, cluster commands)
├── internal/
│   ├── config/                — YAML config loader
│   ├── server/                — HTTP server, routing, and auto-TLS
│   ├── s3/                    — S3 API handlers (auth, buckets, objects, multipart, checksums, preconditions, replication config, restore, POST upload, snowball)
│   ├── storage/               — Storage engine interface + filesystem + encryption + KMS + storage classes
│   ├── metadata/              — BoltDB metadata store
│   ├── metrics/               — Prometheus-compatible metrics collector
│   ├── iam/                   — IAM policy engine, identity, IP access control, conditions, LDAP, external auth, STS AssumeRole
│   ├── notify/                — Event notification dispatcher (webhook, Kafka, NATS, Redis, AMQP, PostgreSQL, Elasticsearch)
│   ├── replication/           — Async + active-active replication (SigV4 signer, queue processor, vector-clock conflict resolution)
│   ├── erasure/               — Reed-Solomon erasure coding + background healer (multi-disk shard placement)
│   ├── cluster/               — Raft metadata, consistent-hash ring, failure detector, failover proxy, rebalancer
│   ├── search/                — In-memory full-text search index
│   ├── vector/                — Optional vector store: cosine kNN index + OpenAI-compatible embedder (semantic search / RAG)
│   ├── migrate/               — Import from any S3-compatible source (MinIO/AWS/...): SigV4 source client + async migrator
│   ├── snapshot/              — Bucket snapshots ("git-for-buckets"): commit / diff / restore on version pointers
│   ├── scanner/               — Webhook virus scanning with quarantine
│   ├── ratelimit/             — Token bucket rate limiter (per IP, per key, per bucket bandwidth)
│   ├── tiering/               — Hot/cold data tiering manager + remote S3-compatible tier
│   ├── backup/                — Backup scheduler with local targets
│   ├── versioning/            — Version diff (LCS), tagging, rollback
│   ├── fuse/                  — FUSE filesystem mount (go-fuse/v2)
│   ├── middleware/             — HTTP middleware (request ID, panic recovery, latency, security headers, PROXY protocol)
│   ├── api/                   — Dashboard REST API (JWT auth, IAM, STS, audit, events, logs, trace, diagnostics, heal, speedtest)
│   ├── batch/                 — Batch operations processor (bulk delete/copy)
│   ├── inventory/             — S3 Inventory report generator (periodic CSV)
│   └── dashboard/             — Embedded React SPA
├── web/                       — React dashboard source (Vite + Tailwind)
├── configs/vaults3.yaml       — Default configuration
├── Makefile                   — Build commands
├── Dockerfile                 — Multi-stage Docker build
└── README.md
```

## Tech Stack

- **Go**: net/http (no frameworks)
- **React 19**: Dashboard UI (embedded via `//go:embed`)
- **Tailwind CSS**: Dashboard styling
- **BoltDB**: Embedded key-value store for metadata
- **Local filesystem**: Object storage backend
- **AES-256-GCM**: Server-side encryption (SSE-S3 and SSE-KMS with HashiCorp Vault)

## Requirements

- Go 1.26+ (build)
- Node.js 20.19+ (dashboard build only, Vite 8 / Rolldown)
- No runtime dependencies

## Updating

VaultS3 can check GitHub Releases once a day and show a **dashboard banner** when
a newer version is out. Updates only ever replace the binary or image, your
object data, metadata, and config are never touched.

**Docker (recommended): [Watchtower](https://containrrr.dev/watchtower/)** watches
for a new image and recreates the container. Your data volumes are preserved:

```yaml
services:
  vaults3:
    image: eniz1806/vaults3:latest
    volumes: [vaults3-data:/data, vaults3-meta:/metadata]
  watchtower:
    image: containrrr/watchtower
    volumes: [/var/run/docker.sock:/var/run/docker.sock]
    command: --interval 86400   # check daily
```

**Binary / systemd:** enable the built-in updater in `vaults3.yaml`. With
`apply: true` it downloads the new release for your platform, **verifies its
SHA-256 checksum**, swaps the binary, and restarts into the new version
(checked daily. Never auto-crosses a major version):

```yaml
auto_update:
  enabled: true     # daily check + dashboard banner
  apply: true       # also install automatically (omit for notify-only)
```

The current/latest version is also exposed at `GET /api/v1/version`.

### Upgrading to 4.4.56 (security release)

**This release closes 14 findings from an external security assessment, several
of them remotely exploitable against a default deployment.** The full list is in
[CHANGELOG.md](CHANGELOG.md). Upgrading is strongly recommended, and a few things
change behaviour, so read this first.

#### Before you upgrade

**Set `cluster.secret` on every node of a clustered deployment.** This is the one
change that stops a server booting. Inter-node endpoints authenticate with it and
now fail closed, so a clustered node with no secret exits at startup with an
error naming the setting. Use the same value on every node, ideally from a secret
manager. The Helm chart already derives one, so chart users need do nothing.

```yaml
cluster:
  enabled: true
  secret: "a-shared-value"      # or VAULTS3_CLUSTER_SECRET
```

Single-node deployments are unaffected.

#### After you upgrade

**Rotate the admin credentials if this installation ever ran with
`vaults3-secret-change-me`.** 4.4.55 stopped shipping that secret, but an
installation that already booted with it has it persisted, and persisted
credentials win over configuration, so upgrading does not replace it. Change it
from the dashboard, or set `VAULTS3_ACCESS_KEY` and `VAULTS3_SECRET_KEY`.

**Everyone is logged out once.** The console signing key is now random per
installation instead of derived from the admin secret, so existing dashboard
sessions stop working. Users log in again. Nothing else is affected.

#### If something stops working, this is probably why

Each of these was a security fix, and each can look like a regression.

| Symptom | Cause | What to do |
|---|---|---|
| A non-admin dashboard user gets 403 on a bucket | The console now enforces IAM policies, as the S3 API always did. Any authenticated user used to reach any bucket | Give the user a policy covering the buckets they need |
| OIDC login fails | The implicit flow is disabled. The authorization-code flow, which the dashboard uses, is unaffected | Use the code flow, or set `oidc.allow_implicit_flow: true` if your provider supports nothing newer |
| Per-bucket panels in Prometheus go blank | Anonymous scrapes no longer receive the per-bucket series, which carry bucket names, sizes and counts | Send `X-Cluster-Secret` with the scrape, or set `metrics.public_bucket_labels: true` |
| A migration from an internal source fails | Loopback, private and link-local destinations are blocked by default, because a caller-supplied endpoint was a server-side request primitive | Re-run the job with private sources allowed |
| An STS credential has less access than before | Session policies are now enforced. A scoped session used to inherit the full permissions of the user it came from | Widen the session policy if the access was intended |
| An STS request returns 403 | `X-Amz-Security-Token` is now verified. Standard SDKs send it automatically | Send the session token that was issued with the key |
| A copy returns 403 | A copy now requires `s3:GetObject` on its source, not only write on the destination | Grant read on the source bucket |
| An IAM policy now denies what it used to allow | `Condition`, `NotAction` and `NotResource` are now evaluated. They used to be ignored, so a restriction you wrote was not being applied | The policy is now doing what it says. Adjust it if the restriction was not intended |
| Automation gets 429 on login | Ten failed logins from one address earn a fifteen-minute lockout | Fix the credentials the automation is using |

### Upgrading to 4.4.55

**A server that has never had an admin secret now generates one** rather than
falling back to the example secret from these docs. If your installation already
has credentials, whether persisted, configured, or set from the dashboard,
nothing changes: those still win. Only a genuinely new installation gets a
generated secret, which it prints once at startup and then stores.

If you were relying on `vaults3-secret-change-me`, set `VAULTS3_ACCESS_KEY` and
`VAULTS3_SECRET_KEY` explicitly, or read the generated secret from the first
start's output.

### Upgrading to 4.4.54

One behaviour change is worth knowing before you upgrade, because it is visible
in your storage numbers:

**On a versioning-enabled bucket, a multi-object delete now writes a delete
marker and keeps the data**, which is what a single `DELETE` has always done and
what S3 specifies. Before this it removed the object outright, so a bulk delete
freed space. After upgrading it will not, and the space is released when the
versions are expired, either by a lifecycle rule (`NoncurrentVersionExpiration`)
or by deleting versions explicitly. Buckets without versioning are unaffected.

Nothing else needs action. Metadata sharding is off unless you set
`cluster.metadata_shards` above 1, and the server refuses to start if you set it
on a node whose metadata store already holds objects, so an upgrade cannot enable
it by accident.

## Contributing

Contributions are welcome! See **[CONTRIBUTING.md](CONTRIBUTING.md)** for build,
test, and PR guidelines, and **[CHANGELOG.md](CHANGELOG.md)** for release notes.

```bash
make build                      # React dashboard + server + CLI binaries
go test ./...                   # run the Go test suite
go test -race ./internal/erasure/ ./internal/cluster/ ./internal/replication/
```

The data-durability subsystems (`erasure`, `cluster`, `replication`) carry
fault-injection tests, corrupt a shard and heal it, lose a node and re-route,
partition two sites and resolve the conflict. New logic there should keep that bar.

## Roadmap

- [x] Core S3 CRUD operations
- [x] S3 Signature V4 authentication
- [x] Presigned URLs
- [x] Content-Type detection and storage
- [x] Range requests (partial GET)
- [x] Copy object (same/cross-bucket)
- [x] Batch delete
- [x] Multipart upload (full lifecycle)
- [x] Multiple access keys
- [x] Object tagging
- [x] AES-256-GCM encryption at rest
- [x] Bucket policies (public-read, custom)
- [x] Quota management (per-bucket)
- [x] Virtual-hosted style URLs
- [x] Prometheus-compatible metrics
- [x] Web dashboard with built-in UI (login, bucket browser, file management, access keys, activity log, stats, dark/light theme, responsive)
- [x] Health check endpoints (/health, /ready)
- [x] Graceful shutdown (SIGTERM/SIGINT with configurable timeout)
- [x] TLS support (HTTPS with cert/key)
- [x] Docker image (multi-stage build with health check)
- [x] Object versioning (per-bucket, version IDs, delete markers, version-specific operations)
- [x] Object locking / WORM (legal hold, retention with GOVERNANCE/COMPLIANCE modes)
- [x] Lifecycle rules (per-bucket expiration with background worker)
- [x] Gzip compression (transparent compress/decompress)
- [x] Access logging (structured JSON lines)
- [x] Static website hosting (index/error documents, no-auth serving)
- [x] IAM users, groups & policies (fine-grained access control, policy evaluation engine, built-in policies)
- [x] CORS per bucket (S3-compatible, OPTIONS preflight)
- [x] STS temporary credentials (short-lived keys, auto-cleanup, configurable max duration)
- [x] Audit trail (persistent log, filtering by user/bucket/time, auto-pruning)
- [x] IP allowlist/blocklist (global and per-user CIDR restrictions, IPv4/IPv6)
- [x] S3 event notifications (per-bucket webhooks, event type + prefix/suffix filtering, retry with backoff)
- [x] Async replication (one-way to peer VaultS3 instances, BoltDB queue, retry with exponential backoff, loop prevention)
- [x] CLI tool (`vaults3-cli`, bucket, object, user, replication, cluster management)
- [x] Presigned upload restrictions (max size, content type whitelist, key prefix enforcement)
- [x] Full-text search (in-memory index over keys, content types, tags. `GET /api/v1/search`)
- [x] Webhook virus scanning (ClamAV/VirusTotal integration, quarantine bucket, fail-open/closed modes)
- [x] Data tiering (hot/cold storage, automatic migration based on access patterns, transparent reads, manual migration API)
- [x] Backup scheduler (full/incremental backups to local targets, cron scheduling, backup history, trigger API)
- [x] Git-like versioning (visual diff with LCS, version tagging with labels, one-click rollback)
- [x] FUSE mount (mount buckets as local filesystem, read/write, lazy loading via range requests)
- [x] Rate limiting (token bucket per IP and per access key, 429 responses, auto-cleanup)
- [x] UploadPartCopy (copy byte ranges from existing objects as multipart parts)
- [x] S3 Select (SQL queries on CSV and JSON objects, SELECT/WHERE/LIMIT/LIKE/AND/OR)
- [x] Multi-backend notifications (Kafka, NATS, Redis pub/sub and queue backends)
- [x] Bucket default retention (auto-apply GOVERNANCE/COMPLIANCE retention to new objects)
- [x] Per-bucket Prometheus metrics (request counts, bytes in/out, errors by bucket label)
- [x] OIDC/JWT SSO (dashboard login via Google/Keycloak/Auth0, RS256 JWKS verification, domain filtering, auto-create users, role mapping)
- [x] Lambda compute triggers (webhook functions on S3 events, event/key filtering, optional body inclusion, output storage, worker pool)
- [x] FUSE read cache (LRU block cache, metadata TTL cache, kernel attribute caching, SigV4 key caching)
- [x] RAM optimization (slim search index with LRU cap, batched last-access writes, GOMEMLIMIT support)
- [x] Dashboard advanced pages (IAM users/groups/policies, audit trail, search, notifications, replication, lambda triggers, backups, 7 new pages with full CRUD)
- [x] GetBucketLocation, Bucket Tagging, Bucket/Object ACL, ListMultipartUploads, ListParts (6 new S3 operations for AWS CLI/SDK compatibility)
- [x] Structured logging with slog (key-value pairs, configurable log level)
- [x] Request ID middleware (X-Request-Id header on every response)
- [x] Panic recovery middleware (stack trace logging, graceful 500 responses)
- [x] Request latency histogram (Prometheus-compatible `vaults3_request_duration_seconds_bucket`)
- [x] Go unit tests for 14 packages (metadata, storage, IAM, ratelimit, search, config, middleware, server, lifecycle, S3 handler, API, versioning, notify), 133 tests
- [x] Makefile targets: `make test-coverage`, `make lint`
- [x] Dashboard polish: sortable table columns on all list pages (files, buckets, keys, audit, search)
- [x] Dashboard polish: client-side pagination (50 items/page) on file browser and audit trail
- [x] Dashboard polish: file preview panel (images, text files, JSON) with metadata sidebar
- [x] Dashboard polish: bucket config UI (versioning toggle, lifecycle rule editor, CORS config editor)
- [x] Dashboard API: versioning, lifecycle, and CORS endpoints for bucket settings management
- [x] SVG dashboard charts: BarChart (per-bucket sizes), DonutChart (request method distribution), Sparkline (activity)
- [x] GitHub Actions CI: build, test, lint, coverage on push/PR
- [x] pprof debug endpoint: `/debug/pprof/*` behind `debug: true` config flag
- [x] Extended stats API with request metrics (total requests, errors, bytes in/out, requests by method)
- [x] Dashboard bulk operations: multi-select with bulk delete and bulk zip download
- [x] Dashboard drag-and-drop folder upload (recursive directory reading via webkitGetAsEntry)
- [x] Dashboard auto-refresh toggle on stats page (30s interval, localStorage persistence)
- [x] Dashboard copy-to-clipboard buttons for S3 URIs, object keys, and access keys
- [x] Dashboard API: bulk-delete and download-zip endpoints
- [x] Dashboard home page with summary cards, activity sparkline, system stats, quick actions
- [x] Settings page: read-only server configuration viewer (`GET /api/v1/settings`, secrets stripped)
- [x] Keyboard shortcuts: `/` go to search, `?` shortcut help overlay, `Esc` close modal/blur
- [x] Breadcrumb navigation in file browser with home icon, chevron separators, clickable path segments
- [x] Security headers middleware (CSP, X-Frame-Options, X-Content-Type-Options, HSTS, Referrer-Policy)
- [x] CORS origin validation (same-origin + localhost, replaces wildcard)
- [x] Dashboard API rate limiting (429 Too Many Requests)
- [x] Input validation (DNS-compatible bucket names, object key constraints)
- [x] Object versioning dashboard UI (version list panel, diff viewer, rollback, version tagging, bucket versioning indicator)
- [x] Version list API (`GET /api/v1/versions?bucket=X&key=Y`)
- [x] Toast notification system (success/error/info toasts, bottom-right stack, auto-dismiss 5s)
- [x] Sortable table columns on all dashboard pages (notifications, backups, lambda triggers, replication queue)
- [x] Enhanced bucket creation wizard (optional versioning toggle + auto-expire lifecycle rule)
- [x] API tests (auth login/failure, JWT required, bucket CRUD, CORS preflight, input validation)
- [x] Versioning tests (LCS diff algorithm, text type detection, metadata diff, tag store CRUD)
- [x] Notify tests (dispatcher lifecycle, backend publish, webhook delivery, event/key filtering, retry)
- [x] Python integration test (security headers, CORS, auth, validation, bucket CRUD, versioning, settings)
- [x] User metadata (x-amz-meta-*) on PUT/GET/HEAD
- [x] Conditional GET/PUT (If-Modified-Since, If-None-Match, If-Match → 304/412)
- [x] Content-MD5 validation on PUT
- [x] Metadata-only copy (x-amz-metadata-directive: REPLACE)
- [x] Conditional copy (x-amz-copy-source-if-* headers)
- [x] Response header overrides (?response-content-type, etc.)
- [x] Inline tagging and retention on PUT
- [x] Canned ACL headers (x-amz-acl, x-amz-grant-*)
- [x] S3 Checksum API (CRC32, CRC32C, SHA1, SHA256)
- [x] Parts count header on HEAD (x-amz-mp-parts-count)
- [x] ListObjectsV1 (marker-based pagination)
- [x] ListBuckets with prefix filter
- [x] Versioning suspend support
- [x] GetObject by part number (?partNumber=N)
- [x] Advanced lifecycle rules (NoncurrentVersionExpiration, AbortIncompleteMultipartUpload, MaxNoncurrentVersions, ExpiredObjectDeleteMarker cleanup, object size filter)
- [x] IAM policy conditions (StringEquals, StringLike, IpAddress, DateLessThan)
- [x] Policy variables (${aws:username}, ${aws:userid})
- [x] LDAP authentication with group mapping
- [x] STS AssumeRole with session policies
- [x] External auth webhook
- [x] Access key description & status
- [x] Governance bypass (x-amz-bypass-governance-retention)
- [x] Per-bucket replication rules with prefix/tag filters
- [x] S3 Replication Config API (PUT/GET/DELETE /{bucket}?replication)
- [x] Real-time event-driven replication
- [x] Existing object replication
- [x] Delete marker replication
- [x] Site replication (IAM + bucket config sync)
- [x] KMS integration (HashiCorp Vault + local key provider)
- [x] Remote tiering (S3-compatible cold tier)
- [x] RestoreObject API (POST /{bucket}/{key}?restore)
- [x] Storage classes (STANDARD, REDUCED_REDUNDANCY)
- [x] Compression exclusions (skip already-compressed file types)
- [x] Real-time event streaming (SSE at /api/v1/events)
- [x] Real-time log streaming (SSE at /api/v1/logs)
- [x] Request tracing (SSE at /api/v1/trace)
- [x] Health diagnostics (/api/v1/diagnostics)
- [x] Manual heal API (POST /api/v1/heal)
- [x] Speedtest (POST /api/v1/speedtest)
- [x] Batch operations processor (bulk delete/copy)
- [x] PROXY protocol v1 support
- [x] Auto-TLS (Let's Encrypt + self-signed fallback)
- [x] Inter-node network separation
- [x] Bucket bandwidth throttling
- [x] S3 Select on compressed files (GZIP, BZIP2)
- [x] S3 POST policy (HTML form-based upload)
- [x] S3 Inventory reports (periodic CSV)
- [x] Snowball/TAR bulk upload
- [x] FIFO quota (delete oldest objects when quota exceeded)
- [x] AMQP/RabbitMQ notification backend (amqp091-go client with lazy connection and topic exchange)
- [x] PostgreSQL notification backend (lib/pq driver, auto-create table, JSONB storage)
- [x] Elasticsearch notification backend
- [x] SSE-KMS encryption (HashiCorp Vault Transit engine + local key provider, key rotation, per-object AES-256-GCM)
- [x] S3 Select on Parquet files (parquet-go, row group iteration, columnar to record conversion)
- [x] Integration test suite (26 end-to-end tests with real SigV4 signing, filesystem storage, BoltDB metadata)
- [x] Race detection in CI (`go test -race`)
- [x] `vaults3 setup`: interactive and scripted first-run configuration, with a generated per-installation admin secret instead of a published default
- [x] Sharded metadata (`cluster.metadata_shards`): object metadata split across independent Raft groups so metadata capacity grows with the cluster, with per-shard membership reconciliation, all groups sharing one Raft port, and an unreachable shard reported as unavailable rather than empty

## License

Copyright (C) 2026 Kodiqa Solutions.

VaultS3 is free software, licensed under the **GNU Affero General Public License
v3.0**. You may use, modify and redistribute it, including commercially, provided
you preserve the licence and copyright notices, state your changes, and release
the source of anything you derive from it under the same licence. The AGPL adds
one condition the GPL does not: if you run a modified version as a network
service, the users of that service are entitled to its source.

The full text is in [LICENSE](LICENSE), and [NOTICE](NOTICE) carries the
copyright statement. Separate paid add-ons are described under
[Project and support](#project-and-support) and are not covered by this licence.
