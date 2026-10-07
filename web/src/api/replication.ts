import { apiFetch } from './client'

export interface ReplicationPeer {
  name: string
  url: string
  queueDepth: number
  lastSync: string
  totalSynced: number
  // Sent only while the peer has a failure to report (omitempty on the server).
  lastError?: string
}

export interface ReplicationStatus {
  enabled: boolean
  peers: ReplicationPeer[]
}

export interface ReplicationEvent {
  type: string
  bucket: string
  key: string
  peer: string
  retryCount: number
  nextRetry: string
}

export function getReplicationStatus(): Promise<ReplicationStatus> {
  return apiFetch<ReplicationStatus>('/replication/status')
}

export function getReplicationQueue(limit?: number): Promise<ReplicationEvent[]> {
  const qs = limit ? `?limit=${limit}` : ''
  return apiFetch<ReplicationEvent[]>(`/replication/queue${qs}`)
}
