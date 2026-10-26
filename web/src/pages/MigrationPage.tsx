import { useState, useEffect, useCallback, useRef } from 'react'
import { useI18n } from '../i18n'
import { testMigrateSource, startMigration, listMigrateJobs, cancelMigration, type MigrateJob } from '../api/migrate'

// Migration works against any S3-compatible source. These presets only pre-fill a
// sensible endpoint hint and the SigV4 region for common sources, so the fields do
// not have to be looked up by hand. Everything is still fully editable.
const PRESETS: Record<string, { label: string; placeholder: string; region: string; hintKey?: string }> = {
  custom: { label: 'Any S3-compatible', placeholder: 'http://host:9000', region: 'us-east-1' },
  minio: { label: 'MinIO', placeholder: 'http://minio:9000', region: 'us-east-1' },
  seaweedfs: { label: 'SeaweedFS', placeholder: 'http://seaweedfs:8333', region: 'us-east-1', hintKey: 'migrate.hintSeaweedfs' },
  garage: { label: 'Garage', placeholder: 'http://garage:3900', region: 'garage', hintKey: 'migrate.hintGarage' },
  ceph: { label: 'Ceph RADOS Gateway', placeholder: 'http://ceph-rgw:8080', region: 'us-east-1' },
  aws: { label: 'AWS S3', placeholder: 'https://s3.us-east-1.amazonaws.com', region: 'us-east-1', hintKey: 'migrate.hintAws' },
  r2: { label: 'Cloudflare R2', placeholder: 'https://<account-id>.r2.cloudflarestorage.com', region: 'auto', hintKey: 'migrate.hintR2' },
  wasabi: { label: 'Wasabi', placeholder: 'https://s3.wasabisys.com', region: 'us-east-1' },
  b2: { label: 'Backblaze B2', placeholder: 'https://s3.us-west-000.backblazeb2.com', region: 'us-west-000', hintKey: 'migrate.hintB2' },
}

