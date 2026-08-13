# Design: Per-Bucket Encryption Keys

**Status:** Proposal (prototype implemented in `internal/bucketcrypto`)
**Issue:** per-bucket encryption keys for the bucket-per-tenant multi-tenancy pattern

## Summary

Give each bucket its own encryption key so that, in a bucket-per-tenant
deployment, one tenant can encrypt their bucket with a key that is **not shared**
with any other tenant, and another tenant can opt out entirely. The key
hierarchy uses **envelope encryption**: a master key-encryption-key (KEK) wraps a
per-bucket data-encryption-key (DEK). Objects are encrypted with their bucket's
DEK.

## Motivation

A common, sound multi-tenancy pattern is **one bucket per tenant** with an IAM
access key scoped per bucket (VaultS3 already supports the IAM half). The natural
next step is a **per-bucket encryption key**:

- Tenant A encrypts their bucket. Tenant B opts out, independently.
- A compromise of one tenant's key does not expose any other tenant's data.
- Offboarding a tenant can be a single **key deletion** (crypto-shredding) rather
  than a bulk object wipe.

## Current state (what exists, what's missing)

| Capability | Today |
| --- | --- |
| Server-wide encryption-at-rest | ✅ `EncryptedEngine` wraps the whole engine with **one** AES-256-GCM key |
| KMS (Vault) integration | ✅ `KMSEncryptedEngine` with a single named key |
| Per-bucket SSE **config** API | ✅ `PutBucketEncryption` / `BucketEncryptionConfig{SSEAlgorithm, KMSKeyID}`, **declarative only** |
| Per-bucket IAM keys | ✅ |
| **Per-bucket distinct keys** | ❌ **the gap**, all buckets share the one engine key |
| Per-bucket opt-in / opt-out | ❌ encryption is all-or-nothing for the whole server |

So the missing piece is precisely **per-bucket keys** and **per-bucket opt-in**.
The `BucketEncryptionConfig` (including its unused `KMSKeyID`) is the natural place
to hang the new key material.

## Goals

- Distinct, non-shared encryption key per bucket.
- Per-bucket opt-in / opt-out (encrypted and plaintext buckets coexist).
- Key **rotation** without rewriting historical objects eagerly.
- **Crypto-shredding**: deleting a bucket's key renders its data unrecoverable.
- Backward compatibility: existing globally-encrypted and plaintext objects stay
  readable.
- Pluggable KEK source: a config master key today, Vault KMS or a cloud KMS later.

## Non-goals (initially)

- Operator-blind encryption where even the operator cannot read tenant data, that
  is **SSE-C / BYOK** (tenant-held keys), described as a follow-on below.
- Per-object distinct keys (per-bucket is the isolation boundary the issue asks
  for).
- Re-encrypting all historical data on rotation (lazy re-encrypt on write instead).

## Design

### Key hierarchy (envelope encryption)

```
master KEK  (from config or KMS; never written to disk in the clear)
   │  wraps
   ▼
per-bucket DEK (random 256-bit, one per bucket, versioned)
   │  encrypts
   ▼
object payload (AES-256-GCM, per-object random nonce)
```

- **KEK**: a 256-bit key from `encryption.master_key` (or unwrapped via Vault
  KMS). Used only to wrap/unwrap DEKs. Never touches object data.
- **DEK**: generated with a CSPRNG when a bucket opts in. Stored **only in
  wrapped form** (KEK-encrypted) in the bucket's encryption config. Unwrapped DEKs
  live in an in-memory cache, never on disk.
- **Object**: encrypted with its bucket's current DEK, AES-256-GCM, a fresh
  random nonce per object.

### Object on-disk format

New encrypted objects are self-describing so decryption can pick the right key
version and so legacy/plaintext objects are distinguishable.

