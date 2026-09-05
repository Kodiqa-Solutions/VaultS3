import { apiFetch } from './client'

export interface Settings {
  server: {
    address: string
    port: number
    domain?: string
    shutdownTimeoutSecs: number
    tlsEnabled: boolean
  }
  storage: {
    dataDir: string
    metadataDir: string
    usageScanIntervalSecs: number
  }
  features: {
    encryption: boolean
    compression: boolean
    accessLog: boolean
    rateLimit: boolean
    replication: boolean
    scanner: boolean
    tiering: boolean
    backup: boolean
    oidc: boolean
    lambda: boolean
    vector: boolean
    erasure: boolean
    cluster: boolean
    packing: boolean
    perBucketEncryption: boolean
    externalAuth: boolean
    debug: boolean
  }
  lifecycle: {
    scanIntervalSecs: number
    auditRetentionDays: number
  }
  rateLimit?: {
    requestsPerSec: number
    burstSize: number
    perKeyRps: number
    perKeyBurst: number
  }
  memory: {
    maxSearchEntries: number
    goMemLimitMb?: number
  }
}

export function getSettings(): Promise<Settings> {
  return apiFetch<Settings>('/settings')
}

export function changeCredentials(currentSecretKey: string, newAccessKey: string, newSecretKey: string): Promise<{ message: string }> {
  return apiFetch<{ message: string }>('/settings/credentials', {
    method: 'PUT',
    body: JSON.stringify({ currentSecretKey, newAccessKey, newSecretKey }),
  })
}
