# VaultS3

Lightweight S3-compatible object storage. Single binary, <80MB RAM, built-in dashboard.

## Quick Start

```bash
docker run -d \
  --name vaults3 \
  -p 9000:9000 \
  --stop-timeout 35 \
  -e VAULTS3_ACCESS_KEY=myadmin \
  -e VAULTS3_SECRET_KEY=mysupersecretkey123 \
  -v vaults3-data:/data \
  -v vaults3-meta:/metadata \
  eniz1806/vaults3
```

Dashboard: `http://localhost:9000/dashboard/`
S3 API: `http://localhost:9000`

`--stop-timeout 35` gives requests still running at a stop the 30 seconds the server waits for them (`server.shutdown_timeout_secs`); Docker's default of 10 kills them part way.

## Features

- **Full S3 API** -- 80+ operations, presigned URLs and presigned POST (browser uploads), works with AWS CLI, mc, boto3, any S3 client
- **Built-in web dashboard** -- file browser, IAM management, audit trail, search, stats
- **SSE-S3 and SSE-KMS encryption** -- AES-256-GCM with a static key or a local KMS key provider
- **Object versioning** -- per-bucket versioning with version IDs, delete markers, diff/rollback
- **Object locking (WORM)** -- legal hold and retention (GOVERNANCE/COMPLIANCE)
- **Multipart upload** -- full lifecycle with UploadPartCopy
- **S3 Select** -- SQL queries on CSV, JSON, and Parquet objects
- **IAM users, groups, policies** -- fine-grained access control with policy conditions
- **STS temporary credentials** -- short-lived, auto-expiring access keys
- **OIDC/SSO** -- Google, Keycloak, Auth0 via OpenID Connect
- **External authorization webhook** -- delegate each access decision to an endpoint you run
- **Lifecycle rules** -- expiration by prefix, abort incomplete multipart
- **Event notifications** -- Kafka, NATS, Redis, AMQP/RabbitMQ, PostgreSQL, Elasticsearch, webhooks
- **Replication** -- async push and active-active bidirectional with vector clocks, versioned buckets included
- **Raft clustering** -- multi-node with consistent hashing and automatic failover, replica repair that restores copies after a node is lost for good, and optional sharded metadata so the index grows with the cluster
- **Erasure coding** -- Reed-Solomon with background healer
- **Compression** -- transparent zstd on write (gzip still read) with exclusions for already-compressed types
- **FUSE mount** -- mount buckets as local filesystem directories
- **Full-text search** -- search objects by key, content type, and tags
- **Virus scanning** -- webhook integration (ClamAV, VirusTotal) with quarantine
- **Data tiering** -- objects not read for a while move to a cold directory and are still read from there, RestoreObject brings them back
- **Backup scheduler** -- cron-based full/incremental backups
- **Lambda triggers** -- webhook functions on S3 events
- **Orphan reclaim** -- `vaults3-cli storage reclaim` frees data files that no metadata refers to, across every node
- **Upload integrity** -- every signed upload is checked against its signature and checksums (CRC32, CRC32C, CRC64NVME, SHA1, SHA256, as a header or a trailer) before anything is stored
- **Slow client protection** -- 30 second header limit and a configurable idle timeout on every listener
- **Rate limiting** -- per-IP and per-access-key token bucket, on by default
- **PROXY protocol** -- real client IP behind load balancers
- **Prometheus metrics** -- per-bucket request counts, bytes, errors at `/metrics`
- **Health checks** -- `/health` (liveness) and `/ready` (readiness)

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `VAULTS3_ACCESS_KEY` | Admin access key | `vaults3-admin` |
| `VAULTS3_SECRET_KEY` | Admin secret key | generated on first start, printed once in the container log |
| `VAULTS3_PORT` | Server port | `9000` |
| `VAULTS3_ADDRESS` | Bind address | `0.0.0.0` |
| `VAULTS3_DOMAIN` | Domain for virtual-hosted URLs | _(empty)_ |
| `VAULTS3_DATA_DIR` | Object storage directory | `/data` |
| `VAULTS3_METADATA_DIR` | BoltDB metadata directory | `/metadata` |
| `VAULTS3_DEFAULT_BUCKETS` | Comma-separated buckets to create on startup if missing | _(none)_ |
| `VAULTS3_USAGE_SCAN_INTERVAL_SECS` | How often VaultS3 may re-measure its own on-disk footprint (0 disables) | `300` |
| `VAULTS3_MIN_FREE_MB` | Free disk space (MiB) kept for deletes; below it new writes get 507 (0 disables) | `100` |
| `VAULTS3_ENCRYPTION_KEY` | 64-char hex key (enables SSE-S3) | _(disabled)_ |
| `VAULTS3_TLS_CERT` | TLS certificate path | _(disabled)_ |
| `VAULTS3_TLS_KEY` | TLS private key path | _(disabled)_ |
| `VAULTS3_LOG_LEVEL` | Log level (debug/info/warn/error) | `info` |
| `PUID` / `PGID` | With the container started as root (`--user 0:0`): give `/data` and `/metadata` to this user and group, and run as them (Unraid: `99`/`100`) | _(unset: runs as uid 1000)_ |

