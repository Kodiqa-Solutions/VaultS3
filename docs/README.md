# VaultS3 documentation

Everything here lives in the repository, so it is versioned with the code and
travels with a clone or a fork. Check out a tag and you get the documentation
for that release.

[Back to the project README](../README.md)

---

## Getting started

| Guide | What it covers |
|---|---|
| [Installation and deployment](INSTALL.md) | Packages, source builds, running the server, Docker, Kubernetes, and what VaultS3 expects from a disk |
| [Configuration](CONFIGURATION.md) | The config file, every option group, TLS, health checks, metrics, rate limiting |
| [Web dashboard](DASHBOARD.md) | The built-in React UI at `/dashboard/` |
| [Command-line tool](CLI.md) | Buckets, objects, users, storage and clusters with `vaults3-cli` |

## Using it

| Guide | What it covers |
|---|---|
| [Supported S3 operations](S3-API.md) | Every S3 API call VaultS3 implements |
| [Data management](DATA-MANAGEMENT.md) | Versioning, object lock, lifecycle, compression, small-file packing, tiering, backup, snapshots |
| [Access control](ACCESS-CONTROL.md) | IAM users and policies, CORS, STS, the audit trail, IP restrictions, presigned upload restrictions |
| [Integrations](INTEGRATIONS.md) | Event notifications, replication, virus scanning, website hosting, access logs, search, S3 Select, FUSE |
| [Full feature list](FEATURES.md) | Everything in the box, in one list |

## Running it in production

| Guide | What it covers |
|---|---|
| [Scaling and operations](SCALING.md) | Multi-disk erasure coding, cluster setup, large-prefix listing, lost-disk and lost-server recovery runbooks |
| [Benchmarks](BENCHMARKS.md) | A reproducible way to measure throughput and RAM on your own hardware |
| [Security hardening](HARDENING.md) | The measures built into the server |
| [S3 conformance testing](../scripts/s3-tests/README.md) | How VaultS3 is measured against the ceph/s3-tests suite, and the current baseline |
| [Upgrading](UPGRADING.md) | Release-to-release upgrade notes, including the 4.4.56 security release |

## Reference

| Guide | What it covers |
|---|---|
| [Architecture](ARCHITECTURE.md) | How the source tree is laid out and what it is built on |
| [Roadmap](ROADMAP.md) | What is planned, and what has already shipped |
| [Per-bucket encryption design](design/per-bucket-encryption.md) | Envelope encryption: KEK and DEK, rotation, crypto-shredding |
| [Sharded metadata design](design/sharded-metadata.md) | Splitting object metadata across independent Raft groups |

---

Reporting a vulnerability is covered in [SECURITY.md](../SECURITY.md).
Contributing, including [translating the dashboard](../CONTRIBUTING.md#translating-the-dashboard),
is in [CONTRIBUTING.md](../CONTRIBUTING.md).
