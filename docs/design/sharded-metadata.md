# Design: Sharded Metadata

**Status:** Implemented in 4.4.54, off by default (`cluster.metadata_shards` is 1)
**Issue:** [#50](https://github.com/Kodiqa-Solutions/VaultS3/issues/50), "can VaultS3 handle billions of objects?"

## Summary

By default a VaultS3 cluster spreads object **data** across its nodes but keeps a
complete copy of the **metadata** on every node. Adding nodes therefore buys data
capacity and read throughput and buys nothing at all for metadata. This document
records the measured ceiling that follows from that, and the design that splits
the metadata across independent Raft groups so that metadata capacity grows with
the cluster instead.

The default is unchanged: a cluster that says nothing keeps the topology it has
always had. Setting `cluster.metadata_shards` above 1 opts into the one described
here.

## The asymmetry

| | How it is placed | What N nodes buy you |
| --- | --- | --- |
| Object data | Consistent hash ring, `replica_count` nodes hold each object | Capacity grows with N |
| Metadata, unsharded | Raft-replicated, every node holds every record | Nothing. Every node holds the same index |
| Metadata, sharded | One Raft group per shard, `metadata_replicas` nodes hold each | Capacity grows with N |

Metadata being replicated everywhere is what makes a read servable from any node
without a lookup hop, and it is what makes metadata authoritative for
reconciliation. It is also the object-count ceiling, and sharding trades the first
of those for the third: a node that does not hold a bucket's shard pays one
store-level hop to read it.

## Measured

Measured on a 3-node Docker cluster with a Linux client container, and with a
direct harness against the metadata store. Keys were 50 to 60 characters.

| What | Result |
| --- | --- |
| Metadata per object | about **600 bytes** (621 B at 1M objects, 592 B at 10M) |
| So the index is | 0.6 GB at 1M, 60 GB at 100M, **600 GB at 1B, on every node** |
| Point lookup | 3.9 us at a 0.6 GB index, **39 us at 5.5 GB** (same hardware, the index no longer fits page cache) |
| Listing a 1000-key page | 1 to 4 ms at 10M objects, flat, including deep in the keyspace |
| Full scan of the index | 1.3 s at 1M, **27.6 s at 10M** (lifecycle, reclaim and stats backfill are all O(objects)) |
| Single-node metadata writes | 2554/s on Linux ext4, 126/s on macOS APFS (one commit is one fsync) |
| 3-node clustered ingest, 1 KiB, c=32 | 515/s before batching, **666/s after** (4.4.54) |
| The same ingest with Raft removed | 886/s, so consensus costs about 25% and the per-object file write is the real ceiling |
| Follower index size after 200k objects | 128.0 MB on each follower, byte-identical, which is the replication being complete |

Two properties worth knowing before planning a large deployment:

- **The metadata file never shrinks.** Deleting 900k of 1M objects left the file
  at its high-water mark. Space is reused by later writes, it is not returned to
  the filesystem.
- **Every Raft snapshot re-serializes the whole store**, and `snapshot_count`
  defaults to 8192 log entries. A 200k-object ingest wrote 170 MB of snapshots.
  At 100M objects each snapshot is a multi-GB write on every node.

### Practical guidance today

- Up to about **10M objects per node**: comfortable.
- Up to about **100M objects per node**: workable on NVMe with enough RAM to keep
  the index in page cache, and with the full-scan costs above in mind.
- **Billions**: run a cluster per tenant rather than one larger cluster. If your
  tenant boundary is already a bucket or a customer, this also gives you blast
  radius isolation and per-tenant crypto-shredding for free.

## Why not a different database

The obvious answer to "the index does not shard" is to put the index in something
that does, FoundationDB being the usual suggestion. The blocking constraint is
that VaultS3 ships as a **single static binary built with `CGO_ENABLED=0`**, and
the FoundationDB client is a C library. Adopting it would mean either giving up
the single-binary property, or making it an optional non-default backend that
almost nobody runs and that therefore gets almost no testing. Sharding the
existing store keeps the deployment story intact.

## Design

The cluster gains a **control group** (the existing Raft group, which keeps
buckets, IAM, policies and the shard map) and **shard groups**, each an
independent Raft group owning the object metadata for a subset of buckets.

- Buckets map to shards by `xxhash(bucket) % shards`. Shard membership comes from
  the same consistent hash ring the data placement uses, so no second topology
  source has to be maintained.
- The map is a committed control-group record with a version, a creation epoch
  and a founding member set. Shard count is fixed at creation.

### Constraints the design has to respect

An adversarial review of an earlier draft, run against the real code, invalidated
the original routing plan and produced the constraints below. They are recorded
here because each one is a way the feature can lose data if it is forgotten.

1. **Request routing does not change.** An earlier draft proxied the whole S3
   request to a member of the bucket's metadata shard. That cannot work: data
   placement and metadata placement are two independent constraints, and the
   handler allows exactly one proxy hop, so the bytes would land where no read
   path looks. The hop instead lives **inside the metadata store** as a
   store-level RPC to the shard leader.
2. **Unavailable is not empty.** A shard lookup that cannot be served returns an
   explicit error, never a nil result. Metadata is authoritative for
   reconciliation, so "I could not ask" being read as "it does not exist" is how
   orphan reclaim deletes live data.
3. **Only a founding member may bootstrap a shard.** Raft's bootstrap check looks
   only at local state, and pre-vote makes incumbents invisible to a node that is
   not in their configuration. Without the founder and epoch check, a node added
   later would bootstrap a rival, empty group for a shard that already exists,
   and that group would answer authoritatively that the shard is empty.
4. **Each shard needs its own membership reconciler.** Removing a node updates
   the control group only, so without reconciliation every shard configuration
   keeps a dead voter forever.
5. **Shard transports need a `ServerAddressProvider`.** Raft addresses are pod
   IPs, and auto-join re-announces into the control group only, so a rolling
   restart would otherwise strand every shard member.
6. **Multiplexing marker on shard dials only.** Shard traffic is framed with a
   marker byte sequence that a Raft RPC type byte can never collide with, and it
   is sent only when dialling a shard, so that a node running an older build
   still accepts control-group connections during an upgrade.
7. **Every background subsystem must go through the store interface.** Lifecycle,
   tiering, search, rebalance, the scanner, backup, inventory and batch
   operations hold the concrete local store. Left as they are, each would see an
   empty object space once the objects move into shards, and several of them
   delete things. Conversion is a precondition for enabling sharding, and an
   unconverted subsystem must refuse to start rather than run blind.

### Explicitly out of scope for the first version

- **Resharding.** The shard count is fixed when the map is created. Changing it
  means moving metadata between Raft groups while writes continue, which is a
  larger problem than the one this solves.
- **In-place migration from an unsharded cluster.** Enabling sharding on a store
  that already holds object metadata is refused at startup rather than attempted:
  those records would stay in the control group, which nothing reads once objects
  route to shards, so every object would report missing while its metadata and
  its bytes both still exist. Copy the objects into a new sharded cluster with
  any S3 client instead. A future offline converter would also have to purge the
  object records from the control store, or its snapshots keep shipping stale
  entries to every node that joins later.

## Delivery order

| Phase | Content | State |
| --- | --- | --- |
| P0 | Safety work that stands on its own: checked metadata writes, fail-closed reclaim, batched Raft application | Done |
| P1 | Shard map as committed control state, epochs and founders, CLI inspection | Done |
| P2 | Multi-group runtime: transport demultiplexing, per-group stream layers, group-addressed apply | Done |
| P3 | Shard groups holding real object metadata, the store-level hop, shard-leader listings | Done |
| P4 | Per-shard membership reconciliation | Done |
| P5 | Background subsystems and admin plane, control group on the shared transport, shard counts above 1 enabled | Done |

### P1, the assignment

The assignment is a Raft-committed record carrying a creation epoch and the
founding member set of every shard, and the state machine, not the proposing
node, refuses any later version that changes the shard count, the epoch or a
founding set. Membership is the only part a later version may change. The map is
computed once on the leader and only after ring membership has held still for 30
seconds, because the founding sets are permanent and a map computed from a
half-formed cluster would under-replicate those shards for the life of the
cluster.

### P2, the runtime

Several Raft groups run on one node, sharing the existing Raft port. A connection
announces which group it is for by writing four magic bytes and a shard id
immediately after connecting; a connection that announces nothing is control
traffic, exactly as every existing node already sends. That asymmetry is what
keeps a rolling upgrade safe, and it is unambiguous because Raft's first byte is
an rpcType in the range 0 to 4 and can never be the magic byte. Shard transports
resolve peer addresses through the control group rather than through their own
configuration, so a pod that restarts on a new IP is followed rather than
stranded. A shard's state machine accepts object commands only: anything else
arriving there is a routing bug, and applying it would put a cluster-wide record
into one shard where no other node could see it.

### P3, the store

`ShardedStore` answers the control-group surface exactly as before and routes the
object surface to the group that owns the bucket. When this node holds a copy of
that shard the call is local; when it does not, it is one store-level RPC to a
member. Writes are ordered by the shard's leader and nowhere else: a member that
is not the leader names the leader rather than forwarding, so a leadership flap
cannot make a write bounce between nodes. Listings are leader-only for the same
reason bucket listings go to the control leader on an unsharded cluster: a
follower can be an entry behind, and a listing served from one omits a key the
client was just told was stored.

Two rules are enforced rather than documented. A shard that cannot be asked
returns an error wrapping `ErrShardUnavailable`, never an empty result, and the
S3 layer turns that into a 503 rather than a 404. And a caller routing by a
different assignment is refused with a 409 rather than served, because writing a
bucket's records into the wrong group would hide them from every correct reader.

Multipart upload state and version tags stay in the control group. Both are
bounded by work in flight rather than by object count, and multipart records are
keyed by upload id, which carries no bucket to route by.

### P4, membership

Three jobs with one owner each. The planner, on the control-group leader,
recomputes which nodes should hold each shard from the ring and commits it; it
refuses a reassignment that would keep no current member of a shard, since that
does not move the metadata, it abandons it. The supervisor, on every node, starts
the groups this node is listed for and stops the ones it is not, but only once the
shard's leader has actually removed it from the group. The reconciler, on each
shard's own leader, drives that shard's Raft configuration towards the committed
list one member at a time, adds before removes, so a group that gains its
replacement before losing a member never drops below the quorum of members that
hold the data.

### P5, the rest of the system

Every background subsystem now takes the store interface rather than the concrete
local store, so lifecycle, tiering, search, the scanner, replication, backup,
inventory, batch operations, the rebalancer and the erasure healer all see the
routed object space. Full scans are partitioned across shard leaders: each leader
walks the shards it leads, so the union across the cluster is every object,
visited once, instead of every node walking everything.

The control group moved onto the shared transport at the same time, so there is
one transport path exercised by every deployment rather than a second one used
only by the rare sharded install. Deleting a bucket is two commits, object
metadata first, so a bucket record is never removed while records nothing owns
are left behind in a shard. `vaults3-cli cluster shards` shows the committed
assignment and the groups running on the node it is talking to, which is how the
reconciler's progress is visible.

## Enabling it

```yaml
cluster:
  metadata_shards: 8
  metadata_replicas: 3
```

The server refuses to start if `metadata_shards` is above 1 and its metadata
store already holds object metadata written unsharded. Those records live in the
control group, which nothing reads once objects route to shards, so every object
would report as missing while its metadata and its bytes both still exist. There
is no in-place migration: create a new sharded cluster and copy the objects
across, or set the value back to 1.

The assignment is not created until the cluster has at least `metadata_replicas`
nodes and membership has held still, so a cluster that is too small logs that it
is waiting rather than committing an assignment it would be stuck with.
