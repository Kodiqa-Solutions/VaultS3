# Security hardening

The measures built into VaultS3. For reporting a vulnerability, see SECURITY.md.

[Documentation index](README.md) · [Back to the project README](../README.md)

---

VaultS3 is designed with security in mind:

- **S3 Signature V4**: full signature verification including presigned URLs
- **Signed headers**: every `x-amz-*` header of a signed request or presigned URL must be covered by its signature, as on AWS, so a header added on the network or by a URL's holder (a copy source, an encryption key) is refused. Only `X-Amz-Content-Sha256` (covered by a header-signed request's signature anyway, and never used to check a presigned upload), `X-Amz-Cf-Id` and `X-Amz-User-Agent`, which cannot change what a request does, may be unsigned
- **Upload body integrity**: the body of a signed upload is checked against the SHA-256 its signature covers, and every chunk and trailer signature of a signed streaming upload is verified, so a body changed on the network is refused even over plain HTTP when the client signed it. A body sent as `UNSIGNED-PAYLOAD` (presigned uploads, and the CLI's and FUSE's) has nothing to be checked against and relies on HTTPS. Flexible checksums (CRC32, CRC32C, CRC64NVME, SHA1, SHA256) are verified whether sent as a header or as a trailer
- **Slow client protection**: an HTTP/1.1 client has 30 seconds to send its request headers, the TLS handshake is held to 30 seconds, a keep-alive connection idle between requests is closed after `server.idle_timeout_secs` (650 by default), the dashboard sign-in body (read before authentication) has the same 30 seconds, and after an answer the server waits at most 10 seconds for the rest of a body it did not read, on every HTTP listener (S3, console, inter-node). Over HTTP/2 the per-request header limit and the 10 second wait do not apply (an unread HTTP/2 body does not hold the connection); the handshake and idle limits do. Request bodies and responses have no time limit, so large uploads, downloads and live streams are never cut off
- **Presigned URL validation**: signature, expiry, and restrictions enforced server-side
- **Constant-time credential comparison**: `crypto/hmac.Equal` prevents timing attacks on login
- **Path traversal protection**: `..` segments rejected at S3, API, versioning API, CopyObject/UploadPartCopy source, and filesystem layers
- **SSRF prevention**: webhook, lambda, and notification URLs blocked from targeting localhost, private IPs, and cloud metadata endpoints
- **Upload size limits**: 5GB per PUT (S3 spec), enforced with `http.MaxBytesReader`
- **Rate limiting**: per-IP and per-access-key token bucket, **on by default** (2000 req/s, 4000 burst), using `RemoteAddr` (not spoofable via `X-Forwarded-For`). The ceiling sits far above real client traffic so it bounds a flood without throttling legitimate use. Because the per-IP bucket keys on the connection address, every client behind a reverse proxy or ingress shares one bucket: raise `requests_per_sec` in that setup rather than lowering it
- **AES-256-GCM encryption at rest**: SSE-S3 (static key) and SSE-KMS (local key provider, the HashiCorp Vault provider does not work yet) modes. Encryption is a server setting: with it off, a request asking for SSE-S3 is stored unencrypted (not refused), and no response claims `x-amz-server-side-encryption` for a bucket that does not encrypt at rest
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
- **Tiering safety**: an object moved to the cold tier is still read from there (5.0.12). A move removes the hot file only if it is still the one copied, so a PUT that lands during the move keeps its data. A restore puts the data back only where no hot file exists, never over data a PUT wrote meanwhile, and drops the stale cold copy
- **Lambda output key validation**: Output key template expansion validated against path traversal
- **S3 Select limits**: at most 64 MiB of input after decompression, 4 million fields, 128 MiB of values, 1 MiB per record and two requests at a time, so a small compressed object cannot expand into gigabytes of memory
- **User content is sandboxed**: an S3 response a browser could run as a page (HTML, SVG, XML, script, unknown types) carries `Content-Security-Policy: sandbox`, so such an object served from the dashboard's own origin runs no script; a website bucket's pages run theirs in an origin of their own, away from the dashboard's session
- **Signature guessing is slowed**: past 20 failed signatures in a row for one access key from one address (an IPv6 /64), each further failure is answered after one second; a correct signature is never refused or delayed, so the throttle cannot be used to lock a key out (a success clears the count)
- **Version IDs are checked**: a version ID with a path separator or dots is refused, so it cannot name another key's version file
- **Object lock on upload takes the lock permission**: `x-amz-object-lock-*` headers need `s3:PutObjectRetention` / `s3:PutObjectLegalHold`, like the lock API
- **FUSE cache size caps**: Signature cache, HEAD cache, and LIST cache bounded to prevent unbounded memory growth
- **GetObjectAttributes version support**: Respects `versionId` parameter and handles delete markers
- **External authorization webhook**: Delegate the access decision to an HTTP endpoint. Deny-only by default, fail-closed by default, admin exempt so a broken endpoint cannot lock the operator out
- **KMS envelope encryption**: local key provider for data encryption key management (the HashiCorp Vault provider does not work yet)
- **PROXY protocol v1**: Real client IP extraction behind PROXY protocol-aware load balancers
- **Governance bypass protection**: `x-amz-bypass-governance-retention` restricted to authorized principals
- **IAM policy conditions**: `StringEquals`, `StringLike`, `IpAddress`, `DateLessThan` condition evaluation
- **POST policy validation**: a browser form upload signed with a POST policy (presigned POST) is checked from the fields before the file, and nothing of the file is read until it passes: the SigV4 signature over the policy, its expiry, every condition (an unknown one refuses the form, and the bucket is compared to the one in the URL), that no field is left out of the policy, the signer's IP allowlist and permission to write the key, then the size range and the bucket quota while the file streams. Everything before the file, part headers included, is bounded (320 KiB) and must arrive within the header time limit, since nobody is authenticated yet. A policy is valid until the expiration it states, which is not capped (presigned URLs are, at 7 days), so give policies a short expiration and rotate a key whose policies leaked
- **Content-MD5 validation**: Server-side integrity verification on PUT rejects corrupted uploads
- **S3 Checksum API**: CRC32, CRC32C, CRC64NVME, SHA1, SHA256 checksums verified on upload (as a header or an aws-chunked trailer) and returned on download
- **Conditional request handling**: `If-Match`/`If-None-Match` ETag checks prevent lost updates (412 Precondition Failed)
- **Dependency hygiene**: Dashboard dependencies kept current against Dependabot advisories (latest: `react-router` 7.18.1 and `postcss` 8.5.24, closing a backslash open redirect in `<Link>`/`useNavigate`, an unauthenticated route-matching DoS, an SSR hydration constructor injection, an RSCErrorHandler XSS, and a postcss path traversal. Earlier: `react-router` 7.17.0 closing 6 alerts, turbo-stream RCE, RSC/Location XSS, `__manifest`/single-fetch DoS, protocol-relative open redirect). The one advisory left open is a **React Server Components CSRF bypass** that is only patched in `react-router` 8.x: the dashboard is a client-rendered SPA and never uses RSC mode, so it is not affected, and 8.x would additionally require Node 22.22+ and dropping `react-router-dom`

See [SECURITY.md](../SECURITY.md) for vulnerability reporting policy and deployment best practices.
