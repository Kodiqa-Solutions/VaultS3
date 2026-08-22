# VaultS3: Scaling & Operations Guide

This guide explains how to scale VaultS3 across **multiple disks** and **multiple
servers**, and provides step-by-step runbooks for recovering from a **lost disk**
or a **lost server**.

> **TLDR for "4 disks per server, going horizontal":**
> Use **erasure coding** to spread data across the 4 disks on each server (survives
> disk loss), and a **Raft cluster** with `replica_count ≥ 2` to spread data across
> servers (survives server loss). These are two independent redundancy layers, use
> **both** for full protection.

---

## 1. Two independent redundancy layers

VaultS3 protects data at two different levels. Understand both before designing a deployment.

| Layer | Protects against | Mechanism | Config block |
|-------|------------------|-----------|--------------|
| **Erasure coding (EC)** | **Disk** failure *within one server* | Reed, Solomon shards striped across local disks | `erasure:` |
| **Clustering** | **Server/node** failure | Raft (metadata) + consistent-hash placement with N replicas (object data) across nodes | `cluster:` |

Key facts:

- **EC does not span servers.** Its shards live on the disks of a single node.
- **Cluster replicas do not protect a single disk.** A replica is a whole-object copy on
  another node. Within a node, disk redundancy still comes from EC (or RAID).
- **Metadata** (bucket/object index) lives in a local embedded BoltDB per node. In a
  cluster it is replicated via **Raft** and is durable only while a Raft **quorum**
  (majority of nodes) is alive.
- **Storage is shared-nothing.** Each node stores its own object data locally. There is
  no shared SAN/NFS requirement.

---

## 2. Choose your topology

| You have… | You want… | Use |
|-----------|-----------|-----|
| 1 server, multiple disks | Survive a disk failure | **Erasure coding** (Section 3) |
| Multiple servers, 1 disk each | Survive a server failure, scale capacity | **Cluster** with `replica_count ≥ 2` (Section 4) |
| Multiple servers, multiple disks each | Survive both disk **and** server failure | **EC + Cluster** (Section 5) ← *recommended for the 4-disk / multi-server case* |
| A second site / region | Disaster recovery, geo-redundancy | **Replication** (Section 8) |

---

## 3. Single server, multiple disks: Erasure coding

Erasure coding splits each object into `data_shards` data shards plus `parity_shards`
parity shards, then distributes them **round-robin** across the disks listed in
`data_dirs`. You can lose up to **`parity_shards`** disks and still reconstruct every object.

### Configuration

`configs/vaults3.yaml`:

```yaml
erasure:
  enabled: true
  data_shards: 4
  parity_shards: 2          # tolerates losing up to 2 disks
  block_size: 4194304       # 4 MB — objects smaller than this bypass EC
  data_dirs:                # one path per physical disk / mount point
    - /mnt/disk1
    - /mnt/disk2
    - /mnt/disk3
    - /mnt/disk4
    - /mnt/disk5
    - /mnt/disk6
  heal_interval_secs: 300   # background heal scan cadence
```

### Sizing rules

- Number of `data_dirs` should be **≥ `data_shards + parity_shards`** so each shard can
  land on its own disk. (Fewer disks still works, shards share disks round-robin, but
  you lose independent-failure protection.)
- **Usable capacity ≈ raw × `data_shards / (data_shards + parity_shards)`.**
  Example: `4 + 2` over six 4 TB disks = 24 TB raw → **~16 TB usable**, tolerates 2 disk losses.
- **Failure tolerance = `parity_shards`.** `4+2` survives 2 disks. `6+2` survives 2 of 8. `3+1` survives 1.
- Objects **smaller than `block_size`** are stored whole (no EC) for efficiency, they are
  protected by clustering replicas, not EC. Size `block_size` to your workload.

### How healing works

- A background **Healer** scans on the `heal_interval_secs` cadence, detects objects with
  missing/corrupt shards, and **reconstructs them from parity** onto a healthy disk, 
  automatically and transparently.
- Reads of a *degraded* object (some shards missing, but ≤ `parity_shards`) succeed live by
  reconstructing on the fly. A warning is logged.
- You can trigger an **on-demand** heal pass with `POST /api/v1/heal?bucket=&prefix=`
  (both params optional, empty `bucket` scans all buckets). It runs the same
  reconstruction as the background healer and returns `202 heal initiated`. Use it to
  repair immediately after replacing a disk instead of waiting for the next interval.

---

## 4. Multiple servers: Raft cluster

A cluster gives you horizontal capacity and **survives losing a node**. Metadata is kept
strongly consistent by **HashiCorp Raft**. Object data is placed across nodes using a
**consistent hash ring** with `replica_count` copies per object.

### Quorum sizing (read this first)

Raft needs a **majority** of nodes alive to elect a leader and accept writes.

