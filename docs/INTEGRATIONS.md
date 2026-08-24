# Integrations

Event notifications, replication, virus scanning, website hosting, search, S3 Select, and FUSE.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

## S3 Event Notifications

Configure webhooks on buckets to receive notifications when objects are created or deleted:

```python
from botocore.auth import SigV4Auth
from botocore.credentials import Credentials
from botocore.awsrequest import AWSRequest
import requests

# PUT notification configuration (S3-compatible XML)
notif_xml = b"""<?xml version="1.0" encoding="UTF-8"?>
<NotificationConfiguration>
  <TopicConfiguration>
    <Id>my-webhook</Id>
    <Topic>https://example.com/webhook</Topic>
    <Event>s3:ObjectCreated:*</Event>
    <Event>s3:ObjectRemoved:*</Event>
    <Filter>
      <S3Key>
        <FilterRule>
          <Name>prefix</Name>
          <Value>images/</Value>
        </FilterRule>
      </S3Key>
    </Filter>
  </TopicConfiguration>
</NotificationConfiguration>"""

# Sign and send (using botocore for SigV4)
url = "http://localhost:9000/my-bucket?notification"
creds = Credentials("vaults3-admin", "vaults3-secret-change-me")
req = AWSRequest(method="PUT", url=url, data=notif_xml,
    headers={"Content-Type": "application/xml"})
SigV4Auth(creds, "s3", "us-east-1").add_auth(req)
requests.put(url, headers=dict(req.headers), data=notif_xml)
```

Supported events: `s3:ObjectCreated:Put`, `s3:ObjectCreated:Copy`, `s3:ObjectCreated:CompleteMultipartUpload`, `s3:ObjectRemoved:Delete`. Use wildcards like `s3:ObjectCreated:*`. Webhook payloads follow the AWS S3 event notification JSON format.

Configure webhook delivery in `configs/vaults3.yaml`:

```yaml
notifications:
  max_workers: 4       # concurrent webhook delivery goroutines
  queue_size: 256      # buffered event queue size
  timeout_secs: 10     # webhook HTTP timeout
  max_retries: 3       # retry attempts for failed webhooks
  kafka:
    enabled: true
    brokers: ["localhost:9092"]
    topic: "vaults3-events"
  nats:
    enabled: true
    url: "nats://localhost:4222"
    subject: "vaults3.events"
  redis:
    enabled: true
    addr: "localhost:6379"
    channel: "vaults3-events"   # pub/sub mode
    list_key: ""                # set for LPUSH queue mode
  amqp:
    enabled: true
    url: "amqp://guest:guest@localhost:5672/"
    exchange: "vaults3-events"
    routing_key: "s3.events"
  postgres:
    enabled: true
    dsn: "postgres://user:pass@localhost:5432/vaults3?sslmode=disable"
    table: "s3_events"
  elasticsearch:
    enabled: true
    urls: ["http://localhost:9200"]
    index: "vaults3-events"
```

Additional backends: **AMQP/RabbitMQ** (publish to exchanges), **PostgreSQL** (insert into table), **Elasticsearch** (index events). In addition to per-bucket webhooks, you can enable global notification backends. All S3 events are published to every enabled backend. Multiple backends can be active simultaneously. Disabled backends add zero overhead.

## Async Replication

Replicate objects to a peer VaultS3 instance automatically:

```yaml
replication:
  enabled: true
  peers:
    - name: "dc2"
      url: "http://peer-vaults3:9000"
      access_key: "peer-admin"
      secret_key: "peer-secret"
  scan_interval_secs: 30   # queue processing interval
  max_retries: 5           # retry before dead-letter
  batch_size: 100          # events per scan cycle
```

Objects created, copied, or deleted on the primary, whether through the S3 API **or the web dashboard**, are asynchronously pushed to all configured peers over the S3 protocol. Buckets are auto-created on peers. Failed deliveries retry with exponential backoff (5s, 15s, 45s, 135s, 405s). The `X-VaultS3-Replication` header prevents infinite loops. Monitor via dashboard API:

```bash
curl http://localhost:9000/api/v1/replication/status   # per-peer sync stats
curl http://localhost:9000/api/v1/replication/queue     # pending queue entries
```

