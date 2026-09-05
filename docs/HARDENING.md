# Security hardening

The measures built into VaultS3. For reporting a vulnerability, see SECURITY.md.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

VaultS3 is designed with security in mind:

- **S3 Signature V4**: full signature verification including presigned URLs
- **Presigned URL validation**: signature, expiry, and restrictions enforced server-side
- **Constant-time credential comparison**: `crypto/hmac.Equal` prevents timing attacks on login
- **Path traversal protection**: `..` segments rejected at S3, API, versioning API, CopyObject/UploadPartCopy source, and filesystem layers
- **SSRF prevention**: webhook, lambda, and notification URLs blocked from targeting localhost, private IPs, and cloud metadata endpoints
- **Upload size limits**: 5GB per PUT (S3 spec), enforced with `http.MaxBytesReader`
- **Rate limiting**: per-IP and per-access-key token bucket, **on by default** (2000 req/s, 4000 burst), using `RemoteAddr` (not spoofable via `X-Forwarded-For`). The ceiling sits far above real client traffic so it bounds a flood without throttling legitimate use. Because the per-IP bucket keys on the connection address, every client behind a reverse proxy or ingress shares one bucket: raise `requests_per_sec` in that setup rather than lowering it
- **AES-256-GCM encryption at rest**: SSE-S3 (static key) and SSE-KMS (HashiCorp Vault / local key) modes
- **IAM with default-deny**: policy evaluation engine with wildcard matching
- **Security headers**: CSP, HSTS, X-Frame-Options, X-Content-Type-Options, Referrer-Policy
- **Non-root Docker**: container runs as `vaults3` user (UID 1000)
- **Default credential warning**: startup log warns if admin credentials haven't been changed
- **Error message sanitization**: OIDC and health check errors return generic messages, preventing internal detail leaking
- **Race condition safety**: Replication handler creates per-request struct copy instead of mutating shared state
- **UploadID validation**: Hex-only regex validation prevents path traversal via crafted multipart upload IDs
- **Bounded request bodies**: All JSON API endpoints use `readJSON()` with 1MB `io.LimitReader`. Bucket policy body capped at 1MB
- **OIDC SSRF prevention**: Issuer URL validated against loopback, private, and link-local addresses before JWKS discovery
- **IPv6-safe rate limiting**: Uses `net.SplitHostPort` for correct IP extraction from IPv6 `[::1]:port` addresses
- **OIDC authorization layer**: Dashboard admin routes (IAM, keys, STS, audit, settings, lambda, backups) restricted to admin user. OIDC users get read-only access
- **Chunked encryption**: encrypted objects are sealed a chunk at a time (AES-256-GCM per chunk, nonce bound to chunk index and end-of-stream), so a read is authenticated before any byte is served and costs a chunk of memory rather than a copy of the object. The 1GB cap now applies only to objects still stored in the pre-4.4.53 whole-object format
- **Compression size cap**: 1GB max decompressed size prevents decompression-bomb DoS (gzip/zstd)
- **Version path traversal protection**: `versionId` parameter validated against directory escape in version storage
- **BatchDelete lock enforcement**: Batch delete respects WORM/legal-hold and validates keys against path traversal
- **SigV4 timestamp validation**: Requests with `X-Amz-Date` skewed more than 15 minutes are rejected (prevents replay)
- **Presigned URL expiry cap**: Maximum 7 days (604800 seconds), matching AWS behavior
- **Atomic file writes**: PutObject writes to temp file then renames, preventing corruption from concurrent writes
- **Backup scheduler thread safety**: Atomic bool prevents concurrent backup races
- **OIDC admin name reservation**: OIDC users cannot claim the "admin" username
- **OIDC domain validation enforcement**: Tokens without email are rejected when domain filtering is enabled
- **OIDC code flow hardening**: The authorization code is redeemed server-side, so the ID token never travels through the browser. The PKCE verifier, nonce and client secret stay on the server. The CSRF state is sealed with AES-GCM and expires after 15 minutes, so it cannot be read, forged, or replayed from another deployment. The ID token's nonce is checked against the login that requested it
- **CORS port restriction**: Localhost CORS only allowed on the server's own port
- **Presigned URL credential isolation**: Presigned URLs use a dedicated non-admin key, preventing privilege escalation
- **CORS Host header protection**: Origin validation uses configured server address, not attacker-controlled Host header
- **Admin-only route expansion**: Backup, replication, scanner, and tiering endpoints restricted to admin users
- **Replication queue limit cap**: Queue listing capped at 1000 entries to prevent memory exhaustion
- **Backup path traversal protection**: Backup target validates resolved paths stay within base directory
- **LIKE pattern O(n*m) matching**, Iterative DP algorithm replaces recursive backtracking, preventing ReDoS
- **Tiering promotion safety**: Async cold-to-hot promotion re-checks tier state and orders operations safely
- **Lambda output key validation**: Output key template expansion validated against path traversal
- **S3 Select record cap**: JSON/CSV parsing capped at 1M records to prevent memory exhaustion
- **FUSE cache size caps**: Signature cache, HEAD cache, and LIST cache bounded to prevent unbounded memory growth
- **GetObjectAttributes version support**: Respects `versionId` parameter and handles delete markers
- **External authorization webhook**: Delegate the access decision to an HTTP endpoint. Deny-only by default, fail-closed by default, admin exempt so a broken endpoint cannot lock the operator out
- **KMS envelope encryption**: HashiCorp Vault and local key provider for data encryption key management
- **Auto-TLS**: Automatic Let's Encrypt certificate provisioning with self-signed fallback
- **PROXY protocol v1**: Real client IP extraction behind PROXY protocol-aware load balancers
- **Governance bypass protection**: `x-amz-bypass-governance-retention` restricted to authorized principals
- **IAM policy conditions**: `StringEquals`, `StringLike`, `IpAddress`, `DateLessThan` condition evaluation
- **Bucket bandwidth throttling**: Per-bucket upload/download rate limits prevent resource monopolization
- **POST policy validation**: HTML form upload policies validated for expiration, conditions, and signature
- **Content-MD5 validation**: Server-side integrity verification on PUT rejects corrupted uploads
- **S3 Checksum API**: CRC32, CRC32C, SHA1, SHA256 checksums verified on upload and returned on download
- **Conditional request handling**: `If-Match`/`If-None-Match` ETag checks prevent lost updates (412 Precondition Failed)
- **Dependency hygiene**: Dashboard dependencies kept current against Dependabot advisories (latest: `react-router` 7.18.1 and `postcss` 8.5.24, closing a backslash open redirect in `<Link>`/`useNavigate`, an unauthenticated route-matching DoS, an SSR hydration constructor injection, an RSCErrorHandler XSS, and a postcss path traversal. Earlier: `react-router` 7.17.0 closing 6 alerts, turbo-stream RCE, RSC/Location XSS, `__manifest`/single-fetch DoS, protocol-relative open redirect). The one advisory left open is a **React Server Components CSRF bypass** that is only patched in `react-router` 8.x: the dashboard is a client-rendered SPA and never uses RSC mode, so it is not affected, and 8.x would additionally require Node 22.22+ and dropping `react-router-dom`

See [SECURITY.md](../SECURITY.md) for vulnerability reporting policy and deployment best practices.
