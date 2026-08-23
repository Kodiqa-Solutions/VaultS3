import { apiFetch } from './client'

export interface AuditEntry {
  time: string
  user: string
  action: string
  resource: string
  effect: string
  sourceIp: string
  statusCode: number
}

export interface AuditQuery {
  limit?: number
  from?: string
  to?: string
  user?: string
  bucket?: string
}

export function queryAudit(q: AuditQuery = {}): Promise<AuditEntry[]> {
  const params = new URLSearchParams()
  if (q.limit) params.set('limit', String(q.limit))
  if (q.from) params.set('from', q.from)
  if (q.to) params.set('to', q.to)
  if (q.user) params.set('user', q.user)
  if (q.bucket) params.set('bucket', q.bucket)
  const qs = params.toString()
  return apiFetch<AuditEntry[]>(`/audit${qs ? `?${qs}` : ''}`)
}