| Cluster size | Tolerates losing | Notes |
|--------------|------------------|-------|
| 1 | 0 | Not HA |
| **3** | **1** | Minimum recommended HA |
| **5** | **2** | Recommended for production |
| 7 | 3 | Diminishing returns |

> **Always run an odd number of nodes** (3, 5, 7). An even count adds a node without
> adding fault tolerance and risks split-brain.

Set `replica_count` for object data independently of cluster size. For 3 nodes,
`replica_count: 3` keeps a full copy on every node (max durability). `replica_count: 2`
trades one copy for ~33% more usable capacity.

### Configuration: per node

Each node gets a unique `node_id`. **Exactly one** node bootstraps. The rest join it.

**Node 1 (bootstrap):**
```yaml
cluster:
  enabled: true
  node_id: "node-1"
  bind_addr: "0.0.0.0"
  raft_port: 9001
  api_port: 9000
  bootstrap: true                 # ONLY on the first node
  peers: []
  peer_apis: {}
  placement:
    replica_count: 3
    read_quorum: 2
    write_quorum: 2
    virtual_nodes: 128
  detector:
    probe_interval_secs: 5
    suspect_after: 3              # → "suspect" after 3 missed probes
    down_after: 6                # → "down" after 6 missed probes
    probe_timeout_secs: 3
  rebalance:
    max_bandwidth_mbps: 50       # throttle data movement on membership change
    batch_size: 100
```

**Node 2 / Node 3 (joiners):**
```yaml
cluster:
  enabled: true
  node_id: "node-2"              # unique per node
  bind_addr: "0.0.0.0"
  raft_port: 9001
  api_port: 9000
  bootstrap: false               # joiners must be false
  peers: ["node-1@<node1-host>:9001"]
  peer_apis:
    node-1: "<node1-host>:9000"
  placement: { replica_count: 3, read_quorum: 2, write_quorum: 2, virtual_nodes: 128 }
```

- `peers` entries use the format **`nodeID@host:raftPort`**.
- `peer_apis` maps **`nodeID → host:apiPort`** so nodes can proxy S3 requests to the data owner.
  Set it whenever nodes do **not** all serve the API on the same port. Without an entry,
  a node derives its peers' addresses from Raft membership, which carries only the Raft
  port, and assumes every peer uses the API port this node uses. Where several nodes share
  a host on different ports, that derivation collapses every peer onto the local node.
  Configured entries always win over derived ones (fixed in 4.4.47; before that they were
  overwritten a second after startup, so replicas were never placed on peers). A node's own
  entry is always derived, since `bind_addr` is frequently the `0.0.0.0` wildcard.
- **Optional, separate the cluster control plane from S3 traffic** (recommended for security):
  ```yaml
  server:
    internode_address: "10.0.0.11"   # private NIC
    internode_port: 9100             # cluster endpoints served here instead of the public port
  ```

### Bootstrapping the cluster

1. Start **node-1** (with `bootstrap: true`). Confirm it becomes leader:
   ```bash
   curl -s http://node1:9000/cluster/status | jq
   # → "state": "Leader"
   ```
2. Start **node-2** and **node-3** (`bootstrap: false`).
3. Join each new node to the leader:
   ```bash
   curl -X POST http://node1:9000/cluster/join \
     -H 'Content-Type: application/json' \
     -d '{"node_id":"node-2","addr":"<node2-host>:9001"}'
   ```
   (If you POST to a follower you get a `307` redirect to the leader, follow it.)
4. Verify all members are present and voting:
   ```bash
   curl -s http://node1:9000/cluster/status | jq '.servers'
   # each entry should show "suffrage": "Voter"
   ```

### Adding a new server to a running cluster

How you add a server depends on how you run VaultS3.

#### Kubernetes (StatefulSet) — automatic

The Helm StatefulSet auto-joins new pods: pod-0 bootstraps the cluster and every
other pod is started with `VAULTS3_CLUSTER_JOIN_ADDR` pointing at pod-0, so a new
pod retries joining the leader until it is admitted (a restart with a new pod IP
self-heals the same way). To add a server you just scale up:

```bash
kubectl scale statefulset vaults3 --replicas=4

# watch the new pod get admitted as a voting member
vaults3-cli cluster status
# NODE ID   RAFT ADDRESS        SUFFRAGE   ROLE
# node-0    10.0.0.1:7000       Voter      leader
# ...
# node-3    10.0.0.4:7000       Voter      follower   ← new pod
```

No manual join step is needed. If you use PVCs, the new pod gets its own volumes;
run `vaults3-cli cluster rebalance` afterwards so existing data spreads onto it.

#### Non-Kubernetes (VM / bare metal / Docker)

1. **Provision the new host** with the same VaultS3 version and the shared cluster
   secret.