export default function MigrationPage() {
  const { t } = useI18n()
  const [preset, setPreset] = useState('custom')
  const [endpoint, setEndpoint] = useState('')
  const [accessKey, setAccessKey] = useState('')
  const [secretKey, setSecretKey] = useState('')
  const [region, setRegion] = useState('us-east-1')

  const onPreset = (key: string) => {
    setPreset(key)
    setRegion(PRESETS[key].region)
  }

  const [buckets, setBuckets] = useState<string[]>([])
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [tested, setTested] = useState(false)

  const [testing, setTesting] = useState(false)
  const [starting, setStarting] = useState(false)
  const [error, setError] = useState('')
  const [jobs, setJobs] = useState<MigrateJob[]>([])

  const pollRef = useRef<number | null>(null)

  const refreshJobs = useCallback(async () => {
    try {
      const data = await listMigrateJobs()
      setJobs((data || []).sort((a, b) => b.started_at - a.started_at))
    } catch {
      /* ignore transient poll errors */
    }
  }, [])

  useEffect(() => {
    refreshJobs()
    pollRef.current = window.setInterval(refreshJobs, 2000)
    return () => {
      if (pollRef.current) window.clearInterval(pollRef.current)
    }
  }, [refreshJobs])

  const source = () => ({ endpoint: endpoint.trim(), accessKey: accessKey.trim(), secretKey: secretKey.trim(), region: region.trim() })

  const handleTest = async () => {
    if (!endpoint.trim()) return
    setTesting(true)
    setError('')
    setTested(false)
    try {
      const { buckets } = await testMigrateSource(source())
      setBuckets(buckets || [])
      setSelected(new Set(buckets || []))
      setTested(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : t('migrate.connectionFailed'))
    } finally {
      setTesting(false)
    }
  }

  const handleStart = async () => {
    setStarting(true)
    setError('')
    try {
      await startMigration({ ...source(), buckets: Array.from(selected) })
      await refreshJobs()
    } catch (err) {
      setError(err instanceof Error ? err.message : t('migrate.couldNotStartMigration'))
    } finally {
      setStarting(false)
    }
  }

  const [cancelling, setCancelling] = useState<Set<string>>(new Set())

  const handleCancel = async (id: string) => {
    setCancelling(prev => new Set(prev).add(id))
    try {
      await cancelMigration(id)
      await refreshJobs()
    } catch (err) {
      setError(err instanceof Error ? err.message : t('migrate.couldNotCancelMigration'))
    } finally {
      setCancelling(prev => {
        const next = new Set(prev)
        next.delete(id)
        return next
      })
    }
  }

  const toggle = (b: string) => {
    setSelected(prev => {
      const next = new Set(prev)
      next.has(b) ? next.delete(b) : next.add(b)
      return next
    })
  }

  const input = "w-full px-3 py-2.5 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-900 dark:text-white text-sm"
  const label = "block text-xs font-medium text-gray-500 dark:text-gray-400 mb-1"

  // Guard against double-clicks spawning duplicate migrations of the same source.
  const sourceBusy = jobs.some(j => j.status === 'running' && j.endpoint === endpoint.trim())

  return (
    <div>
      <div className="mb-6">
        <h2 className="text-xl font-semibold text-gray-900 dark:text-white">{t('migration.migrateFromS3')}</h2>
        <p className="text-sm text-gray-500 dark:text-gray-400 mt-0.5">
          Import buckets and objects from MinIO, SeaweedFS, Garage, Ceph, AWS S3,
          Cloudflare R2, Wasabi, Backblaze B2, or any S3-compatible source.
        </p>
      </div>

      {error && (
        <div className="mb-4 p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
          {error}
        </div>
      )}

      {/* Step 1: source */}
      <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-5 mb-5">
        <h3 className="text-sm font-semibold text-gray-900 dark:text-white mb-4">1. Source endpoint</h3>
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          <div className="md:col-span-2">
            <label className={label}>{t('migration.sourceType')}</label>
            <select className={input} value={preset} onChange={e => onPreset(e.target.value)}>
              {Object.entries(PRESETS).map(([key, p]) => (
                <option key={key} value={key}>{p.label}</option>
              ))}
            </select>
          </div>
          <div className="md:col-span-2">
            <label className={label}>{t('migration.endpointUrl')}</label>
            <input className={input} placeholder={PRESETS[preset].placeholder} value={endpoint} onChange={e => setEndpoint(e.target.value)} />
            {PRESETS[preset].hintKey && (
              <p className="mt-1 text-xs text-gray-500 dark:text-gray-400">{t(PRESETS[preset].hintKey!)}</p>
            )}
          </div>
          <div>
            <label className={label}>{t('migration.accessKey')}</label>
            <input className={input} value={accessKey} onChange={e => setAccessKey(e.target.value)} />
          </div>
          <div>
            <label className={label}>{t('migration.secretKey')}</label>
            <input className={input} type="password" value={secretKey} onChange={e => setSecretKey(e.target.value)} />
          </div>
          <div>
            <label className={label}>{t('migration.region')}</label>
            <input className={input} value={region} onChange={e => setRegion(e.target.value)} />
          </div>
        </div>
        <button onClick={handleTest} disabled={testing || !endpoint.trim()}
          className="mt-4 px-5 py-2.5 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium transition-colors">
          {testing ? t('migrate.connecting') : t('migrate.connectListBuckets')}
        </button>
      </div>

      {/* Step 2: select buckets */}
      {tested && (
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-5 mb-5">
          <h3 className="text-sm font-semibold text-gray-900 dark:text-white mb-4">2. Select buckets to import</h3>
          {buckets.length === 0 ? (
            <p className="text-sm text-gray-400">{t('migration.noBucketsFoundOnTheSource')}</p>
          ) : (
            <div className="grid grid-cols-2 md:grid-cols-3 gap-2 mb-4">
              {buckets.map(b => (
                <label key={b} className="flex items-center gap-2 px-3 py-2 rounded-lg border border-gray-200 dark:border-gray-700 cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-700/30">
                  <input type="checkbox" checked={selected.has(b)} onChange={() => toggle(b)} className="rounded" />
                  <span className="text-sm text-gray-700 dark:text-gray-300 truncate">{b}</span>
                </label>
              ))}
            </div>
          )}
          <button onClick={handleStart} disabled={starting || selected.size === 0 || sourceBusy}
            className="px-5 py-2.5 rounded-lg bg-emerald-600 hover:bg-emerald-700 disabled:bg-emerald-400 disabled:cursor-not-allowed text-white text-sm font-medium transition-colors">
            {starting ? t('migrate.starting') : sourceBusy ? t('migrate.migrationInProgress') : t('migrate.migrateN', { n: selected.size })}
          </button>
          {sourceBusy && (
            <p className="mt-2 text-xs text-gray-400">A migration from this source is already running — cancel or wait for it to finish.</p>
          )}
        </div>
      )}

      {/* Jobs */}
      {jobs.length > 0 && (
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
          <div className="px-5 py-3 border-b border-gray-200 dark:border-gray-700">
            <h3 className="text-sm font-semibold text-gray-900 dark:text-white">{t('migration.migrations')}</h3>
          </div>
          <div className="divide-y divide-gray-100 dark:divide-gray-700/50">
            {jobs.map(job => {
              const pct = job.total > 0 ? Math.round(((job.copied + job.failed) / job.total) * 100) : (job.status === 'completed' ? 100 : 0)
              const color = job.status === 'failed' ? 'bg-red-500' : job.status === 'completed' ? 'bg-emerald-500' : job.status === 'cancelled' ? 'bg-gray-400' : 'bg-indigo-500'
              return (
                <div key={job.id} className="px-5 py-4">
                  <div className="flex items-center justify-between mb-2 gap-2">
                    <div className="text-sm text-gray-700 dark:text-gray-300 font-mono truncate">{job.endpoint}</div>
                    <div className="flex items-center gap-2 flex-shrink-0">
                      {job.status === 'running' && (
                        <button
                          onClick={() => handleCancel(job.id)}
                          disabled={cancelling.has(job.id)}
                          className="text-xs px-2 py-0.5 rounded-md border border-red-300 dark:border-red-700 text-red-600 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-900/20 disabled:opacity-50 transition-colors"
                        >
                          {cancelling.has(job.id) ? t('migrate.cancelling') : t('common.cancel')}
                        </button>
                      )}
                      <span className={`text-xs px-2 py-0.5 rounded-full ${
                        job.status === 'completed' ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
                        : job.status === 'failed' ? 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
                        : job.status === 'cancelled' ? 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-300'
                        : 'bg-indigo-100 text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-400'}`}>
                        {job.status}
                      </span>
                    </div>
                  </div>
                  <div className="w-full h-2 rounded-full bg-gray-200 dark:bg-gray-700 overflow-hidden mb-1.5">
                    <div className={`h-full ${color} transition-all`} style={{ width: `${pct}%` }} />
                  </div>
                  <div className="flex items-center justify-between text-xs text-gray-500 dark:text-gray-400">
                    <span>{job.copied} copied{job.failed > 0 ? ` · ${job.failed} failed` : ''}{job.total > 0 ? ` / ${job.total}` : ''}{job.policies > 0 ? ` · ${job.policies} ${job.policies === 1 ? 'policy' : 'policies'}` : ''}</span>
                    <span>{job.buckets.length === 1 ? t('buckets.countOne') : t('buckets.countOther', { n: job.buckets.length })}</span>
                  </div>
                  {job.error && <div className="mt-1.5 text-xs text-red-500">{job.error}</div>}
                </div>
              )
            })}
          </div>
        </div>
      )}
    </div>
  )
}
