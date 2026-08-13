# VaultS3 Benchmarks

This page describes **how to benchmark VaultS3** reproducibly and provides a
results template. The numbers tables are intentionally left as placeholders, 
fill them in from a controlled run on your own hardware. **Do not cite numbers
you haven't measured.** Throughput depends heavily on disk, CPU, network, object
size, and concurrency, so a number without its methodology is meaningless.

> If you run these and want to contribute results, open a PR adding a row with
> your hardware spec, that's far more credible than vendor-claimed figures.

---

## 1. Built-in drive benchmark (`/speedtest`)

VaultS3 ships a quick single-object drive benchmark. It writes then reads a 64 MB
object through the storage engine and reports throughput. It measures **local
disk + engine overhead only**, no network, no concurrency, no S3 protocol.

```bash
# Get a dashboard JWT first (admin login), then:
curl -s -X POST http://localhost:9000/api/v1/speedtest \
  -H "Authorization: Bearer $TOKEN" | jq
```

```json
{
  "writeThroughputMBps": 0.0,
  "readThroughputMBps": 0.0,
  "duration": "0s"
}
```

Use this for a fast "is my disk healthy?" check, **not** for comparisons against
other systems, it doesn't exercise the S3 API path or concurrent clients.

---

## 2. Comparative S3 throughput (recommended: `warp`)

For apples-to-apples comparisons against MinIO / SeaweedFS / Garage, drive all
systems with the same S3 benchmark tool, on the same machine, with the same
object size and concurrency. [`warp`](https://github.com/minio/warp) is the
standard.

```bash
# Example: mixed read/write, 4 KB–10 MB objects, 20 concurrent clients, 60s.
warp mixed \
  --host=localhost:9000 \
  --access-key=vaults3-admin \
  --secret-key=vaults3-secret-change-me \
  --obj.size=1MiB \
  --concurrent=20 \
  --duration=60s \
  --bucket=bench
```

Run the identical command against each system (only `--host`/keys change) and
record the reported GET/PUT throughput and latency percentiles.

`s3-bench` and `hyperfine`-wrapped `aws s3 cp` loops are reasonable alternatives.
the key is **same tool, same flags, same box** for every system under test.

---

## 3. Memory footprint

VaultS3's headline claim is low RAM. Measure resident set size (RSS) under a
steady workload, not at idle:

```bash
# Native process:
ps -o rss= -p "$(pgrep -f vaults3)" | awk '{printf "%.1f MB\n", $1/1024}'

# Docker:
docker stats --no-stream --format '{{.MemUsage}}' vaults3
```

Capture RAM **while `warp` is running**, so the number reflects real load.

**In a container, measure `anon`, not `memory.current`.** `docker stats`, cgroup
v2 `memory.current` and Prometheus `container_memory_usage_bytes` all include
**page cache**, which a server writing object data fills to the limit as a matter
of course; the kernel reclaims it under pressure. Use `anon` from
`/sys/fs/cgroup/memory.stat`:

```bash
docker exec vaults3 grep -E '^(anon|file) ' /sys/fs/cgroup/memory.stat
```

The difference is not academic. Measured at a 4 GiB limit with 64 MiB objects at
64 concurrent uploads, a build that **OOM-killed** reported 1804 MiB of
`memory.current` while a healthy one reported 4094 MiB — the counter ranked the
broken build ahead of the working one. Their `anon` figures were 2253 MiB and
20 MiB respectively, which is the number that decides an OOM kill.

**Object size and concurrency drive the peak, not the object count.** A steady
workload of small objects sits near the idle figure; large objects at high
concurrency are what set the ceiling, because each in-flight request holds
buffers proportional to the part or object it is moving. Measure at the object
size and concurrency you actually run, and size container limits from that number
rather than from an idle reading.

**Benchmark the features you run, not just the defaults.** Wrapper engines change
the memory profile completely, and a default-config run will not show it. One
node, 256 MiB objects, 3 GiB limit, peak `anon` by concurrent readers:

| configuration | c=1 | c=4 | c=8 |
|---|---|---|---|
| default (no encryption/compression) | 15 MiB | 15 MiB | 16 MiB |
| compression on | 33 MiB | 62 MiB | 98 MiB |
| erasure coding on | 719 MiB | 719 MiB | 719 MiB |
| encryption on, 4.4.52 | 847 MiB | 2348 MiB | **OOMKilled** |
| encryption on, 4.4.53 | 19 MiB | 25 MiB | 29 MiB |

The 4.4.52 encryption row is issue #49: reads scaled with object size times
concurrency. It is also why an encryption benchmark must vary concurrency, since
the c=1 figure alone looks merely large rather than fatal.

Read the erasure row carefully: it is high but **flat**, because that cost is the
write path (which still assembles the object to shard it), not the read. Only a
row that climbs with concurrency is a per-reader cost. Combinations behave the
same way, measured on 4.4.53 with a 128 MiB object at 8 readers: encryption plus
compression 45 MiB, encryption plus erasure 348 MiB and flat from c=1.

Comparing a memory number across versions is only meaningful with the same object
size, concurrency and codec on both sides.

**A single node and a cluster member are not the same measurement.** Two figures
for 64 MiB objects, both `anon`, both under sustained PUT load:

| deployment | concurrency | peak `anon` |
|---|---|---|
| single node, 4 GiB limit | 64 | **~20 MiB** |
| 12-pod cluster, 8 GiB per pod (user-reported, 4.4.51) | 48 | **~1.85 GiB** |

The gap is not explained by object size or codec, so treat the single-node number
as a floor rather than a guide for a cluster: a clustered node also forwards
bodies to the owner and fans replicas out to peers. **Size cluster pods from your
own measurement, not from the single-node figure or the README's small-deploy
claim.**

**Startup is a separate peak from steady state.** A node installing a Raft
snapshot on join or restart allocates for that restore; before 4.4.51 it did so in
one transaction and scaled with the total object count (2175 MiB at 1.6M objects),
which OOM-killed pods before they served anything. From 4.4.51 it is bounded
(~66 MiB at the same size). If you are sizing limits, measure a **restart**, not
just a load test.

---

## 4. Results template

Fill these in from your own controlled run. Replace every `TBD`. State the
methodology so the numbers are reproducible.

**Environment:** `TBD` (CPU, cores, RAM, disk type, OS, filesystem, network)
**Tool / workload:** `warp mixed, 1 MiB objects, 20 concurrent, 60s` (or your own)
**Date / versions:** VaultS3 `TBD`, MinIO `TBD`, SeaweedFS `TBD`, Garage `TBD`

| Metric | VaultS3 | MinIO | SeaweedFS | Garage |
|---|---|---|---|---|
| GET throughput (MB/s) | TBD | TBD | TBD | TBD |
| PUT throughput (MB/s) | TBD | TBD | TBD | TBD |
| GET p99 latency (ms) | TBD | TBD | TBD | TBD |
| PUT p99 latency (ms) | TBD | TBD | TBD | TBD |
| RSS under load (MB) | TBD | TBD | TBD | TBD |
| Idle RSS (MB) | TBD | TBD | TBD | TBD |

---

## 5. Reporting honestly

- Always publish the **hardware, tool, flags, and date** alongside the numbers.
- Run each system several times and report medians, not best-case cherry-picks.
- If VaultS3 loses on a metric, say so, credibility compounds. The pitch is
  "lightweight and batteries-included in one binary," and RAM/footprint is where
  that shows. Raw peak throughput on big iron is not the claim.
