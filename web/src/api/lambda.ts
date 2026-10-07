import { apiFetch } from './client'
import { encodeSegment } from './paths'

export interface LambdaTrigger {
  id: string
  functionURL: string
  events: string[]
  keyFilter: string
}

export interface LambdaStatus {
  enabled: boolean
  totalTriggers: number
  buckets: number
  queueDepth: number
}

export interface BucketTriggers {
  bucket: string
  triggers: LambdaTrigger[]
}

export function getLambdaStatus(): Promise<LambdaStatus> {
  return apiFetch<LambdaStatus>('/lambda/status')
}

export function listLambdaTriggers(): Promise<BucketTriggers[]> {
  return apiFetch<BucketTriggers[]>('/lambda/triggers')
}

export async function setBucketTriggers(bucket: string, triggers: LambdaTrigger[]): Promise<void> {
  return apiFetch<void>(`/lambda/triggers/${encodeSegment(bucket)}`, { method: 'PUT', body: JSON.stringify({ triggers }) })
}

export async function deleteBucketTriggers(bucket: string): Promise<void> {
  return apiFetch<void>(`/lambda/triggers/${encodeSegment(bucket)}`, { method: 'DELETE' })
}
