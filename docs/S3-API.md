# Supported S3 operations

Every S3 API call VaultS3 implements.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

| Operation | Endpoint | Status |
|-----------|----------|--------|
| List Buckets | `GET /` | Done |
| Create Bucket | `PUT /{bucket}` | Done |
| Delete Bucket | `DELETE /{bucket}` | Done |
| Head Bucket | `HEAD /{bucket}` | Done |
| Put Object | `PUT /{bucket}/{key}` | Done |
| Get Object | `GET /{bucket}/{key}` | Done |
| Delete Object | `DELETE /{bucket}/{key}` | Done |
| Head Object | `HEAD /{bucket}/{key}` | Done |
| List Objects V2 | `GET /{bucket}?prefix=&max-keys=` | Done |
| Copy Object | `PUT /{bucket}/{key}` + `x-amz-copy-source` | Done |
| Batch Delete | `POST /{bucket}?delete` | Done |
| Multipart Upload | `POST/PUT/DELETE /{bucket}/{key}?uploads&uploadId` | Done |
| UploadPartCopy | `PUT /{bucket}/{key}?partNumber&uploadId` + `x-amz-copy-source` | Done |
| S3 Select | `POST /{bucket}/{key}?select&select-type=2` | Done |
| Object Tagging | `PUT/GET/DELETE /{bucket}/{key}?tagging` | Done |
| Bucket Policy | `PUT/GET/DELETE /{bucket}?policy` | Done |
| Bucket Quota | `PUT/GET /{bucket}?quota` | Done |
| Bucket Durability (erasure + replicas) | `PUT/GET /{bucket}?durability` | Done |
| Bucket Versioning | `PUT/GET /{bucket}?versioning` | Done |
| List Object Versions | `GET /{bucket}?versions` | Done |
| Object Locking (Legal Hold) | `PUT/GET /{bucket}/{key}?legal-hold` | Done |
| Object Locking (Retention) | `PUT/GET /{bucket}/{key}?retention` | Done |
| Bucket Default Retention | `PUT/GET /{bucket}?object-lock` | Done |
| Lifecycle Rules | `PUT/GET/DELETE /{bucket}?lifecycle` | Done |
| Website Hosting | `PUT/GET/DELETE /{bucket}?website` | Done |
| Bucket CORS | `PUT/GET/DELETE /{bucket}?cors` | Done |
| Presigned URLs |, | Done |
| Get Bucket Location | `GET /{bucket}?location` | Done |
| Bucket Tagging | `PUT/GET/DELETE /{bucket}?tagging` | Done |
| Bucket ACL | `GET/PUT /{bucket}?acl` | Done |
| Object ACL | `GET/PUT /{bucket}/{key}?acl` | Done |
| Get Object Attributes | `GET /{bucket}/{key}?attributes` | Done |
| Bucket Encryption | `PUT/GET/DELETE /{bucket}?encryption` | Done |
| Public Access Block | `PUT/GET/DELETE /{bucket}?publicAccessBlock` | Done |
| Bucket Logging | `PUT/GET /{bucket}?logging` | Done |
| List Multipart Uploads | `GET /{bucket}?uploads` | Done |
| List Parts | `GET /{bucket}/{key}?uploadId=X` | Done |
| Metrics | `GET /metrics` | Done |
| IAM (Users/Groups/Policies) | Dashboard API `/api/v1/iam/*` | Done |
| STS Temporary Credentials | `POST /api/v1/sts/session-token` | Done |
| Audit Trail | `GET /api/v1/audit` | Done |
| IP Restrictions | `PUT /api/v1/iam/users/{name}/ip-restrictions` | Done |
| Bucket Notifications | `PUT/GET/DELETE /{bucket}?notification` | Done |
| Notification Configs | `GET /api/v1/notifications` | Done |
| Replication Status | `GET /api/v1/replication/status` | Done |
| Replication Queue | `GET /api/v1/replication/queue` | Done |
| Presigned URL Generation | `POST /api/v1/presign` | Done |
| Full-Text Search | `GET /api/v1/search?q=...` | Done |
| Scanner Status | `GET /api/v1/scanner/status` | Done |
| Quarantine List | `GET /api/v1/scanner/quarantine` | Done |
| Tiering Status | `GET /api/v1/tiering/status` | Done |
| Tiering Migrate | `POST /api/v1/tiering/migrate` | Done |
| Backup List | `GET /api/v1/backups` | Done |
| Backup Trigger | `POST /api/v1/backups/trigger` | Done |
| Backup Status | `GET /api/v1/backups/status` | Done |
| Version Diff | `GET /api/v1/versions/diff` | Done |
| Version Tags | `GET/POST/DELETE /api/v1/versions/tags` | Done |
| Version Rollback | `POST /api/v1/versions/rollback` | Done |
| Rate Limit Status | `GET /api/v1/ratelimit/status` | Done |
| OIDC Config | `GET /api/v1/auth/oidc/config` | Done |
| OIDC Login (code flow) | `POST /api/v1/auth/oidc/start`, `POST /api/v1/auth/oidc/callback` | Done |
| OIDC Login (implicit) | `POST /api/v1/auth/oidc` | Done |
| Lambda Triggers | `PUT/GET/DELETE /{bucket}?lambda` | Done |
| Lambda Trigger List | `GET /api/v1/lambda/triggers` | Done |
| Lambda Trigger CRUD | `GET/PUT/DELETE /api/v1/lambda/triggers/{bucket}` | Done |
| Lambda Status | `GET /api/v1/lambda/status` | Done |
| Bucket Versioning (Dashboard) | `GET/PUT /api/v1/buckets/{name}/versioning` | Done |
| Bucket Lifecycle (Dashboard) | `GET/PUT/DELETE /api/v1/buckets/{name}/lifecycle` | Done |
| Bucket CORS (Dashboard) | `GET/PUT/DELETE /api/v1/buckets/{name}/cors` | Done |
| Bulk Delete (Dashboard) | `POST /api/v1/buckets/{name}/bulk-delete` | Done |
| Bulk Download Zip | `GET /api/v1/buckets/{name}/download-zip?keys=...` | Done |
| Version List (Dashboard) | `GET /api/v1/versions?bucket=X&key=Y` | Done |
| Settings | `GET /api/v1/settings` | Done |
| System / Capacity | `GET /api/v1/system` | Done |
| Cluster Capacity | `GET /api/v1/cluster/info` | Done |
| Cluster Status | `GET /cluster/status`, `GET /api/v1/cluster/status` | Done |
| Cluster Join | `POST /cluster/join`, `POST /api/v1/cluster/join` | Done |
| Cluster Leave | `POST /cluster/leave`, `POST /api/v1/cluster/leave` | Done |
| Cluster Drain / Undrain | `POST /api/v1/cluster/{drain,undrain}` | Done |
| Cluster Rebalance | `POST /api/v1/cluster/rebalance` | Done |
| Replication Sync | `POST /_replication/sync` | Done |
| List Objects V1 | `GET /{bucket}?marker=` | Done |
| Replication Config | `PUT/GET/DELETE /{bucket}?replication` | Done |
| Restore Object | `POST /{bucket}/{key}?restore` | Done |
| POST Upload (Form) | `POST /{bucket}` (multipart/form-data) | Done |
| Get Object (Part) | `GET /{bucket}/{key}?partNumber=N` | Done |
| Event Stream | `GET /api/v1/events` (SSE) | Done |
| Log Stream | `GET /api/v1/logs` (SSE) | Done |
| Request Trace | `GET /api/v1/trace` (SSE) | Done |
| Health Diagnostics | `GET /api/v1/diagnostics` | Done |
| Manual Heal | `POST /api/v1/heal` | Done |
| Speedtest | `POST /api/v1/speedtest` | Done |
| Batch Operations | `POST /api/v1/batch` | Done |
| Inventory Reports | `GET /api/v1/inventory` | Done |
