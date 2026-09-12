# Upgrading

Release-to-release upgrade notes, including the 4.4.56 security release.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

VaultS3 can check GitHub Releases once a day and show a **dashboard banner** when
a newer version is out. Updates only ever replace the binary or image, your
object data, metadata, and config are never touched.

**Docker (recommended): [Watchtower](https://containrrr.dev/watchtower/)** watches
for a new image and recreates the container. Your data volumes are preserved:

```yaml
services:
  vaults3:
    image: eniz1806/vaults3:latest
    volumes: [vaults3-data:/data, vaults3-meta:/metadata]
  watchtower:
    image: containrrr/watchtower
    volumes: [/var/run/docker.sock:/var/run/docker.sock]
    command: --interval 86400   # check daily
```

**Binary / systemd:** enable the built-in updater in `vaults3.yaml`. With
`apply: true` it downloads the new release for your platform, **verifies its
SHA-256 checksum**, swaps the binary, and restarts into the new version
(checked daily. Never auto-crosses a major version):

```yaml
auto_update:
  enabled: true     # daily check + dashboard banner
  apply: true       # also install automatically (omit for notify-only)
```

The current/latest version is also exposed at `GET /api/v1/version`.

## Rolling back

Two releases changed how bytes are laid out on disk, and an older server cannot
read what a newer one wrote in those formats. Neither is a reason to avoid
upgrading, but both are worth knowing before you plan a rollback.

- **SSE-C objects written after the release that introduced the chunked format**
  are refused by an older server with `403`. A clean refusal, not bad data.
- **Compressed objects written by 4.4.70 or later, with encryption also enabled**,
  are read as corrupt by a server older than 4.4.70. Compression used to run
  after encryption and now runs before it, and an older server unwraps the two in
  the order it expects. This one is worse than the SSE-C case because the client
  sees a checksum mismatch rather than a refusal. It was not documented at the
  time and should have been.

Compressed objects are not affected. The zstd seekable format added alongside
these keeps its seek table in a skippable frame, which any plain zstd decoder
ignores, so an older server reads those objects from the front correctly.

Where a rollback is not safe the fix is the same: roll forward rather than back,
or restore the data directory from a backup taken before the upgrade.

## Upgrading to 4.4.75

**Take this one if you run a cluster, especially with per-bucket encryption.** No
configuration or on-disk format changes, and single-node installs are unaffected.

### Per-bucket encryption on a cluster

Two bugs, both present in 4.4.74 and earlier, both fixed here.

**Reads mostly failed.** A clustered read of an encrypted object answered `503
SlowDown` on every holder, so a bucket with encryption enabled was effectively
unreadable. Nothing was lost and no data needs repairing, the objects were always
intact and are readable again as soon as you upgrade.

**One replica of each bucket's first object was written unencrypted.** This one
leaves something behind, because the object on disk stays as it was written. It
affects the first object written to a bucket after encryption was enabled, and
only one of its copies. To find them, search the data directory of each node for
a known plaintext string, or simply rewrite the affected objects:

```bash
# rewrite an object in place so every copy is stored encrypted
aws s3 cp s3://<bucket>/<key> s3://<bucket>/<key> --metadata-directive REPLACE
```

If a bucket holds anything you would rather not have had readable on a disk,
rewrite the affected objects as above. Rotating the bucket key does not help on
its own: a copy that was written in the clear was never sealed with that key, so
rotation leaves it exactly as readable as it was. Rewriting is what fixes it.

### SSE-KMS

Two more encryption problems are fixed in this release.

**SSE-KMS objects returned 503 on a cluster**, for the same reason encrypted
reads did above. Nothing was lost, and they read correctly once you upgrade.

**A server running per-bucket encryption accepted `SSEAlgorithm: aws:kms` and
encrypted nothing.** Only a per-bucket AES256 key encrypts anything in that mode,
so such a bucket reported itself as KMS encrypted while every object and every
replica sat on disk in the clear. That configuration is now refused outright.

If you have a bucket in that state, everything in it is plaintext, not just the
first object. Check with:

```sh
aws s3api get-bucket-encryption --bucket <bucket>
```

If it reports `aws:kms` on a server running `encryption.per_bucket: true`, that
bucket was never encrypted. Decide whether you want it encrypted, and if so
re-create it with `AES256` and copy the objects across, then treat the originals
as exposed. To use real SSE-KMS instead, set `encryption.kms` in the server
config, which is a server-wide mode and not a per-bucket one.

**Two behaviour changes come with the fix.** Enabling encryption on a bucket now
waits for the rest of the cluster to apply the change before returning, which
adds roughly 100 ms to that one call. And a node that does not yet hold a
bucket's encryption key now answers a write with `503 SlowDown` rather than
storing the object unencrypted. Every S3 SDK retries that on its own, and the
retry lands once the key arrives.

Clusters now repair replica counts on their own. Copies were only ever placed
when an object was written, so a node lost for good left every object that had a
copy on it one copy short, quietly, with nothing to put it back. Rebalance did
not cover this, it moves objects whose owner changed rather than objects that are
short of copies, and the lost-server runbook in `docs/SCALING.md` used to say
otherwise.

**If you have replaced or lost a cluster node on an earlier version, run one pass
after upgrading** and let it settle, because the copies missing from that event
are still missing:

```bash
vaults3-cli cluster repair
vaults3-cli cluster repair --status    # repeat until `repaired` stays at 0
```

`--status` reports `undecidable` when a holder could not be reached, which means
nothing was concluded and nothing was copied. That is expected while a node is
down and should fall to 0 once the cluster is whole. It reports `unrecoverable`
when no node still has the data, and names those keys in the server log. Nothing
is ever deleted in either case.

The scan runs every `cluster.repair.interval_secs` (600 by default), throttled by
`cluster.repair.max_bandwidth_mbps` (50). Set the interval negative to turn it
off. Erasure-coded buckets are untouched by it, they are still repaired from
parity by `erasure.heal_interval_secs`.

## Upgrading to 4.4.74

**Worth taking if you run on spinning disks or have a large object count.**
Nothing to change, no configuration or on-disk format changes.

Startup no longer stalls while the search index is built. On a reporter's HDD
with 250,000 objects that was 5 minutes 26 seconds of the container sitting
unhealthy, and is now under 8 seconds. The server also warns at startup when the
index is truncated, which happens whenever you hold more objects than
`memory.max_search_entries` allows, so search results were already incomplete and
are now saying so.

Two search behaviours changed, both on the dashboard search box and the
`/api/v1/search` endpoint. Plain terms no longer match an object's ETag, since a
short hex term matched unrelated objects by coincidence, and ETag lookup moved to
an `etag:` prefix filter. A bare `type:` or `etag:` with no value now behaves as
an empty query rather than matching everything.

## Upgrading to 4.4.73

**Upgrade now if you run a cluster and use SSE-C.** Nothing to change, no
configuration changes.

Server-side encryption with a customer-provided key was unreadable on a
multi-node cluster from 4.4.70 through 4.4.72: every GET was refused with
`503 SlowDown`. Writes were fine and no data was ever at risk, only the read path
was wrong, so objects written during that window read correctly as soon as you
upgrade. Single-node servers were never affected.

Two read paths also stopped expanding whole objects into memory: a range read of
a compressed object now decompresses one frame rather than the object, and SSE-C
reads decrypt a chunk at a time. Objects written by earlier versions still read
with no migration, but they keep the format they were stored in, so the benefit
appears as data is rewritten.

Read "Rolling back" below before planning a downgrade. SSE-C objects written by
this release cannot be read by an older server.

## Upgrading to 4.4.72

**Nothing to change.** No configuration, API or on-disk format changes.

If you run `encryption.per_bucket`, this release is worth taking. Reads of
objects in buckets that never opted in no longer buffer the whole object, objects
over 1 GiB in those buckets are no longer served truncated, and the
`x-amz-server-side-encryption` response header now reflects whether the bucket is
actually encrypted rather than whether the server has encryption switched on. If
you also set `encryption.legacy_key`, plaintext objects in opted-out buckets that
previously returned `404 NoSuchKey` are readable again, with no migration and no
rewrite: the data was always on disk, only the read path was wrong.

## Upgrading to 4.4.71

**Nothing to change.** No behaviour, configuration or API changes.

If you use the external authorization webhook, the server is now clearer about
who it evaluates. The admin identity is never sent to the hook, and a login is
authentication rather than an access decision, so testing with the admin
credential sends the endpoint nothing at all. That was true before and is
unchanged. The difference is that the server now says so in its log the first
time it happens, names the audience in its startup line, and repeats it in
`vaults3 diagnose`. Test the hook with a non-admin access key.

## Upgrading to 4.4.70

**Nothing to change, but read this if you run compression with encryption.**

Compression now runs on plaintext, before encryption, instead of being handed
ciphertext. Objects written by earlier versions are still read correctly and
nothing has to be rewritten, but they keep the size they were stored at. Only
objects written from this release on get smaller, so the numbers on an existing
deployment move as data is rewritten rather than at upgrade time.

Two correctness fixes need no action. A delete marker placed over an object that
predates versioning on its bucket is now reversible, and objects already orphaned
by the old behaviour remain reclaimable with `vaults3-cli storage reclaim`. In a
cluster, a node that holds bytes older than its metadata now routes the read to a
holder that has the current data, and answers `503 SlowDown` if none can be
reached yet, which S3 SDKs retry automatically.

## Upgrading to 4.4.69

**Nothing to change.** The external authorization webhook added in this release
is off unless you set `external_auth.enabled`, and every other behaviour is
unchanged.

If you do enable it, read [the guide](ACCESS-CONTROL.md#external-authorization-webhook)
first. Two defaults are deliberate and worth understanding before you deploy:

- **Fail-closed.** An endpoint that cannot be reached refuses requests rather
  than serving them, so your authorization service becomes a dependency of your
  storage. `fail_open: true` inverts that, at the cost of an outage silently
  widening access.
- **`cache_ttl_secs: 10`.** Leave it on. With caching off, throughput becomes
  whatever your endpoint can serve: measured at 220 req/s against a simple
  endpoint versus 1960 with the default, a 9x drop. The server warns at startup
  if you turn it off.

The admin identity is never sent to the webhook, on either the S3 or the
dashboard path, so a misconfigured endpoint cannot lock you out of your own
server.

## Upgrading to 4.4.68

**Dependency security updates only. Nothing to change.** No configuration, API,
or behaviour changes, so this upgrade is a straight swap of the binary or image.

- `github.com/rabbitmq/amqp091-go` 1.10.0 to 1.13.0 (GHSA-6c5v-hqjr-5xxp). A
  malicious or compromised AMQP broker could send content body frames larger
  than the negotiated `frame_max` and drive the client into unbounded memory
  use. This is only reachable if you enable AMQP event notifications and point
  them at a broker you do not control.
- `golang.org/x/crypto` 0.53.0 to 0.56.0, clearing three `x/crypto/ssh`
  advisories. VaultS3 uses this module only for `acme/autocert`, so none of the
  three were reachable from VaultS3 code.
- Dashboard build dependencies `browserslist` and `postcss-selector-parser`.
  Build-time only, never part of the shipped bundle or the server binary.

## Upgrading to 4.4.67

**Anonymous bucket policies are now enforced per object key.** This closes a
disclosure where a policy scoped to one prefix published the whole bucket, and it
makes three cases stricter. Check your public bucket policies if any of these
describe yours:

- A Resource naming a prefix, such as `arn:aws:s3:::bucket/public/*`, now
  publishes that prefix **only**. Previously it published every key in the
  bucket. If you were relying on the wider access, widen the Resource to
  `arn:aws:s3:::bucket/*` deliberately.
- A statement with **no `Resource` field** no longer grants anything. `Resource`
  is required in an S3 bucket policy, and treating a missing one as "everything"
  turned a malformed policy into a public grant.
- A bare bucket ARN, `arn:aws:s3:::bucket`, no longer covers the objects in the
  bucket for `s3:GetObject`. Use `arn:aws:s3:::bucket/*` for object access. The
  bare form remains correct for `s3:ListBucket`.

Authenticated access is unchanged: the IAM path already matched the full object
ARN. Nothing needs to change in your config files.

## Upgrading to 4.4.65

**Rate limiting is now on by default.** Nothing is required of you, but it is a
behaviour change worth knowing about:

- If your `vaults3.yaml` already has a `rate_limit` block, it is respected
  exactly as written. An explicit `enabled: false` still turns it off.
- If your config has no `rate_limit` block, or you run with no config file at
  all, you now get 2000 requests per second per IP and per access key, with a
  4000 burst.
- Docker users who do not mount their own config pick up the new defaults with
  the new image. The Helm chart and the Kubernetes manifests already enabled
  rate limiting and move from 200 to the same 2000.

The ceiling is set far above real traffic: a saturating 8-thread `boto3` client
measures around 1300 requests per second, comfortably inside it. If you push
more than that through a single endpoint, or you sit behind a reverse proxy
where every client shares one address for limiting purposes, raise
`rate_limit.requests_per_sec` to suit. See
[rate limiting](CONFIGURATION.md#rate-limiting).

## Upgrading to 4.4.56 (security release)

**This release closes 14 findings from an external security assessment, several
of them remotely exploitable against a default deployment.** The full list is in
[CHANGELOG.md](../CHANGELOG.md). Upgrading is strongly recommended, and a few things
change behaviour, so read this first.

### Before you upgrade

**Set `cluster.secret` on every node of a clustered deployment.** This is the one
change that stops a server booting. Inter-node endpoints authenticate with it and
now fail closed, so a clustered node with no secret exits at startup with an
error naming the setting. Use the same value on every node, ideally from a secret
manager. The Helm chart already derives one, so chart users need do nothing.

```yaml
cluster:
  enabled: true
  secret: "a-shared-value"      # or VAULTS3_CLUSTER_SECRET
```

Single-node deployments are unaffected.

### After you upgrade

**Rotate the admin credentials if this installation ever ran with
`vaults3-secret-change-me`.** 4.4.55 stopped shipping that secret, but an
installation that already booted with it has it persisted, and persisted
credentials win over configuration, so upgrading does not replace it. Change it
from the dashboard, or set `VAULTS3_ACCESS_KEY` and `VAULTS3_SECRET_KEY`.

**Everyone is logged out once.** The console signing key is now random per
installation instead of derived from the admin secret, so existing dashboard
sessions stop working. Users log in again. Nothing else is affected.

### If something stops working, this is probably why

Each of these was a security fix, and each can look like a regression.

| Symptom | Cause | What to do |
|---|---|---|
| A non-admin dashboard user gets 403 on a bucket | The console now enforces IAM policies, as the S3 API always did. Any authenticated user used to reach any bucket | Give the user a policy covering the buckets they need |
| OIDC login fails | The implicit flow is disabled. The authorization-code flow, which the dashboard uses, is unaffected | Use the code flow, or set `oidc.allow_implicit_flow: true` if your provider supports nothing newer |
| Per-bucket panels in Prometheus go blank | Anonymous scrapes no longer receive the per-bucket series, which carry bucket names, sizes and counts | Send `X-Cluster-Secret` with the scrape, or set `metrics.public_bucket_labels: true` |
| A migration from an internal source fails | Loopback, private and link-local destinations are blocked by default, because a caller-supplied endpoint was a server-side request primitive | Re-run the job with private sources allowed |
| An STS credential has less access than before | Session policies are now enforced. A scoped session used to inherit the full permissions of the user it came from | Widen the session policy if the access was intended |
| An STS request returns 403 | `X-Amz-Security-Token` is now verified. Standard SDKs send it automatically | Send the session token that was issued with the key |
| A copy returns 403 | A copy now requires `s3:GetObject` on its source, not only write on the destination | Grant read on the source bucket |
| An IAM policy now denies what it used to allow | `Condition`, `NotAction` and `NotResource` are now evaluated. They used to be ignored, so a restriction you wrote was not being applied | The policy is now doing what it says. Adjust it if the restriction was not intended |
| Automation gets 429 on login | Ten failed logins from one address earn a fifteen-minute lockout | Fix the credentials the automation is using |

## Upgrading to 4.4.55

**A server that has never had an admin secret now generates one** rather than
falling back to the example secret from these docs. If your installation already
has credentials, whether persisted, configured, or set from the dashboard,
nothing changes: those still win. Only a genuinely new installation gets a
generated secret, which it prints once at startup and then stores.

If you were relying on `vaults3-secret-change-me`, set `VAULTS3_ACCESS_KEY` and
`VAULTS3_SECRET_KEY` explicitly, or read the generated secret from the first
start's output.

## Upgrading to 4.4.54

One behaviour change is worth knowing before you upgrade, because it is visible
in your storage numbers:

**On a versioning-enabled bucket, a multi-object delete now writes a delete
marker and keeps the data**, which is what a single `DELETE` has always done and
what S3 specifies. Before this it removed the object outright, so a bulk delete
freed space. After upgrading it will not, and the space is released when the
versions are expired, either by a lifecycle rule (`NoncurrentVersionExpiration`)
or by deleting versions explicitly. Buckets without versioning are unaffected.

Nothing else needs action. Metadata sharding is off unless you set
`cluster.metadata_shards` above 1, and the server refuses to start if you set it
on a node whose metadata store already holds objects, so an upgrade cannot enable
it by accident.
