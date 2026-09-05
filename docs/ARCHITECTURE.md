# Architecture

How the source tree is laid out and what it is built on.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

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
│   ├── iam/                   — IAM policy engine, identity, IP access control, policy conditions
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
