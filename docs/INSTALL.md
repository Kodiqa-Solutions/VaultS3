# Installation and deployment

Packages, source builds, Docker, Kubernetes, and what VaultS3 expects from a disk.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

## Requirements

- Go 1.26+ (build)
- Node.js 20.19+ (dashboard build only, Vite 8 / Rolldown)
- No runtime dependencies


## Install a package

RPM, DEB and APK packages are attached to every [release](https://github.com/Kodiqa-Solutions/VaultS3/releases). They install the binary, a config file, a systemd unit and an unprivileged `vaults3` account, and leave `/var/lib/vaults3` alone on upgrade and on removal.

```bash
# Debian or Ubuntu
sudo apt install ./vaults3_4.4.70_amd64.deb
# RHEL, Rocky or Fedora
sudo rpm -i vaults3-4.4.70-1.x86_64.rpm
# Alpine
sudo apk add --allow-untrusted vaults3_4.4.70_x86_64.apk

sudo systemctl enable --now vaults3
journalctl -u vaults3 --no-pager | head -40   # the admin secret is printed once
```

Every release also ships an SPDX SBOM per platform, generated from the binary so it lists the modules actually compiled in, and a Sigstore provenance bundle attached as an asset, so a download can be verified against the workflow run and commit that produced it, offline or from a mirror:

```bash
gh attestation verify vaults3_4.4.70_amd64.deb --repo Kodiqa-Solutions/VaultS3
```

## Build from source

```bash
make build
```

## Run

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


## Docker

```bash
# From Docker Hub
docker pull eniz1806/vaults3
docker run -p 9000:9000 \
  -e VAULTS3_ACCESS_KEY=myadmin \
  -e VAULTS3_SECRET_KEY=mysupersecret \
  -v vaults3-data:/data -v vaults3-meta:/metadata \
  eniz1806/vaults3

# Or build locally
docker build -t vaults3 .
docker run -p 9000:9000 -v vaults3-data:/data -v vaults3-meta:/metadata vaults3
```

Images are automatically published to [Docker Hub](https://hub.docker.com/r/eniz1806/vaults3) on every push to `main`.

### Storage layout in production

Keep `/data` and `/metadata` on **separate volumes**, not inside the container and not on the same disk. A volume can be snapshotted, cloned, resized and reattached to a redeployed container, which is the difference between a container you can replace and one you cannot afford to lose.

They also want different storage, because they are not the same kind of data:

| | what it holds | size | wants |
|---|---|---|---|
| `/metadata` | the BoltDB index: buckets, object records, IAM, versions | small, tens of MB for 100k objects at roughly 600 bytes each | **low latency**, an SSD. Every request touches it |
| `/data` | the objects themselves | as large as your data | **capacity and throughput**. Latency matters far less |

Put metadata on fast storage and data on capacity storage, and back them up on different schedules: metadata is small enough to snapshot often, and it is the part that makes the objects findable. Objects without their metadata are files with no index. Metadata without its objects is an index pointing at nothing, so **both must be captured together, from the same moment**, for a restore to mean anything.

### Buckets on First Start

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

### Environment Variables

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

## Storage requirements

VaultS3 stores each object as a regular **file** under `data_dir` and keeps
metadata in a **BoltDB** file under `metadata_dir`, so both must point at a
**mounted filesystem**, not a raw block device. Format the disk first (**XFS
recommended**. `ext4` also works) and mount it, then point `data_dir` at a
directory on the mount. This is the same model as MinIO.

- One file per object means a filesystem with plenty of inodes (XFS handles this
  well). For workloads with millions of tiny objects, enable the experimental
  [small-file packing](DATA-MANAGEMENT.md#small-file-packing-experimental) mode to pack them into
  large volume files and cut per-file overhead.
- On Kubernetes, a CSI driver like **DirectPV** is a good fit, it formats disks
  with XFS and presents them as mounted PVCs, which is exactly what VaultS3 wants.

## Kubernetes

Deploy with the bundled **Helm chart** or a single **plain-manifest** quickstart
(both under [`deploy/`](../deploy/)):

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
Ingress + Prometheus ServiceMonitor. See [`deploy/README.md`](../deploy/README.md)
and the [chart reference](../deploy/helm/vaults3/README.md).

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
k8up, or a CSI snapshot, see the [chart reference](../deploy/helm/vaults3/README.md#backups--restore).
