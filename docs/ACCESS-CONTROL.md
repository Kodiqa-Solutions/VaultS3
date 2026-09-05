# Access control

IAM users and policies, CORS, STS, the audit trail, and IP restrictions.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

## IAM (Users, Groups & Policies)

Fine-grained access control with S3-compatible IAM policies. `Action` and `Resource` accept both the bare-string form (`"Action": "s3:GetObject"`) and the array form, as AWS does, so policies copied from AWS documentation work unchanged. Operations on a specific object version are authorized as their own actions, `s3:DeleteObjectVersion` and `s3:GetObjectVersion`, so allowing `s3:DeleteObject` while denying `s3:DeleteObjectVersion` lets people delete recoverably without ever permanently destroying a version. Multi-object deletes are authorized per entry against the same rules, so the batch route is not a way around them:

```python
import requests, json

API = "http://localhost:9000/api/v1"
headers = {"Authorization": "Bearer <jwt-token>", "Content-Type": "application/json"}

# Create an IAM user
requests.post(f"{API}/iam/users", headers=headers, json={"name": "alice"})

# Attach a built-in policy (ReadOnlyAccess, ReadWriteAccess, FullAccess)
requests.post(f"{API}/iam/users/alice/policies", headers=headers,
    json={"policyName": "ReadOnlyAccess"})

# Create an access key for the user
resp = requests.post(f"{API}/keys", headers=headers, json={"userId": "alice"})
key = resp.json()  # {"accessKey": "...", "secretKey": "..."}

# Create groups and attach policies
requests.post(f"{API}/iam/groups", headers=headers, json={"name": "developers"})
requests.post(f"{API}/iam/groups/developers/policies", headers=headers,
    json={"policyName": "ReadWriteAccess"})

# Add user to group
requests.post(f"{API}/iam/users/alice/groups", headers=headers,
    json={"groupName": "developers"})

# Create custom policies
custom_policy = json.dumps({
    "Version": "2012-10-17",
    "Statement": [{
        "Effect": "Allow",
        "Action": ["s3:GetObject"],
        "Resource": ["arn:aws:s3:::my-bucket/*"]
    }]
})
requests.post(f"{API}/iam/policies", headers=headers,
    json={"name": "MyBucketReadOnly", "document": custom_policy})
```

Policy evaluation follows AWS IAM semantics: default deny, explicit Allow required, explicit Deny always wins. Admin keys and legacy keys (without a user) retain full access.

## Anonymous (public) Access

