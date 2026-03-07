# VaultS3

Lightweight S3-compatible object storage. Single binary, <80MB RAM, built-in dashboard.

## Quick Start

```bash
docker run -d \
  --name vaults3 \
  -p 9000:9000 \
  -e VAULTS3_ACCESS_KEY=myadmin \
  -e VAULTS3_SECRET_KEY=mysupersecretkey123 \
  -v vaults3-data:/data \
  -v vaults3-meta:/metadata \
  eniz1806/vaults3
```

Dashboard: `http://localhost:9000/dashboard/`
S3 API: `http://localhost:9000`

## Features

- **Full S3 API** -- 80+ operations, works with AWS CLI, mc, boto3, any S3 client
- **Built-in web dashboard** -- file browser, IAM management, audit trail, search, stats
- **SSE-S3 and SSE-KMS encryption** -- AES-256-GCM with static key or HashiCorp Vault KMS
- **Object versioning** -- per-bucket versioning with version IDs, delete markers, diff/rollback
- **Object locking (WORM)** -- legal hold and retention (GOVERNANCE/COMPLIANCE)
- **Multipart upload** -- full lifecycle with UploadPartCopy
- **S3 Select** -- SQL queries on CSV, JSON, and Parquet objects
- **IAM users, groups, policies** -- fine-grained access control with policy conditions
- **STS temporary credentials** -- short-lived access keys with AssumeRole
- **OIDC/SSO** -- Google, Keycloak, Auth0 via OpenID Connect
- **LDAP authentication** -- bind-based with group-to-policy mapping
- **Lifecycle rules** -- expiration, noncurrent version cleanup, abort incomplete multipart
- **Event notifications** -- Kafka, NATS, Redis, AMQP/RabbitMQ, PostgreSQL, Elasticsearch, webhooks
- **Replication** -- async push and active-active bidirectional with vector clocks
- **Raft clustering** -- multi-node with consistent hashing and automatic failover
- **Erasure coding** -- Reed-Solomon with background healer
- **Compression** -- transparent gzip with exclusions for already-compressed types
- **FUSE mount** -- mount buckets as local filesystem directories
- **Full-text search** -- search objects by key, content type, and tags
- **Virus scanning** -- webhook integration (ClamAV, VirusTotal) with quarantine
- **Data tiering** -- automatic hot/cold migration with remote S3-compatible tier
- **Backup scheduler** -- cron-based full/incremental backups
- **Lambda triggers** -- webhook functions on S3 events
- **Batch operations** -- bulk delete and copy processor
- **Rate limiting** -- per-IP and per-access-key token bucket
- **Auto-TLS** -- Let's Encrypt with self-signed fallback
- **PROXY protocol** -- real client IP behind load balancers
- **Prometheus metrics** -- per-bucket request counts, bytes, errors at `/metrics`
- **Health checks** -- `/health` (liveness) and `/ready` (readiness)

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `VAULTS3_ACCESS_KEY` | Admin access key | `vaults3-admin` |
| `VAULTS3_SECRET_KEY` | Admin secret key | `vaults3-secret-change-me` |
| `VAULTS3_PORT` | Server port | `9000` |
| `VAULTS3_ADDRESS` | Bind address | `0.0.0.0` |
| `VAULTS3_DOMAIN` | Domain for virtual-hosted URLs | _(empty)_ |
| `VAULTS3_DATA_DIR` | Object storage directory | `/data` |
| `VAULTS3_METADATA_DIR` | BoltDB metadata directory | `/metadata` |
| `VAULTS3_ENCRYPTION_KEY` | 64-char hex key (enables SSE-S3) | _(disabled)_ |
| `VAULTS3_TLS_CERT` | TLS certificate path | _(disabled)_ |
| `VAULTS3_TLS_KEY` | TLS private key path | _(disabled)_ |
| `VAULTS3_LOG_LEVEL` | Log level (debug/info/warn/error) | `info` |

## Volumes

| Path | Purpose |
|------|---------|
| `/data` | Object data storage |
| `/metadata` | BoltDB metadata database |

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
    healthcheck:
      test: ["CMD", "wget", "--spider", "-q", "http://localhost:9000/health"]
      interval: 30s
      timeout: 5s
      retries: 3

volumes:
  vaults3-data:
  vaults3-meta:
```

## With Config File

```bash
docker run -d \
  -p 9000:9000 \
  -v ./vaults3.yaml:/etc/vaults3/vaults3.yaml \
  -v vaults3-data:/data \
  -v vaults3-meta:/metadata \
  eniz1806/vaults3
```

## Architecture

- **Single binary** -- no runtime dependencies
- **<80MB RAM** -- suitable for edge deployments and small VPS
- **BoltDB** -- embedded metadata store, no external database
- **Non-root container** -- runs as UID 1000

## Links

- [GitHub](https://github.com/Kodiqa-Solutions/VaultS3)
- [Documentation](https://github.com/Kodiqa-Solutions/VaultS3/blob/main/README.md)
- [Issues](https://github.com/Kodiqa-Solutions/VaultS3/issues)
