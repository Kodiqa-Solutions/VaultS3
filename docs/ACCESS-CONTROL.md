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