2. **Start it with clustering enabled and pointed at the leader** (auto-join):
   ```yaml
   cluster:
     enabled: true
     node_id: "node-4"            # unique across the cluster
     bind_addr: "10.0.0.5"
     raft_port: 7000
     join_addr: "10.0.0.1:7000"   # any existing node; it forwards to the leader
     secret: "<same secret as the rest of the cluster>"
   ```
   The node keeps retrying `join_addr` until the leader admits it.

   **Or** add it explicitly from any machine with the admin key:
   ```bash
   vaults3-cli cluster join node-4 10.0.0.5:7000    # <nodeId> <raftAddress>
   ```
   (`cluster join` is executed by the leader; if you point it at a follower the
   request is forwarded.)
3. **Verify** it joined as a voting member:
   ```bash
   vaults3-cli cluster status        # node-4 should appear as Voter
   ```
4. **Rebalance** so data spreads onto the new node:
   ```bash
   vaults3-cli cluster rebalance
   ```

> Keep the cluster at an **odd number of voting members** (3, 5, 7) so Raft can
> form a majority — see "Quorum sizing" above.

### Cluster API reference

| Action | Method & path | Body |
|--------|---------------|------|
| Status / membership | `GET /cluster/status` | |
| Add a node | `POST /cluster/join` | `{"node_id":"...","addr":"host:raftPort"}` |
| Remove a node | `POST /cluster/leave` | `{"node_id":"..."}` |

### Day-2 cluster operations with `vaults3-cli`

`vaults3-cli cluster` wraps the admin API (uses your root admin access/secret key,
same as `vaults3-cli info`) so you don't need raw curl. Point `--endpoint` at any
node (or your load balancer / Kubernetes service).

```bash
export VAULTS3_ENDPOINT=http://vaults3:9000
export VAULTS3_ACCESS_KEY=admin VAULTS3_SECRET_KEY=...

vaults3-cli cluster status                     # members, leader, drain state
vaults3-cli cluster join  node-3 10.0.0.4:7000 # add a member (run against the leader)
vaults3-cli cluster leave node-3               # remove a member (run against the leader)
vaults3-cli cluster drain   node-2             # stop a node accepting writes (reads continue)
vaults3-cli cluster undrain node-2             # resume writes
vaults3-cli cluster rebalance                  # move objects to their correct owner
vaults3-cli cluster decommission node-2        # guided drain + rebalance before replacing a node
```

**Adding a member.** See "Adding a new server to a running cluster" above —
Kubernetes auto-joins on `kubectl scale`; elsewhere start the node with
`cluster.join_addr` set or run `vaults3-cli cluster join <id> <raftAddr>`.

> **Upgrading to 4.4.47 on a cluster whose nodes share a host on different ports:**
> before 4.4.47 the configured `peer_apis` were discarded shortly after startup, so
> peers resolved to the local node and replicas were never placed. Confirm the fix took
> with `vaults3-cli info`, whose per-node line should now show a distinct footprint per
> node, then run `vaults3-cli cluster rebalance` once to backfill the copies objects
> written before the upgrade never got. Clusters with one node per IP (the usual
> Kubernetes shape) were unaffected and need no action.

**Draining.** A drained node returns `503 SlowDown` for S3 object writes while
still serving reads, so you can cordon it before maintenance. In a hash-ring
cluster, new writes for that node's keys keep routing to it until you also change
membership — drain is for taking a node down gracefully, not for permanently
steering traffic away.

**Replacing a server (decommission).** To move a member's data onto the rest of
the cluster and retire it:

1. `vaults3-cli cluster decommission <nodeId>` — drains the node and triggers a
   rebalance so its objects move to the remaining members.
2. Watch `vaults3-cli cluster status` / `vaults3-cli info` until its data has moved.
3. `vaults3-cli cluster leave <nodeId>`, then stop the node.

> **Zero-data-loss decommission requires `placement.replica_count >= 2`** so a
> second copy already exists on another node. With `replica_count: 1`, removing a
> node before its data has fully rebalanced off it loses that data.

---

## 5. Recommended: EC + Cluster (4 disks per server, multiple servers)

This is the configuration for the "4 disks per host, go horizontal" goal. Each node uses
EC across its local disks (disk-failure protection) **and** participates in the cluster
(server-failure protection).

Per node (`node_id` unique, `bootstrap: true` on the first only):

```yaml
storage:
  data_dir: "/mnt/disk1"          # primary; EC also uses the disks below
  metadata_dir: "/mnt/disk1/metadata"

erasure:
  enabled: true
  data_shards: 2
  parity_shards: 2                # any 2 of the 4 local disks can fail
  block_size: 4194304
  data_dirs: ["/mnt/disk1", "/mnt/disk2", "/mnt/disk3", "/mnt/disk4"]
  heal_interval_secs: 300

cluster:
  enabled: true
  node_id: "node-1"
  raft_port: 9001
  api_port: 9000
  bootstrap: true                 # first node only
  peers: []                       # joiners list the bootstrap peer
  placement:
    replica_count: 3              # a full object copy per node (3-node cluster)
    read_quorum: 2
    write_quorum: 2
    virtual_nodes: 128
```

