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

Some releases changed how bytes are laid out on disk, and an older server cannot
read what a newer one wrote in those formats. None is a reason to avoid
upgrading, but all are worth knowing before you plan a rollback.

- **Encrypted objects, erasure coded objects and cluster Raft snapshots written
  by 5.0.0** cannot be read by 4.4.79 or older. See
  [Rolling back from 5.0.0](#rolling-back-from-500).

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

## Upgrading to 5.0.0

A security and durability release. There is no migration to run, but it changes
how IAM actions are checked, writes formats 4.4.79 cannot read, and several of
the bugs it fixes could have already exposed or damaged data. Read the whole
section before upgrading. Data written by 4.4.79 is read by 5.0.0 as it is.

### Before you upgrade: custom IAM policies

Every S3 call is now authorized as its own AWS action. Before 5.0.0 bucket
configuration writes were checked as `s3:CreateBucket`, configuration reads as
`s3:ListBucket`, object sub-resource writes as `s3:PutObject`, their reads as
`s3:GetObject`, and multipart part listing and abort as `s3:*`. **A custom policy
that relied on that is refused after upgrading.** Add the actions your clients
use:

| Call | Action needed now |
|------|-------------------|
| Object tagging | `s3:PutObjectTagging`, `s3:GetObjectTagging`, `s3:DeleteObjectTagging` (the `...VersionTagging` forms with `versionId`) |
| Object ACL | `s3:PutObjectAcl`, `s3:GetObjectAcl` (`...VersionAcl` with `versionId`) |
| Retention, legal hold | `s3:PutObjectRetention`, `s3:GetObjectRetention`, `s3:PutObjectLegalHold`, `s3:GetObjectLegalHold` |
| Shortening or removing GOVERNANCE | `s3:BypassGovernanceRetention`, plus the `x-amz-bypass-governance-retention: true` header |
| `GetObjectAttributes` | `s3:GetObjectAttributes` (`s3:GetObjectVersionAttributes` with `versionId`) |
| Multipart | `s3:PutObject` to start, upload and complete, `s3:ListMultipartUploadParts` to list parts, `s3:AbortMultipartUpload` to abort, `s3:ListBucketMultipartUploads` to list uploads |
| Copy | `s3:GetObject` on the source, `s3:GetObjectVersion` when it names a `versionId` |
| Versions, location | `s3:ListBucketVersions`, `s3:GetBucketLocation` |
| Bucket configuration | `s3:Put...`/`s3:Get...` for each: `BucketPolicy` (and `DeleteBucketPolicy`), `BucketVersioning`, `LifecycleConfiguration`, `BucketCORS`, `EncryptionConfiguration`, `ReplicationConfiguration`, `BucketWebsite` (and `DeleteBucketWebsite`), `BucketNotification`, `BucketTagging`, `BucketAcl`, `BucketLogging`, `BucketPublicAccessBlock`, `BucketObjectLockConfiguration`, and VaultS3's own `BucketQuota` and `BucketDurability`. A configuration delete needs the `Put` action unless listed otherwise. |
| `RestoreObject` | `s3:RestoreObject` |

The built-in `ReadOnlyAccess` (now `s3:Get*`, `s3:List*`) and `ReadWriteAccess`
(adds tagging, retention, legal hold, ACL, abort and restore) are updated at
startup, but only if they still hold exactly the old built-in text. A copy you
edited is left alone. Note that `ReadOnlyAccess` is now wider: it also reads
bucket policies, ACLs, tags and other configuration. Policies using wildcards
such as `s3:*` or `s3:Get*` need no change.

### Before you upgrade: other changes that can stop a working setup

- **Behind a reverse proxy.** `aws:SourceIp`, IP allowlists, per-IP rate limits
  and logs now use the TCP peer, so behind nginx every client looks like the
  proxy. List the proxy in `server.trusted_proxies` (IPs or CIDRs, or
  `VAULTS3_TRUSTED_PROXIES` comma separated), and its `X-Forwarded-For` is
  believed. Without it, a condition on client addresses refuses everyone.
- **Webhooks and lambda functions on your own network.** Calls to loopback,
  private, link-local and carrier-grade NAT (`100.64.0.0/10`) addresses are
  refused, which includes a docker-compose service. Set
  `notifications.allow_private_webhooks: true` (`VAULTS3_ALLOW_PRIVATE_WEBHOOKS`)
  or `lambda.allow_private_endpoints: true`
  (`VAULTS3_LAMBDA_ALLOW_PRIVATE_ENDPOINTS`). Cloud metadata addresses
  (`169.254.0.0/16`, `fd00:ec2::254`) stay refused either way, checked on the
  address actually dialled. Redirects are no longer followed.
- **OIDC.** A login is refused unless the ID token says `email_verified: true`.
  If your provider does not send the claim and only issues verified addresses,
  set `oidc.accept_email_without_verified_claim: true`.
- **Non-admin dashboard users** can reach only buckets and objects, and stats,
  activity and cost filtered to their buckets. Creating a bucket needs
  `s3:CreateBucket`. Their IP allowlist now applies to the dashboard too.
- **New IAM user, group and policy names** must match `[\w+=,.@-]`, and policy
  documents must parse. Existing names keep working.
- **Lifecycle expiry in a versioned bucket** now writes a delete marker instead
  of deleting the object. Versioned objects are no longer tiered.
- **Dashboard rollback** needs versioning enabled on the bucket.
- **Presigned URLs** need `X-Amz-Expires`, and every signed request needs a date
  within 15 minutes and a signed `host`. Mainstream SDKs already do this.
- **Multipart complete** refuses unsorted or duplicate parts and parts without
  an ETag.
- **`/cluster/status`** needs the `X-Cluster-Secret` header. Update external
  monitoring, or use `vaults3-cli cluster status`.
- **`/health` and `/ready`** now answer `503` when the metadata store does not
  answer within 3 seconds.
- **Message queue backends** (Kafka, NATS, Redis, AMQP, Postgres, Elasticsearch)
  now get events from every bucket, not only buckets with a webhook. Expect more
  volume. When the queue is full an event is dropped with a warning.
- **Vault KMS.** A config with `encryption.kms.provider: vault` refuses to start.
  It never worked, so no data was written with it. Use `local`.
- **`vaults3-cli`**: `cluster leave` and `cluster decommission` need `--yes`
  outside a terminal, `object get` needs `--force` to overwrite a file, and
  `cluster rebalance` is retired.
- **Self-update.** From 5.0.0 the updater installs a release only if
  `checksums.txt.sig` verifies against the key built into the binary. Release
  binaries carry it. A binary built from source does not, and refuses to update
  itself: replace the binary or image instead.
- **Self-update will not install 5.0.0 by itself.** The updater never crosses a
  major version unless `auto_update.allow_major` is `true`, so a 4.4.x server
  with `auto_update.apply` on stays on 4.4.x. That is on purpose: read this
  section, then upgrade by hand (new binary, package or image). Docker and Helm
  users pull or set the `5.0.0` tag.
- **Small writes are slower.** Every object is now fsynced, and its directory
  after the rename, so a write acknowledged with `200` survives a power loss.
  There is no setting to turn it off. Measure your own small-object workload
  before and after.

### Rolling back from 5.0.0

5.0.0 reads everything 4.4.79 wrote. The reverse is not true:

- **Encrypted objects** written by 5.0.0 use stream format v2. 4.4.79 refuses
  them with `unsupported stream format 2`. This covers every encryption mode:
  the server key, per-bucket keys, SSE-KMS and SSE-C.
- **Erasure coded objects** written by 5.0.0 keep their shards in a generation
  directory (`.ec/<key>/gen-.../`). 4.4.79 looks for them in the old place,
  finds none, and cannot read or heal the object.
- **Raft snapshots** written by a 5.0.0 cluster node use format v2. A 4.4.79
  node clears its whole metadata database before reading one, then fails. **Do
  not downgrade a cluster node.** Rebuild it from the cluster instead.

Once 5.0.0 has taken writes, roll forward rather than back, or restore a
backup taken before the upgrade.

### Clusters: rolling upgrade

- **Upgrade every node of a cluster before creating or changing access keys or
  IAM users.**
- **Upgrade the followers first and the leader last.** A 5.0.0 leader sends
  v2 snapshots, and a lagging 4.4.79 follower that receives one wipes its
  metadata and cannot restore it. A 4.4.79 leader's v1 snapshots are read by
  5.0.0.
- **Keep the mixed period short and avoid versioned writes during it.** 5.0.0
  demotes the previous latest version inside the replicated command, and 4.4.79
  demoted it in the S3 handler. A versioned write while both run can leave two
  latest versions on the old nodes, until the key is written again.
- **Push replication** moved to a queue on each node. On a cluster, 4.4.79 and
  earlier lost about half of all push replication events: an acknowledgement
  from one node deleted an unrelated event on the others. In our test a 4.4.79
  cluster delivered 22 of 45 objects to its peer, and 5.0.0 delivered 45 of 45.
  Objects written while any node still runs the old release are exposed to the
  same loss, and nothing sends a lost event again. After every node runs
  5.0.0, copy each replicated bucket to its peer once, for example
  `aws s3 sync s3://<bucket> s3://<bucket>` with the source and target
  endpoints, or `rclone sync`. A single node was not affected.
- **Client IP rules on forwarded requests.** An old node forwards requests
  without the client address, so a policy or allowlist naming client addresses
  can refuse requests forwarded by it until every node is upgraded.
- **TLS between nodes.** With `server.tls.enabled`, 4.4.79 nodes called each
  other over plain http on a port that only speaks TLS. In our test a 4.4.79
  cluster could not form through `join_addr`, and S3 requests that one node
  forwarded to another failed. 5.0.0 nodes call each other over https, and a
  5.0.0 TLS cluster forms and serves every request. While versions are mixed,
  5.0.0 nodes can join a 4.4.79 leader, but requests an old node forwards to a
  new one still fail. Upgrade every node in one short window, and send clients
  to upgraded nodes meanwhile. Clusters without `server.tls.enabled` are not
  affected.
- **Helm.** The chart now keeps a separate `cluster-secret` in its Secret. An
  upgraded release keeps the value it was using (the admin secret), so nodes
  keep agreeing. With `helm template`, Argo CD or anything that renders without
  cluster access, the chart cannot read the existing Secret and generates a new
  cluster secret on every render, so restarted pods disagree with the rest. Set
  `auth.clusterSecret` (and `auth.secretKey`), or use `existingSecret`, for
  those workflows.
- **Rebalance is retired.** To remove a node: `vaults3-cli cluster decommission
  <node>` (drains it), `vaults3-cli cluster leave <node> --yes`, then
  `vaults3-cli cluster repair` and `cluster repair --status` until `repaired`
  settles at 0. This needs `placement.replica_count` of 2 or more.
- **Object lock on buckets created before 5.0.0.** The flag that marks a bucket
  object-lock enabled was recorded only on the node that served the request.
  Other nodes reported no object lock configuration for the bucket and skipped
  the catch-up before a lock decision. Retention set on objects was replicated
  and enforced. After every node runs 5.0.0, send each locked bucket's object
  lock configuration again (`aws s3api put-object-lock-configuration`, the same
  configuration it has) so every node records the flag.
- **Node-local data on restart.** Before 5.0.0 a restart rolled a node's
  multipart uploads, replication queue, audit trail, bucket snapshots and saved
  admin credentials back to the last Raft snapshot. If a node's admin
  credentials or in-flight uploads changed after a restart, that was why. 5.0.0
  keeps them.

### Check: stored admin credentials

Saved admin credentials win over `VAULTS3_ACCESS_KEY` and `VAULTS3_SECRET_KEY`,
so changing the environment never replaced them. If the server still uses an
example or leaked secret, set the new pair together with
`VAULTS3_ADMIN_CREDENTIALS_OVERRIDE=true` (or `auth.override_stored_credentials:
true`) for one start, then remove the flag. Kubernetes users of
`deploy/k8s/quickstart.yaml` started with the public example password, check it
was changed.

### Check: backups

Backups before 5.0.0 held no `metadata.db` and nothing from versioned buckets.
Take a new full backup after upgrading. It now includes `_vaults3/metadata.db`.

### Check: lifecycle rules

Before 5.0.0 a lifecycle rule with a tag filter, an `And` filter, a size filter
or a rule-level `<Prefix>`, or a configuration with several rules, was accepted
and stored as a single rule with **no filter**. Such a rule expires every object
in the bucket once it is old enough.

For every bucket with a lifecycle configuration, read it back:

```bash
aws --endpoint-url $EP s3api get-bucket-lifecycle-configuration --bucket <bucket>
```

A rule with an empty prefix that you did not write as "the whole bucket" is one
of these. Delete it or put the intended rule again. 5.0.0 stores a prefix rule
faithfully and refuses the other forms with `501 NotImplemented`.

### Check: per-bucket encryption keys

In per-bucket mode, sending `PutBucketEncryption` to a bucket that was already
encrypted, or sending `DeleteBucketEncryption`, threw the bucket's key away.
Terraform and other configuration tools send the first on every apply. Every
object encrypted before that call is unreadable, and the key is gone, so **those
objects cannot be recovered**. A GET of one fails partway through the response.

To find them, read a few older objects in each encrypted bucket. Nothing can
bring the key back, so restore those objects from a backup or from their source.
From 5.0.0 a repeated call keeps the key, and removing the configuration keeps
every object readable.

### Check: website buckets

A bucket with website hosting answered S3 API calls with no authentication:
listings, old object versions, and its policy and other configuration. If such
a bucket held anything you did not mean to publish, or older versions you
deleted for a reason, treat it as exposed. If access logging (`logging.enabled`)
was on, its log shows what was requested.

### Check: Snowball imports and POST uploads

Anyone allowed to write to one bucket could overwrite an object in any other by
naming it `../<bucket>/<key>` in a Snowball archive or a POST upload form. If
users you do not fully trust can write to any bucket, compare important objects
with their sources.

### What changed

- **Deleting a bucket** refuses while it holds any object, version or delete
  marker. A versioned bucket now has to be emptied first, as on AWS.
- **Website buckets** answer only plain GET and HEAD anonymously.
- **Bucket policy conditions** are enforced for anonymous requests. A policy that
  granted more than its conditions said now grants exactly what they say.
- **Lifecycle** refuses what it cannot apply instead of storing a wider rule.
- **Lambda triggers** write their output only to their own bucket. Reconfigure a
  trigger whose `output_bucket` names another bucket.
- **The inter-node port** requires the cluster secret for `/_replication/sync`.
- **Objects with a far-future Last-Modified** from Snowball or POST uploads are
  repaired when read, nothing to do.
- **The HashiCorp Vault KMS provider** is documented as not working. A server
  configured with it never started, so no data was written with it.

## Upgrading to 4.4.79

Access keys now each keep their own grant. Nothing to run, existing keys keep
exactly the access they have, but three things behave differently and one is worth
checking.

### Check: keys issued for users that had policies of their own

Before this release a key issued with no buckets selected got `s3:*` on every
bucket, and that grant was attached to the user. A user you had given
`ReadOnlyAccess` and then issued a key for could therefore write to every bucket,
through every one of its keys. **Upgrading does not narrow those keys**, because
removing access a working client may rely on is a decision for you, not for an
upgrade.

To find them, open **IAM > Users** in the dashboard and look for a user that has a
`key-policy-<user>` policy next to policies of its own, and check that policy's
resource under **IAM > Policies**. Once a new key has been issued for such a user,
the shared policy has been split into one `access-key-<access key>` copy per key:
a copy whose resource is `*` belongs to a key that reaches every bucket, and the
Access Keys page shows which user it belongs to. Delete those keys and issue new
ones. With nothing selected, a new key for that user gets exactly the user's
policies.

### What changed

- **A second key no longer changes the first.** Keys issued before 4.4.79 share one
  policy, `key-policy-<user>`. The first time a key is issued for that user after
  upgrading, every existing key gets its own copy of it, named
  `access-key-<access key>`, and the shared one is removed. Each old key keeps
  exactly the access it had.
- **A key for a user with policies of its own gets those policies, not every
  bucket**, when no buckets are selected. A user with no policies still gets every
  bucket, as before. Scripts calling `POST /api/v1/keys` can say which they want
  with `allBuckets` or `userPoliciesOnly`, and the response now reports `access`.
- **Deleting a user's last key keeps the user** unless issuing a key is what
  created it. Users created before 4.4.79 are recognised by the only shape key
  issuance gave them, the `key-policy-<user>` policy and nothing else.
- **Deleting a user deletes its access keys** and any session tokens issued from
  it. They used to stay behind and work again for a new user created with the
  same name. A script that deletes a user and expects its keys to survive needs
  to change, there is no way to keep them.
- A key's own policy cannot be deleted on its own, delete the key.
- **STS sessions.** A session token issued for a user without an inline policy
  inherits the user's policies. A user that exists only for its keys has none
  once those keys hold their own grants (every key issued from 4.4.79, and older
  keys once they are converted), so such a session can reach nothing. Attach
  a policy to the user, or pass an inline policy when issuing the session.

`vaults3-cli` gained `key create`, `key list` and `key delete`. `key create`
refuses a server older than 4.4.79, so upgrade the server before using it. The Docker
image now carries `vaults3-cli`, so `docker exec <container> vaults3-cli ...` works
with the container's own credentials.

## Upgrading to 4.4.78

**Only `vaults3-cli` changed.** The server is the same as 4.4.77, so there is
nothing to upgrade on it. Replace the CLI binary to get these fixes. The new CLI
works against a server you already run, tested back to 4.4.56.

### Check any user you deleted through the CLI

Before this release the CLI put a user name into the URL without escaping it, so a
`?` or `#` in the name cut it short and the request acted on a different user.
`vaults3-cli user delete 'a?b'` deleted the user `a` and reported `a?b` as deleted.
`user attach-policy` built its request the same way.

If you ever ran either command on a name containing `?` or `#`, list your users and
check that the right one was removed or given the policy. A user deleted this way
is gone and has to be created again, along with its policies and access keys. Names
without those two characters were never affected.

### Scripts that call the CLI

- `user delete` on a user that does not exist now fails with `user not found` and
  exit status 1, where it used to print "deleted" and exit 0. A teardown script
  that deletes users which may already be gone needs `|| true`.
- `user create` refuses `--access-key` and `--secret-key`. They were documented but
  never did anything, because the server generates every access key itself. Issue a
  key for the user from the dashboard, under Access Keys.
- `user create`, `user delete` and `user attach-policy` refuse a name containing `/`.
  The API cannot route such a name, so a user created with one could never be
  managed again.

`user create`, `user attach-policy`, `user list`, `replication status` and
`replication queue` were broken on 4.4.77 and earlier and work now. Nothing to do
for those beyond replacing the binary.

## Upgrading to 4.4.77

**Take this one if you set object tags through the `x-amz-tagging` header.** No
configuration or on-disk format changes.

### Tags written before this release keep their encoded value

`x-amz-tagging` carries the tag set encoded as URL query parameters. Before this
release the server stored what it received without decoding it, so a value that
had to be encoded was saved in its encoded form. Sending
`x-amz-tagging: Tag1=Tag%201%20value` stored the value `Tag%201%20value`, and
`GetObjectTagging` handed that back.

The fix decodes the header on the way in, so new writes are correct. It does not
rewrite what is already stored, because the server cannot tell a value that was
wrongly encoded from one where the `%20` was always meant literally.

To find affected objects, list the tags you set through that header and look for
`%` followed by two hexadecimal digits, or a `+` where you expect a space. Re-apply
the tags on the ones you find, either by sending `PutObjectTagging` with the value
you want, or by repeating the original `PutObject` now that the header is decoded.

Objects tagged through `PutObjectTagging` with an XML body were never affected and
need nothing.

### A copy no longer loses the tags it was not asked to change

`CopyObject` treated the tag set as part of the metadata, so two ordinary copies
behaved wrongly.

- Replacing the metadata without mentioning tags **dropped the source's tags**.
  That is what `aws s3 cp --metadata-directive REPLACE` sends, so any copy of that
  shape silently lost every tag on the object.
- Asking to replace only the tags, with `x-amz-tagging-directive: REPLACE` and no
  metadata directive, was ignored and kept the source's tags instead.

Both now follow S3: the tagging directive governs the tags, the metadata directive
governs everything else, and they are independent. Nothing to configure.

If you have been copying objects with `--metadata-directive REPLACE`, the copies
made before this release have no tags. The originals are untouched, so check the
source object for the tag set the copy should have had.

### Multipart uploads now keep the headers they were given

An object assembled from parts used to keep only its content type. Its tags, its
`x-amz-meta-*` user metadata, `Content-Encoding`, `Content-Disposition`,
`Cache-Control`, `Content-Language` and `x-amz-website-redirect-location` were all
dropped at completion. aws-cli and the SDKs switch to multipart by themselves above
a few megabytes, so any large upload was affected whether or not you asked for
multipart.

Objects already stored are not rewritten. If you rely on metadata or tags that a
large upload was supposed to carry, re-send them with `PutObjectTagging` or
re-upload the object.

An upload that is in progress when you upgrade completes normally, and keeps the
old behaviour for that one object, because its record was written before the
change. On a cluster, finish the rolling upgrade before relying on the new
behaviour: an upload completed by a node still running the older build drops those
fields as it did before.

### Three stricter answers on the same header

These refuse requests an older server accepted. In each case what it accepted was
a tag it could not store correctly.

- A malformed percent-encoding, for example `k=%ZZ`, is refused with
  `InvalidArgument` rather than stored half-decoded.
- More than 10 tags is refused with `BadRequest`, the limit `PutObjectTagging`
  already enforced on the XML body.
- A literal `;` is refused with `InvalidTag`. It is not a character S3 permits in
  a tag, and it cannot be told apart from a pair separator. Percent-encode it as
  `%3B` inside a value, and separate tags with `&`.

A repeated key keeps the last occurrence, which is what `PutObjectTagging` has
always done with a repeated key in the XML body.

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
from the dashboard. Setting `VAULTS3_ACCESS_KEY` and `VAULTS3_SECRET_KEY` alone
does not replace saved credentials: add `VAULTS3_ADMIN_CREDENTIALS_OVERRIDE=true`
(available from 5.0.0) for one start, then remove it.

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
