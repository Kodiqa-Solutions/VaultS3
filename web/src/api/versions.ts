import { apiFetch } from './client'

export interface Version {
  versionId: string
  size: number
  lastModified: number
  etag: string
  isLatest: boolean
  deleteMarker: boolean
  contentType: string
}

export interface DiffLine {
  type: string  // "add" | "remove" | "equal"
  content: string
}

export interface DiffResult {
  isText: boolean
  lines?: DiffLine[]
  metaA?: Record<string, unknown>
  metaB?: Record<string, unknown>
}

export interface VersionTag {
  name: string
  bucket: string
  key: string
  versionId: string
  createdAt?: number
}

export function listVersions(bucket: string, key: string): Promise<Version[]> {
  return apiFetch<Version[]>(`/versions?bucket=${encodeURIComponent(bucket)}&key=${encodeURIComponent(key)}`)
}

export function getVersionDiff(bucket: string, key: string, v1: string, v2: string): Promise<DiffResult> {
  return apiFetch<DiffResult>(
    `/versions/diff?bucket=${encodeURIComponent(bucket)}&key=${encodeURIComponent(key)}&v1=${encodeURIComponent(v1)}&v2=${encodeURIComponent(v2)}`
  )
}

export function getVersionTags(bucket: string, key: string): Promise<VersionTag[]> {
  return apiFetch<VersionTag[]>(
    `/versions/tags?bucket=${encodeURIComponent(bucket)}&key=${encodeURIComponent(key)}`
  )
}

export function createVersionTag(bucket: string, key: string, versionId: string, tag: string): Promise<void> {
  return apiFetch<void>('/versions/tags', {
    method: 'POST',
    body: JSON.stringify({ bucket, key, versionId, tag }),
  })
}

export function deleteVersionTag(bucket: string, key: string, tag: string): Promise<void> {
  return apiFetch<void>(
    `/versions/tags?bucket=${encodeURIComponent(bucket)}&key=${encodeURIComponent(key)}&tag=${encodeURIComponent(tag)}`,
    { method: 'DELETE' }
  )
}

// Rollback writes the chosen version again as a new latest version, and answers
// with that new version's id.
export function rollbackVersion(bucket: string, key: string, versionId: string): Promise<{ versionId?: string }> {
  return apiFetch<{ versionId?: string }>('/versions/rollback', {
    method: 'POST',
    body: JSON.stringify({ bucket, key, versionId }),
  })
}