For one-way push, replication only needs to be enabled on the **source**. The **target** does not need `replication.enabled`, it just needs the peer `access_key`/`secret_key` (from the source's config) to be valid credentials on it. Enable replication on both sides only for `mode: active-active`.


## Webhook Virus Scanning

Scan uploaded objects with an external virus scanner (ClamAV REST, VirusTotal, etc.):

```yaml
scanner:
  enabled: true
  webhook_url: "http://localhost:3310/scan"
  timeout_secs: 30
  quarantine_bucket: "vaults3-quarantine"
  fail_closed: false          # false=fail-open (keep file), true=quarantine on error
  max_scan_size_bytes: 104857600  # 100MB
  workers: 2
```

When enabled, every uploaded object is POSTed to the webhook URL as multipart/form-data. If the scanner returns 406/403 (infected), the object is moved to the quarantine bucket and deleted from the original. Monitor via dashboard API:

```bash
curl http://localhost:9000/api/v1/scanner/status       # queue depth + recent scans
curl http://localhost:9000/api/v1/scanner/quarantine    # quarantined objects
```


## Access Logging

Enable structured JSON access logs:

```yaml
logging:
  enabled: true
  file_path: "./access.log"
```

Each S3 operation is logged as a JSON line with timestamp, method, bucket, key, status code, bytes, and client IP.

## Static Website Hosting

Serve static websites directly from buckets:

```python
s3.put_bucket_website(Bucket='my-site',
    WebsiteConfiguration={
        'IndexDocument': {'Suffix': 'index.html'},
        'ErrorDocument': {'Key': 'error.html'}
    })
```

Website-enabled buckets serve `index.html` for directory paths and a custom error page for missing objects. No authentication required for GET/HEAD requests.


## Full-Text Search

Search objects by key, content type, and tags across all buckets:

```bash
# Search by key substring
curl "http://localhost:9000/api/v1/search?q=readme" -H "Authorization: Bearer <token>"

# Search by content type
curl "http://localhost:9000/api/v1/search?q=type:image" -H "Authorization: Bearer <token>"

# Search by tag
curl "http://localhost:9000/api/v1/search?q=tag:project=vaults3" -H "Authorization: Bearer <token>"

# Filter by bucket and limit results
curl "http://localhost:9000/api/v1/search?q=docs&bucket=my-bucket&limit=10" -H "Authorization: Bearer <token>"
```

The search index is built on startup from BoltDB metadata and updated incrementally on every object put, delete, copy, and tag change. Supports plain text (substring match), `type:` prefix for content-type filtering, and `tag:key=value` for tag matching.


## S3 Select (SQL on Objects)

Execute SQL queries on CSV and JSON objects without downloading the full file:

```python
from botocore.auth import SigV4Auth
from botocore.credentials import Credentials
from botocore.awsrequest import AWSRequest
import requests

# Query a CSV file
url = "http://localhost:9000/my-bucket/data.csv?select&select-type=2"
body = b"""<?xml version="1.0"?>
<SelectObjectContentRequest>
    <Expression>SELECT name, age FROM s3object WHERE city = 'New York' AND age > '25'</Expression>
    <ExpressionType>SQL</ExpressionType>
    <InputSerialization><CSV><FileHeaderInfo>USE</FileHeaderInfo></CSV></InputSerialization>
    <OutputSerialization><JSON/></OutputSerialization>
</SelectObjectContentRequest>"""

creds = Credentials("vaults3-admin", "vaults3-secret-change-me")
req = AWSRequest(method="POST", url=url, data=body, headers={"Content-Type": "application/xml"})
SigV4Auth(creds, "s3", "us-east-1").add_auth(req)
r = requests.post(url, headers=dict(req.headers), data=body)
# Returns JSON lines: {"name":"Alice","age":"30"}\n{"name":"Charlie","age":"35"}
```

Supported SQL features:
- `SELECT *` or `SELECT col1, col2` (column projection)
- `FROM s3object` (required table name)
- `WHERE col = 'value'`, `!=`, `<`, `>`, `<=`, `>=` (comparisons, numeric-aware)
- `AND` / `OR` (logical operators)
- `LIKE 'pattern%'` (SQL wildcards: `%` = any chars, `_` = single char)
- `IS NULL` / `IS NOT NULL`
- `LIMIT N`
- Column references: `name`, `s3object.name`, `s.name`, `_1` (positional for CSV without headers)

Input formats: CSV (with/without headers, custom delimiters), JSON Lines, JSON Document (array), Parquet (columnar format via parquet-go).
Compressed input: GZIP and BZIP2 compressed CSV/JSON files are transparently decompressed before query execution.
Output formats: JSON (one object per line) or CSV.


## FUSE Mount

Mount a VaultS3 bucket as a local filesystem directory:

```bash
# Mount a bucket (requires macFUSE on macOS or FUSE on Linux)
vaults3-cli mount my-bucket /mnt/vaults3

# Browse files
ls /mnt/vaults3
cat /mnt/vaults3/docs/readme.txt

# Write files (creates objects in VaultS3)
echo "hello" > /mnt/vaults3/new-file.txt

# Unmount
# Press Ctrl+C in the mount terminal, or:
fusermount -u /mnt/vaults3
```

FUSE mount uses range requests for lazy loading, only the requested bytes are fetched from the server. Write support buffers data and uploads on file close.