## Volumes

| Path | Purpose |
|------|---------|
| `/data` | Object data storage |
| `/metadata` | BoltDB metadata database |

The image runs as uid 1000. A host folder Docker creates for a bind mount is owned by root, which stops the server with `permission denied`: give the folders to uid 1000, or start as root with `PUID`/`PGID` (see above). Unraid has a ready template in [`deploy/unraid`](https://github.com/Kodiqa-Solutions/VaultS3/tree/main/deploy/unraid).

## Docker Compose

```yaml
services:
  vaults3:
    image: eniz1806/vaults3:latest
    ports:
      - "9000:9000"
    environment:
      VAULTS3_ACCESS_KEY: myadmin
      VAULTS3_SECRET_KEY: mysupersecretkey123
    volumes:
      - vaults3-data:/data
      - vaults3-meta:/metadata
    restart: unless-stopped
    # Longer than server.shutdown_timeout_secs (30), so requests still
    # running at a stop get to finish instead of being killed at 10s.
    stop_grace_period: 35s
    # The image has its own healthcheck (vaults3 healthcheck), which follows
    # the configured port, address and TLS.

volumes:
  vaults3-data:
  vaults3-meta:
```

## With Config File

```bash
docker run -d \
  -p 9000:9000 \
  --stop-timeout 35 \
  -v ./vaults3.yaml:/etc/vaults3/vaults3.yaml \
  -v vaults3-data:/data \
  -v vaults3-meta:/metadata \
  eniz1806/vaults3
```

## Command-line tool

The image carries `vaults3-cli`, built from the same commit as the server. It uses the credentials the container was started with:

```bash
docker exec vaults3 vaults3-cli user create alice
docker exec vaults3 vaults3-cli key create alice --bucket my-bucket
```

## Architecture

- **Single binary** -- no runtime dependencies
- **<80MB RAM** -- suitable for edge deployments and small VPS
- **BoltDB** -- embedded metadata store, no external database
- **Non-root container** -- runs as UID 1000

## Links

- [GitHub](https://github.com/Kodiqa-Solutions/VaultS3)
- [Documentation](https://github.com/Kodiqa-Solutions/VaultS3/blob/main/docs/README.md)
- [Issues](https://github.com/Kodiqa-Solutions/VaultS3/issues)

## Project and support

VaultS3 is a [Kodiqa Solutions](https://www.vaults3.com) project. The storage
engine is AGPL-3.0 and stays that way: everything described above is in this
image, with nothing behind a licence key.

Separate paid add-ons for larger deployments (Fleet Console, Kubernetes
Operator, multi-tenant Gateway, Compliance Pack) are at
[vaults3.com/enterprise](https://www.vaults3.com/enterprise/). Commercial
enquiries: support@vaults3.com

Your data stays portable: objects are ordinary files and the index is a BoltDB
file, served over the S3 API, so moving to anything else that speaks S3 is an
`rclone sync` away.

## License

Copyright (C) 2026 Kodiqa Solutions. VaultS3 is free software under the
[GNU Affero General Public License v3.0](https://github.com/Kodiqa-Solutions/VaultS3/blob/main/LICENSE).