A bucket policy granting an action to `Principal: "*"` lets unauthenticated
callers perform it. The `Resource` is matched as a full object ARN, so a policy
publishes exactly the keys it names and nothing more:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": "*",
    "Action": "s3:GetObject",
    "Resource": "arn:aws:s3:::my-bucket/public/*"
  }]
}
```

Anonymous `GET my-bucket/public/index.html` succeeds. Anonymous
`GET my-bucket/private/secrets.env` returns 403, because its key is outside the
published prefix.

Rules worth knowing:

- `s3:GetObject` needs an object ARN (`arn:aws:s3:::bucket/*`). A bare
  `arn:aws:s3:::bucket` does not cover the objects in the bucket.
- `s3:ListBucket` is a separate permission on the bucket ARN
  (`arn:aws:s3:::bucket`). Granting object reads never makes the listing public,
  and granting the listing never makes objects readable.
- A statement with no `Resource` grants nothing.
- An explicit `Deny` always wins over an `Allow`.
- **Public Access Block overrides the policy.** With `BlockPublicPolicy` or
  `RestrictPublicBuckets` set, the bucket is never anonymously accessible no
  matter what its policy says.

Static website hosting is separate from all of this: enabling it serves GET and
HEAD without authentication for the whole bucket by design. Do not enable it on a
bucket holding anything private.

## CORS per Bucket

Configure Cross-Origin Resource Sharing on a per-bucket basis:

```python
s3.put_bucket_cors(Bucket='my-bucket', CORSConfiguration={
    'CORSRules': [{
        'AllowedOrigins': ['https://example.com'],
        'AllowedMethods': ['GET', 'PUT'],
        'AllowedHeaders': ['*'],
        'MaxAgeSeconds': 3600,
    }]
})
```

The server responds to `OPTIONS` preflight requests with the configured CORS headers. Unknown origins are rejected with 403.

## STS Temporary Credentials

Issue short-lived access keys for temporary access:

```python
import requests, boto3

API = "http://localhost:9000/api/v1"
headers = {"Authorization": "Bearer <jwt-token>", "Content-Type": "application/json"}

# Create temporary credentials for an IAM user (max 12 hours)
resp = requests.post(f"{API}/sts/session-token", headers=headers,
    json={"durationSecs": 3600, "userId": "alice"})
creds = resp.json()  # {"accessKey", "secretKey", "sessionToken", "expiration"}

# Use temporary credentials with any S3 client
s3 = boto3.client("s3", endpoint_url="http://localhost:9000",
    aws_access_key_id=creds["accessKey"],
    aws_secret_access_key=creds["secretKey"])
```

Temporary keys inherit the IAM user's policies. Expired keys are automatically cleaned up by the lifecycle worker.

## External Authorization Webhook

Delegate the access decision to an HTTP endpoint you run, so entitlements that
live in another system can gate VaultS3 without being copied into IAM policies.
Off by default.

```yaml
external_auth:
  enabled: true
  url: "http://authz.internal:8080/authz"
  timeout_ms: 2000
  cache_ttl_secs: 10
  authoritative: false   # deny-only: the webhook can narrow IAM, never widen it
  fail_open: false       # an endpoint that cannot be reached DENIES
  token: ""              # sent as "Authorization: Bearer <token>"
```

`VAULTS3_EXTERNAL_AUTH_URL` sets the URL and switches the feature on.
`VAULTS3_EXTERNAL_AUTH_TOKEN` sets the bearer token, so the shared secret can
come from a Kubernetes Secret rather than a mounted config file.

### The contract

VaultS3 POSTs one JSON object per decision and expects a 200 with `allow`:

```json
{"accessKey": "AKIA...", "user": "bob", "action": "s3:GetObject",
 "resource": "arn:aws:s3:::demo/secret/b.txt", "sourceIP": "10.0.0.4"}
```

```json
{"allow": false, "reason": "secret/ is off limits"}
```

Anything else, a non-200, an unparseable body, a timeout, or a refused
connection, is treated as a failure and resolved by `fail_open`.

### The two modes

**Deny-only (default, `authoritative: false`).** IAM must allow AND the webhook
must allow. The webhook can only narrow access, so an endpoint that is spoofed,
compromised, or simply wrong costs availability rather than every object you
hold. A request IAM already refuses is never sent to the endpoint, so
unauthorized probes do not become traffic on your authorization service.

**Authoritative (`authoritative: true`).** An allow from the webhook grants
access no IAM policy allows, for operators whose entitlements genuinely live
elsewhere. An explicit `Deny` in an IAM policy still refuses, in both modes, and
is never put to the webhook at all.

### What it does not cover

The admin identity is never sent to the webhook, on either the S3 or the
dashboard path. That is deliberate: it is the break-glass route, and a webhook
that is down or misconfigured must not be able to lock you out of your own
server. Scope real users with IAM users and access keys rather than sharing the
admin credential.

The dashboard bucket list is filtered by IAM alone. Asking the webhook once per
bucket would turn one dashboard load into an unbounded fan-out of calls, so a
bucket the webhook would refuse can still appear in the list. Opening it is
still refused.

### Cost

This puts a network hop in front of every authorized request, and the cache is
what keeps it off the hot path. Measured on 8 concurrent readers of a 4 KB
object, driven from a container:

| Configuration | Throughput |
|---|---|
| External auth off | 1913 to 1960 req/s |
| On, `cache_ttl_secs: 10` (default) | 1959 to 1963 req/s |
| On, `cache_ttl_secs: 0` | 217 to 220 req/s |

At the default the feature is free within run-to-run noise. With caching off it
is roughly **9x slower**, because throughput becomes whatever your authorization
endpoint can serve rather than what VaultS3 can. The 220 figure is the limit of
the simple Python endpoint used for the measurement, so a faster endpoint will do
better, but the shape holds: with no cache your storage runs at the speed of your
authorizer. Do not set `0` without meaning it.

The same effect shows on the S3 conformance suite. All 192 gated tests pass in
every configuration, but with caching off they take 47.7s and make 778 webhook
calls, against 12.6s and 12 calls at the default, and a 12.4s baseline with the
feature off.

`fail_open: false` means the endpoint going down takes storage down with it.
That is the honest cost of external authorization. `fail_open: true` serves the
request instead, which means an outage silently widens access. Every fail-open
allow is logged at WARN, and `vaults3 diagnose` reports the mode.

### A minimal endpoint

```python
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        allow = not (req["action"] == "s3:GetObject" and "/secret/" in req["resource"])
        body = json.dumps({"allow": allow}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

HTTPServer(("0.0.0.0", 8080), H).serve_forever()
```

Unlike notification and lambda endpoints, this URL is not rejected for being a
private address. Those are set by bucket owners over the S3 API, so they are
attacker-controlled. This one is set by you, in your own config, and a private
address is exactly where an authorization service normally lives.

## Audit Trail

Query the persistent audit log of all S3 operations:

```python
# List recent audit entries
requests.get(f"{API}/audit?limit=50", headers=headers)

# Filter by user, time range, or bucket
requests.get(f"{API}/audit?user=alice&limit=10", headers=headers)
requests.get(f"{API}/audit?from=1700000000&to=1700100000", headers=headers)
requests.get(f"{API}/audit?bucket=my-bucket", headers=headers)
```

Each entry records: timestamp, principal, user ID, action, resource, effect (Allow/Deny), source IP, and status code. Old entries are automatically pruned based on `security.audit_retention_days`.

## IP Restrictions

Control access by IP address at global or per-user level:

```yaml
# Global restrictions in config
security:
  ip_allowlist: ["10.0.0.0/8", "192.168.0.0/16"]  # empty = allow all
  ip_blocklist: ["10.0.0.99/32"]  # deny always wins
```

```python
# Per-user IP restrictions via API
requests.put(f"{API}/iam/users/alice/ip-restrictions", headers=headers,
    json={"allowedCidrs": ["10.0.0.0/8", "::1/128"]})

# Clear restrictions (allow from anywhere)
requests.put(f"{API}/iam/users/alice/ip-restrictions", headers=headers,
    json={"allowedCidrs": []})
```

Evaluation order: global blocklist (deny wins) → global allowlist → per-user allowlist. Admin keys are exempt from IP restrictions. Supports both IPv4 and IPv6 CIDR notation.


## Presigned Upload Restrictions

Generate presigned PUT URLs with server-enforced restrictions:

```python
import requests

API = "http://localhost:9000/api/v1"
headers = {"Authorization": "Bearer <jwt-token>", "Content-Type": "application/json"}

# Generate restricted presigned PUT URL
resp = requests.post(f"{API}/presign", headers=headers, json={
    "bucket": "uploads",
    "key": "images/photo.jpg",
    "method": "PUT",
    "expires": 3600,
    "maxSize": 10485760,               # 10MB max
    "allowTypes": "image/jpeg,image/png",  # only images
    "requirePrefix": "images/"         # must upload to images/
})
url = resp.json()["url"]

# Upload within restrictions — succeeds
requests.put(url, data=image_data, headers={"Content-Type": "image/jpeg"})

# Upload too large / wrong type / wrong prefix — 403 Forbidden
```

Restriction parameters (`X-Vault-MaxSize`, `X-Vault-AllowTypes`, `X-Vault-RequirePrefix`) are embedded in the signed URL and validated server-side.