**From 4.4.53 the written format is chunked** (`VS3S`), because sealing an object
as one AEAD message forced every read to hold the whole object in memory and
OOM-killed nodes serving large objects to concurrent readers (issue #49):

```
┌──────────┬────────┬─────────────┬────────────┬──────────────┬─────────────────────┐
│ magic    │ format │ key version │ chunk size │ nonce prefix │ chunks              │
│ "VS3S"(4)│ 1 byte │ uint32 (4)  │ uint32 (4) │ 7 bytes      │ [ct + 16-byte tag]* │
└──────────┴────────┴─────────────┴────────────┴──────────────┴─────────────────────┘
```

Each chunk's nonce is `prefix || chunkIndex(4) || finalFlag(1)`, so chunks cannot
be reordered or transplanted between objects and truncation is detected. A
plaintext ending exactly on a chunk boundary is followed by an empty final chunk,
so every object carries an authenticated end-of-stream marker. The plaintext
length is derived from the stored length alone.

The previous format, still written before 4.4.53 and still read, sealed the whole
object as one message:

```
┌──────────┬──────────┬──────────────┬───────────┬──────────────────┐
│ magic    │ format   │ key version  │ nonce     │ ciphertext + tag │
│ "VS3X"(4)│ 1 byte   │ uint32 (4)   │ 12 bytes  │ ...              │
└──────────┴──────────┴──────────────┴───────────┴──────────────────┘
```

- On read, `VS3S` or `VS3X` both decrypt with the bucket's DEK for the embedded
  **key version**. Key version 0 in a `VS3S` blob means it was sealed with a
  server-wide key (per-bucket versions start at 1), so it routes to the legacy key.
- If it starts with neither magic, it is either a legacy global-key object
  (decrypt with the legacy engine key if configured) or plaintext (opt-out bucket)
, handled by the integration layer.

### Per-bucket opt-in / opt-out

- A bucket is **encrypted** iff it has a current wrapped DEK in its config.
- `Encrypt(bucket, plaintext)` returns the plaintext unchanged (and `encrypted =
  false`) when the bucket has no key, so opt-out buckets are pure passthrough.
- Enabling encryption on a bucket = generate DEK → wrap → store. Done via
  `PUT /{bucket}?encryption` (S3) or the dashboard bucket config.

### Key storage

Extend `BucketEncryptionConfig`:

```go
type BucketEncryptionConfig struct {
    SSEAlgorithm  string // "AES256" | "aws:kms"
    KMSKeyID      string
    // new:
    KeyVersion    int    // current DEK version
    WrappedDEKs   map[int][]byte // version -> KEK-wrapped DEK (kept for rotation/read-back)
}
```

Wrapped DEKs are replicated like any other bucket metadata (the
`cmdPutEncryptionConfig` Raft command already exists for clustering).

### Rotation

- Rotation generates DEK version `N+1`, wraps it, sets it current. **Old versions
  are retained** in `WrappedDEKs` so existing objects (which carry their version in
  the header) still decrypt.
- New writes use the current version. Historical objects are re-encrypted lazily
  (on next overwrite) or by an optional background re-encrypt job.

### Crypto-shredding

- `ShredBucket` deletes **all** wrapped DEKs for the bucket and evicts the cache.
- Without the DEK, the KEK cannot recover it, and the ciphertext is unrecoverable, 
  a fast, durable tenant-offboarding / right-to-erasure primitive.

### KEK sources (pluggable)

1. **Config master key** (`encryption.master_key`, 32 bytes hex/base64), simplest.
2. **Vault KMS** (already integrated), the KEK itself is wrapped/unwrapped by
   Vault, so the master key never sits in config.
3. Cloud KMS (AWS/GCP), future, same interface.

## Backward compatibility & migration

- **Existing globally-encrypted objects** (legacy `EncryptedEngine`, no magic
  header): keep the legacy key available. On read, blobs without the `VS3X` magic
  fall back to the legacy key. New writes use per-bucket DEKs.
- **Existing plaintext objects**: unchanged. Buckets that never opt in stay
  plaintext.
- **No forced migration**: opting a bucket into encryption only affects objects
  written after opt-in (or run an optional background re-encrypt).

## API surface

- **S3**: `PUT /{bucket}?encryption` with `ServerSideEncryptionConfiguration` to
  enable. `GET`/`DELETE` to inspect/disable. (Disable ≠ shred, shred is an
  explicit admin action.)
- **Dashboard**: bucket config panel, encryption toggle, "rotate key", and a
  guarded "shred key (irreversible)" action.
- **SSE-C / BYOK (follow-on)**: accept `x-amz-server-side-encryption-customer-*`
  headers so a tenant supplies their own key per request. The server holds only a
  verification hash, never the key, the operator-blind option.

## Security considerations

- Unwrapped DEKs only in memory. Wrapped at rest. KEK never written by the app.
- AES-256-GCM with a unique random nonce per object (no nonce reuse, a new nonce
  is drawn per `Encrypt`).
- Decrypting bucket A's object under bucket B's key fails (GCM auth), isolation is
  cryptographic, not just an access check.
- Wrong KEK ⇒ DEK unwrap fails ⇒ no data access.

## Performance

- One AES-GCM pass per object (same as today's server-wide encryption).
- DEK unwrap is amortized by an in-memory per-bucket cache (one unwrap per bucket
  per process lifetime, not per object).

## Testing

- Round-trip per bucket. **cross-bucket isolation** (B's key can't read A's data).
- Opt-out passthrough (no key ⇒ plaintext in/out).
- Rotation (old-version objects still decrypt after rotate).
- Crypto-shred (after shred, decrypt fails).
- Wrong-KEK rejection.

## Rollout plan

1. **Prototype (this PR):** `internal/bucketcrypto`, envelope core + versioned key
   store + per-object format, with the full test matrix above. Not yet wired into
   the live write path.
2. Extend `BucketEncryptionConfig` + store wrapped DEKs. Wire `PUT ?encryption` to
   generate/store a DEK.
3. Integrate a per-bucket-keyed engine layer (resolve bucket DEK at
   `PutObject`/`GetObject`) with legacy-key read fallback.
4. Dashboard toggle + rotate + shred actions.
5. SSE-C / BYOK option.

## Prototype

`internal/bucketcrypto` implements steps in (1): `Manager` with `EnableBucket`,
`Encrypt`, `Decrypt`, `Rotate`, `ShredBucket`, a `KEK` (wrap/unwrap), and a
versioned `KeyStore`. See `bucketcrypto_test.go` for the proven properties.