**Result:** A 3-node cluster, each node EC-protected across 4 disks. You can lose **up to
2 disks on any node** *and* **one whole node** without data loss or downtime.

---

## 6. Monitoring & health

- **Cluster health:** `GET /cluster/status` → leader, member list, per-node suffrage.
- **Node liveness:** `GET /health`.
- **Capacity:** `GET /api/v1/cluster/info` → per-node and cluster totals. Expect the
  disk figure to be much larger than the logical one, and read them as answers to
  different questions:

  | Figure | What it counts |
  |--------|----------------|
  | `totals.objectBytes` | **Logical** size: each object's current version, counted once cluster-wide. Excludes replicas, parity, and old versions. |
  | `totals.vaultBytes` | **VaultS3's own footprint**: what its data, metadata, erasure, cold-tier and Raft directories actually occupy, summed over the nodes that have finished a scan (`totals.measuredNodes`). Includes replicas, parity shards, non-current versions and in-progress multipart parts, and nothing else. |
  | `totals.disk.usedBytes` | Used space on the whole **filesystems** backing those directories, summed per node. Also counts the operating system, container images, logs, and any other tenant of the same volume. |

  So on a 3-replica cluster with versioning enabled, several times the logical size on
  disk is expected. Compare `vaultBytes` against `objectBytes`, not `disk.usedBytes`:
  the first pair is a real amplification ratio, while the third figure is mostly a
  statement about the volume, not about VaultS3. A `vaultBytes / objectBytes` ratio
  far beyond `replica_count` x erasure overhead usually means accumulated non-current
  versions or abandoned multipart uploads — expire them with a lifecycle rule
  (`noncurrent_version_expiration_days`, `max_noncurrent_versions`,
  `abort_incomplete_multipart_days`). Per-directory numbers in the same response (and
  in `vaults3-cli info`) tell object data apart from metadata and Raft logs.

  If the ratio stays high after expiring those, look for **orphaned data**: files on
  disk that no metadata refers to any more. They are not listed, not readable, and no
  S3 call can delete them, so they never show up as objects. Clusters that ran
  **before 4.4.49** accumulate them, because the multi-object delete removed metadata
  cluster-wide while freeing the data file only on the node serving the request,
  stranding (N-1)/N of every bulk-deleted byte (issue #47). Find and free them:

  ```bash
  vaults3-cli storage reclaim                 # dry run: what would be freed, per node
  vaults3-cli storage reclaim --apply         # actually free it
  ```

  It scans every node, only considers files with no metadata at all, and never
  touches anything written in the last `--min-age` (default 24h), since a `PUT`
  lands its data before its metadata commits. Run the dry run first and check the
  per-node breakdown; if a node is unreachable the output says so, because its
  orphans are then missing from the totals.

  The walk behind `vaultBytes` is cached; `storage.usage_scan_interval_secs` sets how
  often it may repeat (default 300, `0` disables it and leaves `vaultBytes` at zero).
- **Metrics (Prometheus):** `GET /metrics`, watch for replication lag, heal activity, and
  per-node request distribution.
- **Failure detection** is automatic: the detector marks a peer `suspect` after
  `suspect_after` missed probes and `down` after `down_after`. The failover proxy then
  routes reads/writes to a healthy replica.
- **Log lines worth alerting on.** These name the condition instead of leaving you to
  infer it from client-side errors:

  | Log message | Meaning |
  |-------------|---------|
  | `proxy: upstream hop failed before any response` | A hop to a peer failed and was retried or sent to another holder. Occasional lines during a restart are normal; a steady stream means a peer is unhealthy. |
  | `proxy: every candidate node failed` | No holder could serve the request, so the client got `503 SlowDown`. Always worth investigating. |
  | `object data temporarily unavailable: holder unreachable` | The object exists but the node holding its bytes is unreachable; the client was told to retry. |
  | `object metadata/data desync` | The object is listed but its data is missing on **every** holder. This one is a real inconsistency: reconcile with `vaults3-cli object verify --repair`. |
  | `multipart: a part listed by this upload has no data on disk` | `CompleteMultipartUpload` was asked for a part whose file is gone, so that upload can never complete and the client sees `InvalidPart`. Before 4.4.50 a failed retry of an already-good part caused this; if it appears on 4.4.50 or later the part was lost some other way, and it is worth reporting. |
  | `multipart: part upload failed mid-transfer` | A part's body did not arrive in full (a dropped connection, a client or proxy timeout). Harmless on its own, since any previously uploaded copy of that part is untouched and the client can retry, but a steady stream of these means the network or the client is struggling. |

---

## 7. Recovery runbooks

### 7a. Recover from a **lost disk** (EC enabled)

**Symptom:** one mount point in `data_dirs` is failed/unmounted. Objects with a shard on
that disk are *degraded* but still readable (as long as failures ≤ `parity_shards`).

1. **Confirm tolerance.** Ensure no more than `parity_shards` disks are down. If more are
   down than parity, those objects are unrecoverable from EC alone, restore from a cluster
   replica or backup instead.
2. **Replace the hardware.** Swap the failed disk and mount it at the **same path** listed in
   `data_dirs` (e.g. `/mnt/disk3`). Ensure ownership/permissions match the VaultS3 user.
3. **Let the healer rebuild.** The background healer detects missing shards and reconstructs
   them onto the restored disk on its `heal_interval_secs` cadence. To repair immediately,
   trigger an on-demand pass:
   ```bash
   curl -X POST 'http://<host>:9000/api/v1/heal'              # all buckets
   curl -X POST 'http://<host>:9000/api/v1/heal?bucket=my-bucket&prefix=logs/'
   ```
4. **Verify.** Re-read a sample of affected objects (no degraded-read warnings in logs) and
   confirm shard files are repopulating on the new disk.

> If you front the disks with hardware/software RAID instead of EC, follow your RAID
> controller's rebuild procedure. VaultS3 sees a single volume and needs no action.

### 7b. Recover from a **lost server / node** (cluster enabled)

**Symptom:** a node is unreachable. `cluster/status` shows it as `down`. As long as a Raft
**quorum survives**, the cluster keeps serving via the failover proxy and surviving replicas.

**Case A, node comes back (transient outage):**
1. Restart the VaultS3 process on the node with its **original `node_id`** and config
   (`bootstrap: false`). It rejoins, catches up via Raft, and the rebalancer re-syncs any
   data it missed (throttled by `rebalance.max_bandwidth_mbps`).
2. Verify with `GET /cluster/status`, the node returns to `Voter`/healthy.

**Case B, node is permanently dead (replacement):**
1. **Remove the dead node** from the cluster so it stops counting toward quorum:
   ```bash
   curl -X POST http://<leader>:9000/cluster/leave -d '{"node_id":"node-3"}'
   ```
2. **Provision a replacement** with a **new** `node_id` (e.g. `node-3b`), `bootstrap: false`,
   peers pointing at a current member.
3. **Join it:**
   ```bash
   curl -X POST http://<leader>:9000/cluster/join \
     -d '{"node_id":"node-3b","addr":"<new-host>:9001"}'
   ```
4. The rebalancer redistributes the lost node's share of objects onto the new member to
   restore `replica_count`. Watch `/metrics` and `/cluster/status` until balanced.

**Case C, quorum lost (majority of nodes down at once):**
- Writes are rejected and no leader can be elected until a majority is restored. **Recover/restart
  enough original nodes to regain majority** (their Raft logs and snapshots reconstruct state).
- If a majority is permanently lost, the cluster cannot self-recover, restore from **backup**
  (Section 9) or a **replication peer** (Section 8). This is why an odd node count and an
  off-cluster backup/replica matter.

---

## 8. Cross-site replication (disaster recovery)

For geo-redundancy or a warm standby at another site, replicate to a **peer VaultS3**
instance. (Replication targets are VaultS3-to-VaultS3 only, not arbitrary S3 endpoints.)

```yaml
replication:
  enabled: true
  mode: "push"                          # one-way async
  scan_interval_secs: 30
  max_retries: 5
  batch_size: 100
  peers:
    - name: "dr-site"
      url: "https://dr.example.com:9000"
      access_key: "..."
      secret_key: "..."
```

- **`push`**: async, queue-backed, one-directional to the peer(s). Triggered on object
  PUT/DELETE plus a periodic scan. Retries with backoff.
- **`active-active`**: bidirectional multi-site sync with vector clocks and conflict
  resolution (`conflict_strategy`: `last-writer-wins` (default), `largest-object`,
  `site-preference`). Set a unique `site_id` per site.

Per-bucket replication rules support prefix/tag filters, delete-marker replication, and
replicating pre-existing objects.

---

## 9. Backup & restore

```yaml
backup:
  enabled: true
  targets:
    - { name: "nightly", type: "local", path: "/backups/vaults3" }
  schedule_cron: "0 2 * * *"   # minute hour — daily at 02:00
  retention_days: 30
  incremental: false           # true = only objects changed since last run
```

- Backs up **all buckets and objects** on a cron schedule (or on demand). Incremental mode
  copies only objects modified since the previous run.
- **Targets are local filesystem only** in the current build (no S3 target).
- **Restore is manual:** stop the server (or use a fresh instance), copy the backed-up
  object tree back into `data_dir`, and restart so the metadata store re-indexes. There is
  no built-in one-click restore yet.

---

## 9b. Consistency model in cluster mode

- **Read-your-writes for objects.** After a `PUT` is acknowledged, a `GET`/`HEAD`
  for that object is consistent: writes commit through Raft, and if the read misses
  on the owner (a follower whose state machine hasn't applied the entry yet) the
  read waits for the write to replicate in — polling local state until it lands or
  a 2s timeout — before returning. This waits on normal Raft replication with no
  extra inter-node RPC, so it is robust behind any proxy or network topology. The
  write path stays fast and a genuine miss still returns `404` (issue #37).
- **Bucket visibility.** `BucketExists` uses the same wait-on-miss, so writes right
  after `CreateBucket` don't spuriously get `Bucket does not exist`. `ListBuckets`
  on a follower is still eventually consistent (it may briefly show a just-deleted
  bucket); re-list or target the leader if you need a strict view.
- **Routing follows Raft membership.** The placement ring is reconciled to live
  Raft membership every second, so the window where two pods disagree about a
  key's owner during churn is small. With `replica_count: 1` a key's data lives on
  a single node, so during that brief reconcile window a read that routes to the
  wrong node can miss; set `placement.replica_count: 2` (or more) for production so
  more than one node holds each object and reads survive churn and node loss.
- **Object data replicates in the background, and reads follow it.** With
  `replica_count > 1` the object's bytes are copied to the other holders after the
  write is acknowledged, so for a moment a holder can have the object's metadata but
  not its data. A read that lands there fetches the object from a holder that has it
  rather than reporting `404`, so a `GET` right after a `PUT` is served whichever
  node it reaches (issue #42).
- **A down/OOM node fails fast, and transient failures are retried.** Proxied
  requests to an unreachable shard owner time out quickly (short dial timeout). A hop
  that fails before any response byte reaches the client is retried, then tried
  against the object's other data holders, so a restarting pod or a stale pooled
  connection does not surface as an error. When no node can serve the request the
  answer is `503 SlowDown` with an S3 error document — a retryable, parseable error
  rather than a bare `502` — and every mainstream S3 SDK retries it automatically.

## 9c. Redundancy and sizing (read this before production)

- **`replica_count: 1` (default) means no redundancy.** Each object lives on
  exactly one node; if that node is down (or its disk fails) the object is
  unavailable (or lost). Set **`placement.replica_count: 2`+** and a written
  object's data is streamed to the other nodes in its replica set, so `GET`
  failover serves it when the primary is down and a single flapping/OOM node can't
  take out a shard. This replication is **best-effort and asynchronous** (eventual
  redundancy, not synchronous write-quorum), so pair it with **erasure coding** for
  disk-loss protection of freshly-written data.
- **The two multipliers compound, and both can be set per bucket.** Erasure coding
  and replication are independent, so a bucket's cost on disk is roughly
  `replica_count x erasure overhead`. Measured on a 3-node cluster with 4+2 coding
  (1.5x) and `replica_count: 3`:

  | Bucket setting | On disk, per byte stored |
  |----------------|--------------------------|
  | server defaults (coded, 3 copies) | **4.52x** |
  | `--erasure=off` (3 copies) | 3.00x |
  | `--replicas=1` (coded) | 1.51x |
  | `--erasure=off --replicas=1` | **1.00x** |

  Use `vaults3-cli bucket durability <bucket> [--erasure=on|off|default]
  [--replicas=N|default]` to set them, and note that turning erasure off alone
  still leaves every copy in place: on a replicated cluster the replica count is
  usually the bigger of the two multipliers. Only later writes change; objects
  already stored keep their layout, and reads handle both.
- **Memory sizing.** A pod that is `OOMKilled` mid-benchmark takes its shard
  offline while it restarts (surfacing as `503 SlowDown` for keys only it holds,
  once the retry against the other holders has also failed).

  Up to 4.4.47 a `PUT` was held whole in memory to validate it, and compression
  added two more copies on top, so peak memory scaled with **object size x
  concurrency**: 64 MiB objects at 16 concurrent uploads peaked at ~3.9 GiB and
  OOM-killed a 4 GiB pod. **From 4.4.48 uploads stream**, and the same workload
  peaks at ~1.4 GiB (issue #46). Digests are computed as the bytes flow past, so
  nothing is retained to validate them.

  **Reads with encryption at rest were the mirror image of this, and are fixed in
  4.4.53** (issue #49). Encryption sealed each object as a single AES-GCM
  message, which cannot be decrypted incrementally, so every `GET` allocated the
  ciphertext and the plaintext before the first byte went out: memory scaled with
  **object size x concurrent readers** while the object count was irrelevant.
  Measured on one node with a 643 MiB object, 3 GiB limit: 4.4.52 peaked at
  **2661 MiB for a single reader and was OOM-killed by the second**, where 4.4.53
  peaks at **15 MiB for one reader and 31 MiB for eight**. Writes improved with
  it (2010 MiB to 15 MiB for the same object). Objects are now sealed in 1 MiB
  chunks, each authenticated before its bytes are served.

  Objects written before 4.4.53 keep the old format and are still read, but a
  read of one still costs about its own size (one tag covers the whole object, so
  nothing can be released until all of it is verified). Rewriting an object
  migrates it. If you have large encrypted objects from an earlier version and
  tight pod limits, copy them in place to convert them.

  Two paths still buffer a whole object and should be sized for: SSE-C
  (customer-key encryption still seals the object as one message) and an upload
  whose length is not declared up front (the compressor cannot record the frame
  size without it). Multipart uploads are bounded by part size, not object size.

  Size per-pod memory for concurrency x part/object size plus headroom. If OOMKills
  persist after 4.4.48, that is worth reporting rather than only raising the limit.

  **Measure `anon`, not `memory.current`.** In cgroup v2 `memory.current` (and
  Prometheus `container_memory_usage_bytes`) **includes page cache**. A server
  writing object data fills it to the limit as a matter of course and the kernel
  reclaims it on demand, so "sitting at 100%" is normal and is not what gets a pod
  killed. Alert on `anon` from `memory.stat`, or on working set
  (`memory.current` minus `inactive_file`). Measured on one node at a 4 GiB limit
  with 64 MiB objects and 64 concurrent uploads: 4.4.44 held **2253 MiB of anon
  and was OOM-killed**, while 4.4.50 held **20 MiB** and survived the same
  workload. The killed build reported the *lower* `memory.current` of the two
  (1804 MiB against 4094 MiB), because it died before the cache accumulated, so
  that counter ranked the broken build ahead of the healthy one.

  **Startup memory is flat in the size of the cluster from 4.4.51.** A joining or
  restarting node installs a Raft snapshot before it serves anything, and that
  restore used to run as one database transaction, so it allocated in proportion
  to the total object count and could be OOM-killed during startup (issue #46).
  Restoring a 1.6M-object snapshot peaked at **2175 MiB before and 66 MiB after**.

  **Raising the limit can also hide a fault rather than fix one.** Memory pressure
  makes transfers fail, and until 4.4.50 a part upload that failed partway destroyed
  a copy of that part that had already succeeded, which stranded the whole upload on
  `InvalidPart` for good (issue #48). More memory made the failed transfers rarer, so
  the symptom faded without the cause going anywhere. If more RAM improves an error
  rate without removing it, treat the remainder as a bug worth reporting, and check
  the log lines in section 6 for what the server thinks is happening.

## 10. Known limitations & caveats

- **Metadata durability = Raft quorum.** Lose a majority of nodes simultaneously and cluster
  state cannot self-recover. You must restore original nodes or fall back to backup/replica.
- **EC protects object data, not metadata.** Metadata redundancy comes from Raft (in a
  cluster) or from your `metadata_dir` backups (standalone).
- **EC shards do not span nodes**, and **cluster replicas do not protect a single disk**, 
  combine both layers for full disk-*and*-node protection.
- **Backup:** local target only. Restore is a manual file copy.
- **Replication:** VaultS3↔VaultS3 only. Not to AWS S3 or other providers.
- **Config nit:** sample `heal_interval_secs: 300` vs. an older code default of 3600, set it
  explicitly to avoid surprises.

---

## 11. Listing very large buckets (many objects under one prefix)

VaultS3 keeps every object's metadata in a sorted BoltDB index, so listing a
prefix is a **seek to the continuation marker + read one page**, `O(log n +
page_size)`. Page latency stays flat regardless of how many objects the bucket
holds, because the cost is the page, not the bucket:

| Objects under one prefix | `ListObjectsV2` page (1000 keys) |
|--------------------------|----------------------------------|
| 1,000                    | ~0.8 ms                          |
| 100,000                  | ~0.7 ms                          |
| 1,000,000                | ~0.7 ms                          |
| 10,000,000               | ~0.7 ms                          |
| 100,000,000              | ~0.7 ms                          |

Measured, not extrapolated, across five orders of magnitude: page latency is
flat from a thousand to **a hundred million** objects in a single prefix
(`go test -bench BenchmarkListLatestObjectsPage ./internal/metadata`. A single
mid-tier laptop core. The dominant cost is JSON-decoding the 1000-key page, not
the seek.)

Practical guidance for huge flat prefixes (tens of millions of keys):

- **Paginate** with the `ContinuationToken` / `StartAfter` you get back, each
  page is cheap. The per-page cap is the S3-standard 1000 keys. That's by design.
- This applies to **prefix** listing. Arbitrary substring/partial-name matching
  (`*foo*` anywhere in a key) is a different operation and is not served by the
  ordered index.
- Metadata footprint grows with object count (roughly a few hundred bytes per
  object in the BoltDB file). Budget disk for the metadata DB accordingly at the
  100M-object scale (tens of GB).

## 11a. How many objects a cluster can hold

Listing stays flat (section 11), but the metadata **index** does not shrink, and
in a cluster it does not spread. These are the numbers that decide the ceiling,
all measured rather than estimated.

**By default, object data is sharded across nodes by hash ring and metadata is
not.** Metadata goes through Raft, so every node keeps a complete copy of the
index and applies every metadata write. Adding nodes buys data capacity and
read/list throughput. It buys no metadata capacity and no metadata write
throughput. Setting `cluster.metadata_shards` above 1 changes that; the numbers
below are for the default, and "Sharding the metadata" at the end of this section
covers what changes when you enable it.

| What | Measured |
|------|----------|
| Metadata per object | ~600 bytes (BoltDB file, 50 to 60 character key) |
| Random point lookup, 1M objects (0.6 GB index) | 3.9 µs |
| Random point lookup, 10M objects (5.5 GB index) | 39 µs (index no longer cached) |
| Full scan, 1M / 10M objects | 1.3 s / 27.6 s |
| Single-node metadata write | ~2500/sec (one commit, one fsync per object) |
| 3-node clustered ingest, 1 KiB objects, concurrency 32 | ~666 objects/sec |

So the index costs roughly **0.6 GB per million objects, on every node**:

| Objects | Metadata per node |
|---------|-------------------|
| 1M | ~0.6 GB |
| 10M | ~6 GB |
| 100M | ~60 GB |
| 1B | ~600 GB |

Practical guidance:

- **Up to ~10M objects per node** is comfortable. **~100M** is workable with NVMe
  for `metadata_dir` and enough RAM to keep the index in page cache. Past that the
  metadata layer, not the object store, is what limits you.
- **Anything that walks every object is O(objects)**: lifecycle scans,
  `vaults3-cli storage reclaim`, stats backfill. Budget for it at high counts.
- **The BoltDB file never shrinks.** Deleting 900k of 1M objects left the file at
  its high-water mark. The space is reused by later writes, not returned to the
  filesystem.
- **Raft snapshots serialize the whole store.** `cluster.snapshot_count` defaults
  to 8192 log entries, and each snapshot writes the entire metadata index. Raise
  it substantially on a large cluster.
- **Keep keys short.** The key is stored twice: once as the index key and once
  inside the metadata value.
- **Leave versioning off unless you need it.** Every version is another entry.
- **To go beyond one cluster's ceiling, shard the metadata or run more clusters.**
  A bucket-per-tenant deployment also splits naturally: one cluster per tenant (or
  per group of tenants) keeps each index at a size a node can hold, and gives
  blast-radius isolation and per-tenant key shredding as a side effect.

### Sharding the metadata

`cluster.metadata_shards` splits object metadata across that many independent
Raft groups. Each node runs only the groups it is a member of, so the index on a
node is roughly `shards_held / total_shards` of the cluster's, and metadata
capacity and write throughput grow with the cluster instead of being copied to
every node. A bucket maps to a shard by hash, and buckets, IAM, policies and
multipart state stay in the control group that spans every node, so authorization
and routing still need no lookup.

```yaml
cluster:
  metadata_shards: 8      # fixed for the life of the cluster
  metadata_replicas: 3    # nodes holding each shard
```

Four things to know before enabling it:

- **The shard count is fixed** once the cluster commits its assignment, because
  buckets hash to a shard. There is no resharding.
- **It cannot be switched on for a cluster that already holds objects.** Those
  records are in the control group, which nothing reads once objects route to
  shards, so the server refuses to start rather than hide them. Create a new
  sharded cluster and copy the objects across.
- **The cluster needs at least `metadata_replicas` nodes** before the assignment
  can be created, and the assignment is only created after membership has held
  still for 30 seconds, because the founding members of each shard are permanent.
- **A shard that cannot be reached is reported as unavailable, not as empty.**
  Reads of its buckets return 503 rather than 404, which is deliberate: a 404
  would tell a client its object is gone when the object is intact.

`vaults3-cli cluster shards` shows the committed assignment and the groups running
on the node it is talking to. The design, including the measured numbers that
motivate it, is in
[`docs/design/sharded-metadata.md`](design/sharded-metadata.md).

## 12. Quick reference

| Goal | Block | Key settings |
|------|-------|--------------|
| Survive disk loss (1 host) | `erasure` | `enabled`, `data_shards`/`parity_shards`, `data_dirs[]` |
| Survive node loss | `cluster` | `enabled`, odd node count, `placement.replica_count ≥ 2` |
| Both | `erasure` + `cluster` | EC per node + cluster across nodes |
| Geo / DR | `replication` | `mode`, `peers[]` |
| Point-in-time copies | `backup` | `targets[]`, `schedule_cron`, `incremental` |
| Reclaim orphaned data | (no config) | `vaults3-cli storage reclaim [--apply]` |

| Endpoint | Purpose |
|----------|---------|
| `GET /cluster/status` | Leader, members, suffrage |
| `POST /cluster/join` | Add node `{node_id, addr}` |
| `POST /cluster/leave` | Remove node `{node_id}` |
| `GET /health` | Node liveness |
| `GET /metrics` | Prometheus metrics |
| `POST /api/v1/heal` | Trigger on-demand erasure heal (`?bucket=&prefix=`) |
| `POST /api/v1/reclaim` | Find data no metadata refers to; `?apply=true` frees it (`?min_age_hours=`, `?bucket=`, `?scope=local`) |
