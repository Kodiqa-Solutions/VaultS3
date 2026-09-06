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
