# VaultS3 Helm Chart

Deploy [VaultS3](https://github.com/Kodiqa-Solutions/VaultS3), a lightweight,
S3-compatible object store with a built-in dashboard, to Kubernetes.

VaultS3 runs as a **StatefulSet** with persistent volumes for object data
(`/data`) and BoltDB metadata (`/metadata`). A single port (`9000`) serves the
S3 API, the web dashboard (`/dashboard/`), Prometheus metrics (`/metrics`), and
the `/health` / `/ready` probe endpoints.

## Install

```bash
# From a local checkout of the repo:
helm install vaults3 ./deploy/helm/vaults3 \
  --namespace vaults3 --create-namespace \
  --set auth.secretKey="$(openssl rand -hex 20)"
```

Get the admin credentials and reach the dashboard:

```bash
kubectl -n vaults3 get secret vaults3 -o jsonpath='{.data.access-key}' | base64 -d; echo
kubectl -n vaults3 get secret vaults3 -o jsonpath='{.data.secret-key}' | base64 -d; echo
kubectl -n vaults3 port-forward svc/vaults3 9000:9000
# open http://localhost:9000/dashboard/
```

## Key values

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `1` | Keep at 1 unless using Raft clustering (Beta). |
| `image.repository` | `eniz1806/vaults3` | Image repo. |
| `image.tag` | `""` | Defaults to the chart `appVersion`. Pin for reproducibility. |
| `auth.accessKey` | `vaults3-admin` | Admin access key (injected via Secret → env). |
| `auth.secretKey` | `vaults3-secret-change-me` | Admin secret. **Change it**, or set empty to auto-generate. |
| `auth.existingSecret` | `""` | Use your own Secret (keys `access-key`, `secret-key`). |
| `config` | single-node config | The `vaults3.yaml` mounted at `/etc/vaults3/`. Replace to enable encryption/replication/erasure/external auth/etc. |
| `existingConfigMap` | `""` | Use your own ConfigMap (key `vaults3.yaml`). |
| `defaultBuckets` | `[]` | Buckets created on startup if missing (e.g. `{app-data,backups}`). Existing buckets are untouched; an invalid name stops the pod. |
| `usageScanIntervalSecs` | `300` | How often VaultS3 may re-measure its own on-disk footprint, so the dashboard can show it separately from total filesystem usage. `0` disables the walk. |
| `controller.kind` | `StatefulSet` | `StatefulSet` (default. Required for clustering/multi-replica) or `Deployment` (single-node, standalone PVCs). |
| `persistence.enabled` | `true` | Keep enabled for real use. |
| `persistence.data.size` | `50Gi` | Object-data PVC size. |
| `persistence.data.existingClaim` | `""` | Mount a pre-existing data PVC (Deployment mode), e.g. a restored backup. |
| `persistence.metadata.size` | `5Gi` | Metadata PVC size. |
| `persistence.metadata.existingClaim` | `""` | Mount a pre-existing metadata PVC (Deployment mode). |
| `service.type` | `ClusterIP` | Use `LoadBalancer` or an Ingress to expose. |
| `ingress.enabled` | `false` | Enable + set `hosts` to expose via Ingress. For large uploads set `nginx.ingress.kubernetes.io/proxy-body-size: "0"`. |
| `serviceMonitor.enabled` | `false` | Prometheus-Operator scraping of `/metrics`. |
| `resources` | 100m/128Mi → 1/512Mi | VaultS3 is light (<80MB RAM typical). |
| `extraEnv` | `[]` | Extra env vars (`VAULTS3_LOG_LEVEL`, `VAULTS3_DOMAIN`, `VAULTS3_ENCRYPTION_KEY`, …). |

See [`values.yaml`](./values.yaml) for the full list.

## Enabling features

VaultS3 features (encryption, compression, replication, erasure coding, tiering,
etc.) are driven by `vaults3.yaml`. Override the `config` value with your own:

```bash
helm install vaults3 ./deploy/helm/vaults3 -n vaults3 --create-namespace \
  --set-file config=./my-vaults3.yaml \
  --set auth.secretKey="$(openssl rand -hex 20)"
```

Keep `storage.data_dir: /data` and `storage.metadata_dir: /metadata` (the chart
forces these via env vars anyway, so they always land on the PVCs).

## Clustering (Beta)

Set `cluster.enabled=true` with an **odd `replicaCount` ≥ 3** and the chart
auto-forms a Raft cluster: pod-0 bootstraps as the initial leader and the other
pods auto-join it over stable headless-service DNS, no manual bootstrap/join
steps. Raft state lives on the metadata PVC, and a node re-joins automatically
after a restart (its identity is the StatefulSet DNS name, not its pod IP).

```bash
helm install vaults3 ./deploy/helm/vaults3 -n vaults3 --create-namespace \
  --set cluster.enabled=true --set replicaCount=3 \
  --set auth.secretKey="$(openssl rand -hex 20)"

# verify the cluster has a leader + all members
kubectl -n vaults3 exec vaults3-0 -- wget -qO- http://localhost:9000/cluster/status
```

Metadata writes (buckets, objects, IAM, …) are committed through Raft consensus,
so all nodes converge, and a write to **any** node works (a write to a follower
is transparently forwarded to the leader). Reads are served locally.

> **Beta.** Clustering is functional but newer and less battle-tested than
> single-node + erasure coding. For maximum production durability today, prefer a
> **single node with erasure coding** (disk redundancy), validate clustering
> against your workload before trusting it as the only copy of critical data. See
[`docs/SCALING.md`](https://github.com/Kodiqa-Solutions/VaultS3/blob/main/docs/SCALING.md)
for the redundancy trade-offs.

| Cluster value | Default | Description |
|---|---|---|
| `cluster.enabled` | `false` | Auto-form a Raft cluster across the replicas (Beta). |
| `cluster.raftPort` | `9001` | Port for inter-node Raft traffic. |
| `cluster.metadataShards` | `1` | Split object metadata across this many independent Raft groups. |
| `cluster.metadataReplicas` | `3` | Pods holding each metadata shard. |

**With `metadataShards: 1`, adding pods adds capacity for object data, not for
metadata.** Object data is sharded across the pods by consistent hash, metadata is
Raft-replicated, so every pod carries a complete copy of the metadata index at
roughly 600 bytes per object. Ten million objects per pod is comfortable, a
hundred million is workable with a larger metadata PVC.
[`docs/SCALING.md`](https://github.com/Kodiqa-Solutions/VaultS3/blob/main/docs/SCALING.md)
has the measured numbers.

**Beyond that, set `cluster.metadataShards` above 1** and each pod holds only the
shards it is a member of, so metadata capacity grows with the cluster. Three
things to know before you do: the shard count is fixed once the cluster commits
its assignment (buckets hash to a shard, and there is no resharding), it cannot be
switched on for a cluster that already holds objects (the pods refuse to start
rather than hide metadata already written), and `replicaCount` must be at least
`cluster.metadataReplicas` or no assignment can be created.
[`docs/design/sharded-metadata.md`](https://github.com/Kodiqa-Solutions/VaultS3/blob/main/docs/design/sharded-metadata.md)
is the design, and `vaults3-cli cluster shards` shows the result.

**If a clustered install predates 4.4.49, reclaim its leaked space once after
upgrading.** Until then the multi-object delete (what Spark/Hadoop S3A uses)
removed metadata cluster-wide but freed the data file only on the pod serving the
request, stranding `(N-1)/N` of every bulk-deleted byte with no way to reach it
again. `/data` therefore grows well past the logical size and no S3 call shrinks it.

The container image carries the server only, so drive this from outside the
cluster with the `vaults3-cli` release binary (or call the API directly):

```bash
kubectl port-forward -n vaults3 svc/vaults3 9000:9000 &
export VAULTS3_ENDPOINT=http://localhost:9000
export VAULTS3_ACCESS_KEY=vaults3-admin VAULTS3_SECRET_KEY=<your secret>

vaults3-cli storage reclaim               # dry run: what would be freed, per pod
vaults3-cli storage reclaim --apply --yes # actually free it
```

One pod coordinates and scans all of them, so port-forwarding to any single pod
is enough. It only touches files that no metadata refers to, and never anything
written in the last 24 hours. Run the dry run first and read the per-node
breakdown; unreachable pods are called out, since their orphans are then missing
from the totals.

## Backups & restore

VaultS3 keeps object **data** on `/data` (plain files) and **metadata** in a
BoltDB file on `/metadata`, and they reference each other.

**Backing up the PVCs** (Velero, k8up, CSI snapshots, …):

- Back up **`/data` and `/metadata` together, from the same point in time.** A
  snapshot of one paired with a mismatched copy of the other can leave dangling
  references. Atomic **CSI volume snapshots** are preferable to a live file copy.
  if you must file-copy, quiescing writes (or a brief downtime) gives the cleanest
  result. BoltDB is crash-consistent, so a live snapshot is usually recoverable.
- StatefulSet PVCs (`data-<release>-0`, `metadata-<release>-0`) are backed up by
  Velero/k8up like any other PVC, clustering is not required to do so.

**Restoring into a known PVC** (the easiest restore workflow): run in
**Deployment mode** and point the chart at the restored claims:

```bash
helm install vaults3 ./deploy/helm/vaults3 -n vaults3 \
  --set controller.kind=Deployment \
  --set persistence.data.existingClaim=restored-data \
  --set persistence.metadata.existingClaim=restored-metadata \
  --set auth.secretKey="$(openssl rand -hex 20)"
```

In Deployment mode the chart's standalone PVCs carry a `helm.sh/resource-policy:
keep` annotation, so they survive `helm uninstall` and can be re-attached on
reinstall.

**App-level alternative:** VaultS3 also has a built-in backup (full/incremental)
and bucket snapshots, which sidestep the cross-volume-consistency concern, see
the [data management guide](https://github.com/Kodiqa-Solutions/VaultS3/blob/main/docs/DATA-MANAGEMENT.md).

## Uninstall

```bash
helm uninstall vaults3 -n vaults3
# PVCs are retained by design — delete them explicitly to wipe data:
kubectl -n vaults3 delete pvc -l app.kubernetes.io/name=vaults3
```
