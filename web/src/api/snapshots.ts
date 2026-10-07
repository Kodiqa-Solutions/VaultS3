import { apiFetch } from './client'
import { encodeSegment } from './paths'

export interface Snapshot {
  id: string
  bucket: string
  message: string
  createdAt: number
  objects: number
  size: number
}

export interface SnapshotChange {
  key: string
  kind: 'added' | 'removed' | 'modified'
}

export interface SnapshotDiff {
  added: number
  removed: number
  modified: number
  changes: SnapshotChange[]
}

export interface RestoreResult {
  reverted: number
  removed: number
  skipped: number
  // A restore that could not finish answers 500 with these filled in.
  failed?: number
  errors?: string[]
}

export async function listSnapshots(bucket: string): Promise<Snapshot[]> {
  return apiFetch(`/buckets/${encodeSegment(bucket)}/snapshots`)
}

export async function createSnapshot(bucket: string, message: string): Promise<Snapshot> {
  return apiFetch(`/buckets/${encodeSegment(bucket)}/snapshots`, { method: 'POST', body: JSON.stringify({ message }) })
}

export async function diffSnapshot(bucket: string, id: string): Promise<SnapshotDiff> {
  return apiFetch(`/buckets/${encodeSegment(bucket)}/snapshots/${encodeSegment(id)}/diff`)
}

export async function restoreSnapshot(bucket: string, id: string): Promise<RestoreResult> {
  return apiFetch(`/buckets/${encodeSegment(bucket)}/snapshots/${encodeSegment(id)}/restore`, { method: 'POST' })
}

export async function deleteSnapshot(bucket: string, id: string): Promise<void> {
  return apiFetch(`/buckets/${encodeSegment(bucket)}/snapshots/${encodeSegment(id)}`, { method: 'DELETE' })
}
