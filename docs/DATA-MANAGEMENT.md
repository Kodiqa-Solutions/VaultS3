# Data management

Versioning, object lock, lifecycle, compression, packing, tiering, backup, and snapshots.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

## Object Versioning

Enable versioning on a bucket to keep multiple versions of objects:

```python
import boto3

s3 = boto3.client('s3', endpoint_url='http://localhost:9000',
    aws_access_key_id='vaults3-admin',
    aws_secret_access_key='vaults3-secret-change-me')

# Enable versioning
s3.put_bucket_versioning(Bucket='my-bucket',
    VersioningConfiguration={'Status': 'Enabled'})

# Upload creates a new version each time
s3.put_object(Bucket='my-bucket', Key='file.txt', Body=b'v1')
s3.put_object(Bucket='my-bucket', Key='file.txt', Body=b'v2')

# Get specific version
s3.get_object(Bucket='my-bucket', Key='file.txt', VersionId='...')

# Delete creates a delete marker (versions preserved)
s3.delete_object(Bucket='my-bucket', Key='file.txt')

# Permanently delete a specific version
s3.delete_object(Bucket='my-bucket', Key='file.txt', VersionId='...')
```

## Object Locking (WORM)

Protect objects from deletion with legal holds or retention policies:

```python
# Legal hold — prevents deletion regardless
s3.put_object_legal_hold(Bucket='my-bucket', Key='file.txt', VersionId='...',
    LegalHold={'Status': 'ON'})

# Retention — prevents deletion until date
s3.put_object_retention(Bucket='my-bucket', Key='file.txt', VersionId='...',
    Retention={'Mode': 'COMPLIANCE', 'RetainUntilDate': '2030-01-01T00:00:00Z'})
```


## Bucket Default Retention

Set default object retention on a versioned bucket, all new objects automatically inherit the retention policy:

```python
from botocore.auth import SigV4Auth
from botocore.credentials import Credentials
from botocore.awsrequest import AWSRequest
import requests

# Set default retention (requires versioning enabled)
url = "http://localhost:9000/my-bucket?object-lock"
body = b"""<?xml version="1.0" encoding="UTF-8"?>
<ObjectLockConfiguration>
  <Rule>
    <DefaultRetention>
      <Mode>GOVERNANCE</Mode>
      <Days>30</Days>
    </DefaultRetention>
  </Rule>
</ObjectLockConfiguration>"""

creds = Credentials("vaults3-admin", "vaults3-secret-change-me")
req = AWSRequest(method="PUT", url=url, data=body, headers={"Content-Type": "application/xml"})
SigV4Auth(creds, "s3", "us-east-1").add_auth(req)
requests.put(url, headers=dict(req.headers), data=body)

# All new objects now get 30-day GOVERNANCE retention automatically
s3.put_object(Bucket='my-bucket', Key='file.txt', Body=b'protected')
# file.txt cannot be deleted for 30 days
```

Modes: `GOVERNANCE` (admin can bypass with special header) or `COMPLIANCE` (nobody can shorten/remove until expiry). Requires versioning to be enabled on the bucket.


## Lifecycle Rules

Auto-delete objects after a specified number of days:

```python
s3.put_bucket_lifecycle_configuration(Bucket='my-bucket',
    LifecycleConfiguration={
        'Rules': [{
            'ID': 'expire-logs',
            'Expiration': {'Days': 30},
            'Filter': {'Prefix': 'logs/'},
            'Status': 'Enabled',
        }]
    })
```

Abort incomplete multipart uploads (from killed or failed clients) after a number of days, reclaiming the uploaded parts. A rule may specify only this action, with no object expiration:

```python
s3.put_bucket_lifecycle_configuration(Bucket='my-bucket',
    LifecycleConfiguration={
        'Rules': [{
            'ID': 'abort-stale-uploads',
            'AbortIncompleteMultipartUpload': {'DaysAfterInitiation': 7},
            'Filter': {'Prefix': ''},
            'Status': 'Enabled',
        }]
    })
```

The background worker scans periodically (configurable interval, default 1 hour) and deletes expired objects and aborts stale multipart uploads (removing both their metadata and their part files on disk). Locked objects (legal hold or retention) are skipped.

## Compression

Enable zstd compression to reduce storage usage:

```yaml
compression:
  enabled: true
```

All objects are transparently compressed (zstd) on write and decompressed on read. Objects written by older gzip builds are still read correctly.

