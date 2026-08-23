import { apiFetch } from './client'

export interface AccessKey {
  accessKey: string
  maskedSecret: string
  createdAt: string
  isAdmin: boolean
  // The IAM user the key was created for. Absent on the built-in admin key,
  // which is not tied to a created user.
  userId?: string
}

export interface CreatedKey {
  accessKey: string
  secretKey: string
  createdAt: string
}

export function listKeys(): Promise<AccessKey[]> {
  return apiFetch<AccessKey[]>('/keys')
}

export function createKey(userId: string, buckets?: string[]): Promise<CreatedKey> {
  return apiFetch<CreatedKey>('/keys', {
    method: 'POST',
    body: JSON.stringify({ userId, buckets: buckets?.length ? buckets : undefined }),
  })
}

export function deleteKey(accessKey: string): Promise<void> {
  return apiFetch<void>(`/keys/${accessKey}`, { method: 'DELETE' })
}
