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
  <a href="#quick-start">Quick start</a> &middot;
  <a href="#features">Features</a> &middot;
  <a href="docs/README.md">Docs</a> &middot;
  <a href="docs/S3-API.md">S3 API</a> &middot;
  <a href="docs/DASHBOARD.md">Dashboard</a> &middot;
  <a href="docs/INSTALL.md#docker">Docker</a>
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

## Quick start

```bash
docker run -d --name vaults3 -p 9000:9000 \
  -e VAULTS3_ACCESS_KEY=myadmin \
  -e VAULTS3_SECRET_KEY=mysupersecret \
  -v vaults3-data:/data -v vaults3-meta:/metadata \
  eniz1806/vaults3
```

The S3 API and the dashboard both listen on port 9000:

```bash
aws --endpoint-url http://localhost:9000 s3 mb s3://my-bucket
aws --endpoint-url http://localhost:9000 s3 cp ./file.txt s3://my-bucket/
open http://localhost:9000/dashboard/
```

No config file is needed. VaultS3 starts on its built-in defaults, creates the
directories it needs, and generates an admin secret that it prints once on first
start. Set `VAULTS3_ACCESS_KEY` and `VAULTS3_SECRET_KEY` to use your own.

Prefer a package or a plain binary? Every [release](https://github.com/Kodiqa-Solutions/VaultS3/releases)
ships `.deb`, `.rpm` and `.apk` packages, static binaries for Linux, macOS and
Windows, an SPDX SBOM per platform, and a Sigstore provenance bundle you can
verify offline:

```bash
sudo apt install ./vaults3_4.4.67_amd64.deb
sudo systemctl enable --now vaults3
gh attestation verify vaults3_4.4.67_amd64.deb --repo Kodiqa-Solutions/VaultS3
```

Building from source is `make build`. Kubernetes is a Helm chart or a single
manifest. All of it, plus the disk layout to use in production, is in the
**[installation guide](docs/INSTALL.md)**.

## Features

The short version. Each row links to the guide that covers it, and the
[full list](docs/FEATURES.md) has everything else.

| | |
|---|---|
| **S3 API** | 80+ operations, SigV4, multipart, range requests, conditional requests, checksums, presigned URLs. [Operation list](docs/S3-API.md) |
| **Web dashboard** | File browser, drag-and-drop upload, stats, IAM, audit, search, backups, in English, German, French and Chinese. [Guide](docs/DASHBOARD.md) |
| **Access control** | IAM users, groups and policies, OIDC/JWT SSO, LDAP, STS, per-bucket CORS, IP allowlists, audit trail. [Guide](docs/ACCESS-CONTROL.md) |
| **Encryption** | AES-256-GCM at rest, SSE-S3, SSE-KMS, SSE-C, and per-bucket keys with rotation and crypto-shredding. [Guide](docs/CONFIGURATION.md#encryption-at-rest) |
| **Durability** | Reed-Solomon erasure coding with a background healer, per-bucket replica counts, orphan reclaim. [Guide](docs/SCALING.md) |
| **Clustering** | Raft-replicated metadata, consistent-hash placement, failover routing, optional metadata sharding. [Guide](docs/SCALING.md) |
| **Replication** | One-way async and bidirectional active-active with vector-clock conflict resolution. [Guide](docs/INTEGRATIONS.md#async-replication) |
| **Data management** | Versioning with diff and rollback, object lock, lifecycle, compression, tiering, scheduled backups. [Guide](docs/DATA-MANAGEMENT.md) |
| **Integrations** | Webhook, Kafka, NATS, Redis, AMQP, PostgreSQL and Elasticsearch notifications, lambda triggers, virus scanning. [Guide](docs/INTEGRATIONS.md) |
| **Search** | Full-text over metadata and tags, plus optional semantic search and RAG retrieval with no external vector database. [Guide](docs/INTEGRATIONS.md#full-text-search) |
| **Migrate in** | Import buckets from MinIO, SeaweedFS, Garage, Ceph, AWS, R2, Wasabi or B2, preserving dates, metadata, policies and tags |
| **Operations** | Prometheus metrics, structured logs, health and readiness endpoints, pprof, `vaults3 diagnose` for bug reports, `vaults3-cli` for day-2 work. [Guide](docs/CLI.md) |

## Production Readiness

VaultS3 is honest about what's battle-tested versus still maturing. Pick the lane that matches your risk tolerance:

| Path | Maturity | Notes |
|---|---|---|
| **Single-node** (S3 API, versioning, IAM, dashboard) | ✅ Stable | The default deployment. Broad test coverage. Runs in production today. **One known edge case:** deleting an object that was written *before* versioning was enabled on its bucket writes a delete marker with no version record behind it, so removing that marker later cannot bring the object back and its bytes are left orphaned on disk. Turn versioning on before you need it, and `vaults3-cli storage reclaim` frees anything already stranded. |
| **Erasure coding** (single-node, multi-disk) | ✅ Stable | Reed-Solomon encode/reconstruct and the background healer have fault-injection tests (lose disks → reconstruct → heal). Reads and writes both stream as of 4.4.60, so neither holds an object in memory: a PUT costs a fixed number of stripe buffers rather than the object plus its parity, and a degraded read rebuilds one aligned stripe at a time instead of the whole object up front. That is what makes large objects at high concurrency safe here. |
| **Tiering & backup** | ✅ Stable | Hot/cold migration, transparent promotion, and full/incremental backup are tested. Restore is a manual file copy. |
| **Multi-node Raft clustering** | 🟡 Beta | Metadata writes replicate via Raft consensus (writes accepted on any node via leader-forwarding), object data is placed/served by a live-membership hash ring, inter-node calls are authenticated, and on Kubernetes the cluster auto-forms (leader bootstrap + auto-join + self-heal). Validated end-to-end on a real 3-node cluster: leader election & failover, node recovery with catch-up, cross-node reads, and concurrent load, including a 10,000-write list-then-write-then-read workload behind a gateway with a node restarted mid-run. **One known issue:** overwriting an existing key and reading it straight back can return the *previous* bytes carrying the *new* object's `ETag` and `Last-Modified`, because the metadata read waits for replication but a data file already present on the serving node is handed back without checking that it is still current. It converges in about two seconds. Enabling versioning on the bucket avoids it, since a versioned read is keyed by version ID and falls through to a node that actually holds those bytes. Still operationally newer, not yet stress/scale/multi-region hardened, so validate against your workload before trusting it as the only copy of critical data. |
| **Sharded metadata** (`cluster.metadata_shards > 1`) | 🟠 Opt-in, since 4.4.54 | Splits object metadata across independent Raft groups so metadata capacity grows with the cluster instead of every node holding the whole index. Validated on real three-node clusters, local and in containers: shard assignment, group membership reconciliation, routed reads and writes from a node holding no copy of a shard, and an unreachable shard reporting `503` rather than a phantom `404`. Newer than everything above it, and the shard count is fixed when the cluster first commits its assignment, so treat it as opt-in for new clusters you can validate. Off by default. |
| **Active-active replication** | 🟡 Beta | Vector-clock conflict resolution is unit-tested. The cross-site sync worker is less exercised in the wild. |

**Security:** VaultS3 was reviewed by an external white-box security assessment in August 2026, which reported 14 findings. All are fixed in 4.4.56. See [SECURITY.md](SECURITY.md) for the summary and [CHANGELOG.md](CHANGELOG.md) for each finding. If you run an earlier version, upgrade and read the [upgrade notes](docs/UPGRADING.md#upgrading-to-4456-security-release). Container images for versions before 4.4.56 have been withdrawn from Docker Hub, so those tags no longer pull.

Defaults are chosen to be safe on a public network: authentication is required, policy evaluation is default-deny, and as of 4.4.65 rate limiting is on out of the box, at a ceiling far above real client traffic so it bounds a flood without throttling legitimate use. See the [hardening guide](docs/HARDENING.md).

**S3 compatibility:** VaultS3 is measured against [ceph/s3-tests](https://github.com/ceph/s3-tests), the suite the rest of the object-storage world is tested with, rather than only against its own idea of the spec. CI gates on the tests it is expected to pass, and a weekly sweep runs the whole suite to find more to promote. The harness and the current baseline are in [scripts/s3-tests/](scripts/s3-tests/README.md).

**Recommendation:** run single-node (optionally with erasure coding across local disks) for production data you care about, and treat clustering/active-active as advanced opt-in features you validate first. Always keep an independent backup. See the **[Scaling & Operations Guide](docs/SCALING.md)** for redundancy layering and recovery runbooks, and the **[Benchmarks guide](docs/BENCHMARKS.md)** for a reproducible way to measure throughput and RAM on your own hardware.

## Documentation

Full documentation lives in **[docs/](docs/README.md)**, versioned with the code
so a checked-out tag carries the docs for that release.

| | | |
|---|---|---|
| [Install and deploy](docs/INSTALL.md) | [Configure](docs/CONFIGURATION.md) | [Dashboard](docs/DASHBOARD.md) |
| [S3 operations](docs/S3-API.md) | [Access control](docs/ACCESS-CONTROL.md) | [Data management](docs/DATA-MANAGEMENT.md) |
| [Integrations](docs/INTEGRATIONS.md) | [CLI](docs/CLI.md) | [Full feature list](docs/FEATURES.md) |
| [Scaling and operations](docs/SCALING.md) | [Benchmarks](docs/BENCHMARKS.md) | [Upgrading](docs/UPGRADING.md) |
| [Security hardening](docs/HARDENING.md) | [Architecture](docs/ARCHITECTURE.md) | [Roadmap](docs/ROADMAP.md) |

## Security

An external white-box security assessment in August 2026 reported 14 findings.
All are fixed in 4.4.56, and the advisory is published as
[GHSA-3786-qcjv-9h84](https://github.com/Kodiqa-Solutions/VaultS3/security/advisories/GHSA-3786-qcjv-9h84).

The hardening measures built into the server are listed in
**[docs/HARDENING.md](docs/HARDENING.md)**. To report a vulnerability, or for
deployment best practices, see **[SECURITY.md](SECURITY.md)**.

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