**Compression works with encryption at rest.** Compression runs on the plaintext, before encryption, so the two compose: a 216 KB highly repetitive payload with both enabled is stored in 123 bytes. This was not always true. Encryption used to wrap compression, so the compressor was handed ciphertext, which does not compress, and the same payload occupied 216,056 bytes, exactly 1.00x, while still costing the CPU to attempt it. Objects written under that layering are still read correctly and need no rewrite, but they keep the size they were stored at, so the saving on an existing deployment appears as data is rewritten rather than at upgrade time.

Both directions stream, so a large object costs a compression window rather than a copy of itself: peak memory scales with concurrency, not with concurrency multiplied by object size. An upload that does not declare its length falls back to buffering, because the decompressed size has to be recorded in the frame header for reads to stream.

## Small-file packing (experimental)

For workloads with huge numbers of tiny objects, packing stores small objects as
independent zstd frames inside large append-only **volume** files (with byte-offset
locations in BoltDB) instead of one file per object, avoiding per-file overhead.
Objects larger than `max_object_size` are stored as individual files as usual.

```yaml
packing:
  enabled: true
  max_object_size: 1048576       # objects this size (bytes) or smaller are packed
  volume_max_size: 1073741824    # roll to a new volume past this size
  compact_interval_hours: 24     # background dead-space reclamation; 0 = disabled
  compact_min_dead_ratio: 0.5    # compact a volume once half of it is dead space
```

Deleted/overwritten objects leave dead space in volumes. It is reclaimed by
background compaction (or on demand via `POST /api/v1/compact`). Packing is
**experimental** and does not yet compose with encryption or erasure coding (it is
skipped, with a warning, if either is enabled).


## Data Tiering

Automatically migrate infrequently accessed objects to a cold storage directory:

```yaml
tiering:
  enabled: true
  cold_data_dir: "./cold_data"
  migrate_after_days: 30
  scan_interval_secs: 3600
```

Objects not accessed for `migrate_after_days` are moved to the cold data directory. On read, cold objects are transparently served and promoted back to hot storage. Manual migration is available via API:

```bash
# Check tiering status (hot/cold counts and sizes)
curl http://localhost:9000/api/v1/tiering/status -H "Authorization: Bearer <token>"

# Manually migrate an object to cold tier
curl -X POST http://localhost:9000/api/v1/tiering/migrate \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"bucket":"my-bucket","key":"archive/old-file.zip","direction":"cold"}'
```

## Backup Scheduler

Schedule automatic backups to local directories:

```yaml
backup:
  enabled: true
  targets:
    - name: "local-backup"
      type: "local"
      path: "/backups/vaults3"
  schedule_cron: "0 2 * * *"   # daily at 2am
  retention_days: 30
  incremental: false            # true for incremental backups
```

Monitor and trigger backups via API:

```bash
# Check backup status
curl http://localhost:9000/api/v1/backups/status -H "Authorization: Bearer <token>"

# List backup history
curl http://localhost:9000/api/v1/backups -H "Authorization: Bearer <token>"

# Trigger immediate backup
curl -X POST http://localhost:9000/api/v1/backups/trigger -H "Authorization: Bearer <token>"
```

Incremental backups only copy objects modified since the last successful backup. Full backups mirror the complete object store.

## Git-like Versioning

Compare, tag, and rollback object versions:

```bash
# Diff two versions (text files show line-by-line diff, binary shows metadata only)
curl "http://localhost:9000/api/v1/versions/diff?bucket=my-bucket&key=file.txt&v1=VERSION_A&v2=VERSION_B" \
  -H "Authorization: Bearer <token>"

# Tag a version with a label
curl -X POST http://localhost:9000/api/v1/versions/tags \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"bucket":"my-bucket","key":"file.txt","versionId":"VERSION_ID","tag":"v1.0"}'

# List tags for an object
curl "http://localhost:9000/api/v1/versions/tags?bucket=my-bucket&key=file.txt" \
  -H "Authorization: Bearer <token>"

# Delete a tag
curl -X DELETE "http://localhost:9000/api/v1/versions/tags?bucket=my-bucket&key=file.txt&tag=v1.0" \
  -H "Authorization: Bearer <token>"

# Rollback to a specific version (copies old version content as latest)
curl -X POST http://localhost:9000/api/v1/versions/rollback \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"bucket":"my-bucket","key":"file.txt","versionId":"VERSION_ID"}'
```

Text diffs use LCS (Longest Common Subsequence) to produce unified diffs with add/remove/equal lines. Binary files show only size and metadata differences.
