import { apiFetch } from './client'
import { encodeSegment } from './paths'

export interface Bucket {
  name: string
  createdAt: string
  size: number
  objectCount: number
  maxSizeBytes?: number
  maxObjects?: number
  policy?: Record<string, unknown>
}

export function listBuckets(): Promise<Bucket[]> {
  return apiFetch<Bucket[]>('/buckets')
}

export function createBucket(name: string): Promise<Bucket> {
  return apiFetch<Bucket>('/buckets', {
    method: 'POST',
    body: JSON.stringify({ name }),
  })
}

export async function getBucket(name: string): Promise<Bucket> {
  return apiFetch<Bucket>(`/buckets/${encodeSegment(name)}`)
}

export async function deleteBucket(name: string): Promise<void> {
  return apiFetch<void>(`/buckets/${encodeSegment(name)}`, { method: 'DELETE' })
}

export async function setBucketPolicy(name: string, policy: string): Promise<void> {
  return apiFetch<void>(`/buckets/${encodeSegment(name)}/policy`, {
    method: 'PUT',
    body: policy,
  })
}

export async function setBucketQuota(name: string, maxSizeBytes: number, maxObjects: number): Promise<void> {
  return apiFetch<void>(`/buckets/${encodeSegment(name)}/quota`, {
    method: 'PUT',
    body: JSON.stringify({ maxSizeBytes, maxObjects }),
  })
}

// Versioning
export async function getBucketVersioning(name: string): Promise<{ versioning: string }> {
  return apiFetch<{ versioning: string }>(`/buckets/${encodeSegment(name)}/versioning`)
}

export async function setBucketVersioning(name: string, versioning: string): Promise<void> {
  return apiFetch<void>(`/buckets/${encodeSegment(name)}/versioning`, {
    method: 'PUT',
    body: JSON.stringify({ versioning }),
  })
}

export interface BucketEncryption {
  available: boolean // per-bucket encryption configured on the server
  enabled: boolean
  keyVersion?: number
  algorithm?: string
}

export async function getBucketEncryption(name: string): Promise<BucketEncryption> {
  return apiFetch<BucketEncryption>(`/buckets/${encodeSegment(name)}/encryption`)
}

export async function bucketEncryptionAction(name: string, action: 'enable' | 'rotate' | 'shred'): Promise<void> {
  return apiFetch<void>(`/buckets/${encodeSegment(name)}/encryption/${action}`, { method: 'POST' })
}

// Lifecycle
export interface LifecycleRule {
  expirationDays: number
  abortIncompleteMultipartDays?: number
  prefix: string
  status: string
}

export async function getLifecycleRule(name: string): Promise<{ rule: LifecycleRule | null }> {
  return apiFetch<{ rule: LifecycleRule | null }>(`/buckets/${encodeSegment(name)}/lifecycle`)
}

export async function setLifecycleRule(name: string, rule: LifecycleRule): Promise<void> {
  return apiFetch<void>(`/buckets/${encodeSegment(name)}/lifecycle`, {
    method: 'PUT',
    body: JSON.stringify(rule),
  })
}

export async function deleteLifecycleRule(name: string): Promise<void> {
  return apiFetch<void>(`/buckets/${encodeSegment(name)}/lifecycle`, { method: 'DELETE' })
}

// CORS
export interface CORSRule {
  allowed_origins: string[]
  allowed_methods: string[]
  allowed_headers?: string[]
  max_age_secs?: number
}

export async function getCORSConfig(name: string): Promise<{ rules: CORSRule[] }> {
  return apiFetch<{ rules: CORSRule[] }>(`/buckets/${encodeSegment(name)}/cors`)
}

export async function setCORSConfig(name: string, rules: CORSRule[]): Promise<void> {
  return apiFetch<void>(`/buckets/${encodeSegment(name)}/cors`, {
    method: 'PUT',
    body: JSON.stringify({ rules }),
  })
}

export async function deleteCORSConfig(name: string): Promise<void> {
  return apiFetch<void>(`/buckets/${encodeSegment(name)}/cors`, { method: 'DELETE' })
}
