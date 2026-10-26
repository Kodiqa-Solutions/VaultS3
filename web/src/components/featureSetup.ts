import type { Settings } from '../api/settings'

export type FeatureKey = keyof Settings['features']

// The smallest vaults3.yaml change that turns each feature on, shown in
// Settings next to a feature that is off. Keys and values mirror
// configs/vaults3.yaml, which documents every option in full.
export const featureSetup: Record<FeatureKey, string> = {
  encryption: `encryption:
  enabled: true
  key: "<64 hex chars>"  # openssl rand -hex 32`,
  perBucketEncryption: `encryption:
  enabled: true
  key: "<64 hex chars>"  # master key that wraps each bucket's key
  per_bucket: true`,
  compression: `compression:
  enabled: true`,
  accessLog: `logging:
  enabled: true
  file_path: "./access.log"`,
  rateLimit: `rate_limit:
  enabled: true
  requests_per_sec: 2000
  burst_size: 4000`,
  replication: `replication:
  enabled: true
  peers:
    - name: "dc2"
      url: "http://peer-vaults3:9000"
      access_key: "peer-admin"
      secret_key: "peer-secret"`,
  scanner: `scanner:
  enabled: true
  webhook_url: "http://localhost:3310/scan"`,
  tiering: `tiering:
  enabled: true
  cold_data_dir: "./cold_data"
  migrate_after_days: 30`,
  backup: `backup:
  enabled: true
  targets:
    - name: "local-backup"
      type: "local"
      path: "/backups/vaults3"
  schedule_cron: "0 2 * * *"
  retention_days: 30`,
  oidc: `oidc:
  enabled: true
  issuer_url: "https://idp.example.com"
  client_id: "vaults3"`,
  externalAuth: `external_auth:
  enabled: true
  url: "http://authz.internal:8080/check"`,
  notifyBackends: `notifications:
  kafka:
    enabled: true
    brokers: ["localhost:9092"]
    topic: "vaults3-events"
  # also: nats, redis, amqp, postgres, elasticsearch`,
  lambda: `lambda:
  enabled: true`,
  vector: `vector:
  enabled: true
  embedding_url: "http://localhost:11434/v1/embeddings"
  model: "nomic-embed-text"`,
  erasure: `erasure:
  enabled: true
  data_shards: 4
  parity_shards: 2`,
  cluster: `cluster:
  enabled: true
  secret: "<shared by every node>"
  node_id: "node-1"
  bootstrap: true  # first node only
  peers: ["node-2@host2:9001", "node-3@host3:9001"]`,
  packing: `packing:
  enabled: true`,
  debug: `debug: true`,
}

// The Settings anchor for one feature. Pages for a feature that is off link
// here, and Settings opens that feature's setup when it is the URL hash.
export function featureAnchor(key: FeatureKey): string {
  return `feature-${key}`
}
