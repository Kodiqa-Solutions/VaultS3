import { apiFetch } from './client'

export interface SearchResult {
  bucket: string
  key: string
  size: number
  content_type: string
  last_modified: number
  etag: string
  tags: Record<string, string>
}

export interface SearchQuery {
  q: string
  bucket?: string
  limit?: number
}

export function searchObjects(query: SearchQuery): Promise<SearchResult[]> {
  const params = new URLSearchParams()
  params.set('q', query.q)
  if (query.bucket) params.set('bucket', query.bucket)
  if (query.limit) params.set('limit', String(query.limit))
  return apiFetch<SearchResult[]>(`/search?${params.toString()}`)
}
