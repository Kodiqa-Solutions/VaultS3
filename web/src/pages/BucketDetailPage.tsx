import { useState, useEffect } from 'react'
import { useI18n } from '../i18n'
import { useParams, Link } from 'react-router-dom'
import { useToast } from '../hooks/useToast'
import SnapshotsPanel from '../components/SnapshotsPanel'
import EncryptionPanel from '../components/EncryptionPanel'
import {
  getBucket, setBucketPolicy, setBucketQuota,
  getBucketVersioning, setBucketVersioning,
  getLifecycleRule, setLifecycleRule, deleteLifecycleRule,
  getCORSConfig, setCORSConfig, deleteCORSConfig,
  type Bucket, type LifecycleRule, type CORSRule,
} from '../api/buckets'

export default function BucketDetailPage() {
  const { t } = useI18n()
  const { name } = useParams<{ name: string }>()
  const [bucket, setBucket] = useState<Bucket | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const { addToast } = useToast()

  // Quota state
  const [maxSizeBytes, setMaxSizeBytes] = useState('')
  const [maxObjects, setMaxObjects] = useState('')
  const [savingQuota, setSavingQuota] = useState(false)

  // Policy state
  const [policyText, setPolicyText] = useState('')
  const [savingPolicy, setSavingPolicy] = useState(false)

  // Versioning state
  const [versioning, setVersioning] = useState('')
  const [savingVersioning, setSavingVersioning] = useState(false)

  // Lifecycle state
  const [lifecycleRule, setLifecycleRuleState] = useState<LifecycleRule | null>(null)
  const [lcExpDays, setLcExpDays] = useState('')
  const [lcAbortDays, setLcAbortDays] = useState('')
  const [lcPrefix, setLcPrefix] = useState('')
  const [lcStatus, setLcStatus] = useState('Enabled')
  const [savingLifecycle, setSavingLifecycle] = useState(false)

  // CORS state
  const [corsRules, setCorsRules] = useState<CORSRule[]>([])
  const [corsText, setCorsText] = useState('')
  const [savingCors, setSavingCors] = useState(false)

  useEffect(() => {
    if (!name) return
    setLoading(true)

    // Load bucket first (required), then load optional configs independently
    getBucket(name)
      .then((b) => {
        setBucket(b)
        setMaxSizeBytes(b.maxSizeBytes ? String(b.maxSizeBytes) : '')
        setMaxObjects(b.maxObjects ? String(b.maxObjects) : '')
        setPolicyText(b.policy ? JSON.stringify(b.policy, null, 2) : '')

        // Load optional configs — failures are non-fatal
        getBucketVersioning(name)
          .then((v) => setVersioning(v.versioning || ''))
          .catch(() => {})

        getLifecycleRule(name)
          .then((lc) => {
            if (lc.rule) {
              setLifecycleRuleState(lc.rule)
              setLcExpDays(lc.rule.expirationDays ? String(lc.rule.expirationDays) : '')
              setLcAbortDays(lc.rule.abortIncompleteMultipartDays ? String(lc.rule.abortIncompleteMultipartDays) : '')
              setLcPrefix(lc.rule.prefix || '')
              setLcStatus(lc.rule.status || 'Enabled')
            }
          })
          .catch(() => {})

        getCORSConfig(name)
          .then((cors) => {
            setCorsRules(cors.rules || [])
            if (cors.rules && cors.rules.length > 0) {
              setCorsText(JSON.stringify(cors.rules, null, 2))
            }
          })
          .catch(() => {})
      })
      .catch((err) => setError(err instanceof Error ? err.message : t('bucket.loadFailed')))
      .finally(() => setLoading(false))
  }, [name])

  const flash = (msg: string) => { addToast('success', msg) }

  const handleSaveQuota = async () => {
    if (!name) return
    setSavingQuota(true); setError('')
    try {
      await setBucketQuota(name, Number(maxSizeBytes) || 0, Number(maxObjects) || 0)
      flash(t('bucket.quotaUpdated'))
      setBucket(await getBucket(name))
    } catch (err) {
      setError(err instanceof Error ? err.message : t('bucket.quotaFailed'))
    } finally { setSavingQuota(false) }
  }

  const handleSavePolicy = async () => {
    if (!name) return
    setSavingPolicy(true); setError('')
    try {
      await setBucketPolicy(name, policyText)
      flash(t('bucket.policyUpdated'))
      setBucket(await getBucket(name))
    } catch (err) {
      setError(err instanceof Error ? err.message : t('bucket.policyFailed'))
    } finally { setSavingPolicy(false) }
  }

  const handleToggleVersioning = async () => {
    if (!name) return
    setSavingVersioning(true); setError('')
    const next = versioning === 'Enabled' ? 'Suspended' : 'Enabled'
    try {
      await setBucketVersioning(name, next)
      setVersioning(next)
      flash(next === 'Enabled' ? t('bucket.versioningEnabled') : t('bucket.versioningSuspended'))
    } catch (err) {
      setError(err instanceof Error ? err.message : t('bucket.versioningFailed'))
    } finally { setSavingVersioning(false) }
  }

  const handleSaveLifecycle = async () => {
    if (!name) return
    setSavingLifecycle(true); setError('')
    try {
      const rule: LifecycleRule = {
        expirationDays: Number(lcExpDays) || 0,
        abortIncompleteMultipartDays: Number(lcAbortDays) || 0,
        prefix: lcPrefix,
        status: lcStatus,
      }
      await setLifecycleRule(name, rule)
      setLifecycleRuleState(rule)
      flash(t('bucket.lifecycleSaved'))
    } catch (err) {
      setError(err instanceof Error ? err.message : t('bucket.lifecycleSaveFailed'))
    } finally { setSavingLifecycle(false) }
  }

  const handleDeleteLifecycle = async () => {
    if (!name) return
    setSavingLifecycle(true); setError('')
    try {
      await deleteLifecycleRule(name)
      setLifecycleRuleState(null)
      setLcExpDays(''); setLcPrefix(''); setLcStatus('Enabled')
      flash(t('bucket.lifecycleRemoved'))
    } catch (err) {
      setError(err instanceof Error ? err.message : t('bucket.lifecycleDeleteFailed'))
    } finally { setSavingLifecycle(false) }
  }

  const handleSaveCors = async () => {
    if (!name) return
    setSavingCors(true); setError('')
    try {
      const rules = corsText.trim() ? JSON.parse(corsText) : []
      await setCORSConfig(name, rules)
      setCorsRules(rules)
      flash('CORS configuration saved')
    } catch (err) {
      setError(err instanceof Error ? err.message : t('bucket.corsSaveFailed'))
    } finally { setSavingCors(false) }
  }

  const handleDeleteCors = async () => {
    if (!name) return
    setSavingCors(true); setError('')
    try {
      await deleteCORSConfig(name)
      setCorsRules([]); setCorsText('')
      flash('CORS configuration removed')
    } catch (err) {
      setError(err instanceof Error ? err.message : t('bucket.corsDeleteFailed'))
    } finally { setSavingCors(false) }
  }

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-indigo-600" />
      </div>
    )
  }

  if (!bucket) {
    return <div className="text-red-600 dark:text-red-400">{error || t('bucket.notFound')}</div>
  }

  return (
    <div className="max-w-3xl">
      {/* Breadcrumb */}
      <div className="flex items-center gap-2 text-sm text-gray-500 dark:text-gray-400 mb-4">
        <Link to="/buckets" className="hover:text-indigo-600 dark:hover:text-indigo-400">{t('nav.buckets')}</Link>
        <span>/</span>
        <span className="text-gray-900 dark:text-white font-medium">{bucket.name}</span>
      </div>

      <div className="flex items-center justify-between mb-6">
        <h2 className="text-xl font-semibold text-gray-900 dark:text-white">{bucket.name}</h2>
        <Link
          to={`/buckets/${bucket.name}/files`}
          className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 text-white text-sm font-medium transition-colors"
        >
          {t('bucket.browseFiles')}
        </Link>
      </div>

      {error && (
        <div className="mb-4 p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
          {error}
        </div>
      )}

      {/* Info cards */}
      <div className="grid grid-cols-2 gap-4 mb-6">
        <InfoCard label={t('home.objects')} value={String(bucket.objectCount)} />
        <InfoCard label={t('common.size')} value={formatSize(bucket.size)} />
        <InfoCard label={t('common.created')} value={formatDate(bucket.createdAt)} />
        <InfoCard label={t('bucket.quota')} value={bucket.maxSizeBytes ? formatSize(bucket.maxSizeBytes) : t('bucket.unlimited')} />
      </div>

      {/* Versioning indicator */}
      {versioning === 'Enabled' && (
        <div className="mb-4 flex items-center gap-3 px-4 py-3 rounded-lg bg-indigo-50 dark:bg-indigo-900/20 border border-indigo-200 dark:border-indigo-800">
          <svg className="w-5 h-5 text-indigo-600 dark:text-indigo-400 flex-shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M12 6v6h4.5m4.5 0a9 9 0 11-18 0 9 9 0 0118 0z" />
          </svg>
          <div className="flex-1">
            <p className="text-sm font-medium text-indigo-700 dark:text-indigo-300">{t('bucket.versioningOn')}</p>
            <p className="text-xs text-indigo-600/70 dark:text-indigo-400/70">{t('bucket.versioningOnHint')}</p>
          </div>
          <Link
            to={`/buckets/${bucket.name}/files`}
            className="px-3 py-1.5 rounded-lg bg-indigo-600 hover:bg-indigo-700 text-white text-xs font-medium transition-colors flex-shrink-0"
          >
            {t('bucket.browseFiles')}
          </Link>
        </div>
      )}

      {/* Versioning toggle */}
      <Section title={t('bucket.versioning')}>
        <div className="flex items-center justify-between">
          <div>
            <p className="text-sm text-gray-700 dark:text-gray-300">
              {t('bucket.statusLabel')} <span className={`font-medium ${versioning === 'Enabled' ? 'text-green-600 dark:text-green-400' : 'text-gray-500 dark:text-gray-400'}`}>
                {versioning || t('bucket.notConfigured')}
              </span>
            </p>
            <p className="text-xs text-gray-500 dark:text-gray-400 mt-1">
              {t('bucket.versioningHint')}
            </p>
          </div>
          <button
            onClick={handleToggleVersioning}
            disabled={savingVersioning}
            className={`relative inline-flex h-6 w-11 items-center rounded-full transition-colors ${
              versioning === 'Enabled' ? 'bg-indigo-600' : 'bg-gray-300 dark:bg-gray-600'
            } ${savingVersioning ? 'opacity-50' : ''}`}
          >
            <span className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
              versioning === 'Enabled' ? 'translate-x-6' : 'translate-x-1'
            }`} />
          </button>
        </div>
      </Section>

      {/* Lifecycle rule editor */}
      <Section title={t('bucket.lifecycle')}>
        <div className="grid grid-cols-4 gap-4 mb-3">
          <div>
            <label className="block text-xs font-medium text-gray-500 dark:text-gray-400 mb-1">{t('bucket.expirationDays')}</label>
            <input
              type="number"
              value={lcExpDays}
              onChange={e => setLcExpDays(e.target.value)}
              className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none"
              placeholder="30"
            />
          </div>
          <div>
            <label className="block text-xs font-medium text-gray-500 dark:text-gray-400 mb-1" title={t('bucket.abortUploadsHint')}>{t('bucket.abortUploads')}</label>
            <input
              type="number"
              value={lcAbortDays}
              onChange={e => setLcAbortDays(e.target.value)}
              className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none"
              placeholder="7"
            />
          </div>
          <div>
            <label className="block text-xs font-medium text-gray-500 dark:text-gray-400 mb-1">{t('bucket.keyPrefix')}</label>
            <input
              type="text"
              value={lcPrefix}
              onChange={e => setLcPrefix(e.target.value)}
              className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none"
              placeholder="logs/"
            />
          </div>
          <div>
            <label className="block text-xs font-medium text-gray-500 dark:text-gray-400 mb-1">{t('common.status')}</label>
            <select
              value={lcStatus}
              onChange={e => setLcStatus(e.target.value)}
              className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none"
            >
              <option value="Enabled">{t('common.enabled')}</option>
              <option value="Disabled">{t('common.disabled')}</option>
            </select>
          </div>
        </div>
        <div className="flex gap-2">
          <button
            onClick={handleSaveLifecycle}
            disabled={savingLifecycle || (!lcExpDays && !lcAbortDays)}
            className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium transition-colors"
          >
            {savingLifecycle ? t('common.saving') : t('bucket.saveRule')}
          </button>
          {lifecycleRule && (
            <button
              onClick={handleDeleteLifecycle}
              disabled={savingLifecycle}
              className="px-4 py-2 rounded-lg border border-red-300 dark:border-red-700 text-red-600 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-900/20 text-sm font-medium transition-colors"
            >
              {t('bucket.remove')}
            </button>
          )}
        </div>
      </Section>

      {/* CORS editor */}
      <Section title={t('bucket.cors')}>
        <textarea
          value={corsText}
          onChange={e => setCorsText(e.target.value)}
          rows={6}
          className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm font-mono focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none mb-3"
          placeholder={`[{"allowed_origins":["*"],"allowed_methods":["GET","PUT"],"allowed_headers":["*"],"expose_headers":["ETag"],"max_age_secs":3600}]`}
        />
        <div className="flex gap-2">
          <button
            onClick={handleSaveCors}
            disabled={savingCors}
            className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium transition-colors"
          >
            {savingCors ? t('common.saving') : t('bucket.saveCors')}
          </button>
          {corsRules.length > 0 && (
            <button
              onClick={handleDeleteCors}
              disabled={savingCors}
              className="px-4 py-2 rounded-lg border border-red-300 dark:border-red-700 text-red-600 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-900/20 text-sm font-medium transition-colors"
            >
              {t('bucket.remove')}
            </button>
          )}
        </div>
      </Section>

      {/* Quota editor */}
      <Section title={t('bucket.quota')}>
        <div className="grid grid-cols-2 gap-4 mb-3">
          <div>
            <label className="block text-xs font-medium text-gray-500 dark:text-gray-400 mb-1">{t('bucket.maxSize')}</label>
            <input
              type="number"
              value={maxSizeBytes}
              onChange={(e) => setMaxSizeBytes(e.target.value)}
              className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none"
              placeholder={t('bucket.zeroUnlimited')}
            />
          </div>
          <div>
            <label className="block text-xs font-medium text-gray-500 dark:text-gray-400 mb-1">{t('bucket.maxObjects')}</label>
            <input
              type="number"
              value={maxObjects}
              onChange={(e) => setMaxObjects(e.target.value)}
              className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none"
              placeholder={t('bucket.zeroUnlimited')}
            />
          </div>
        </div>
        <button
          onClick={handleSaveQuota}
          disabled={savingQuota}
          className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium transition-colors"
        >
          {savingQuota ? t('common.saving') : t('bucket.saveQuota')}
        </button>
      </Section>

      {/* Policy editor */}
      <Section title={t('bucket.policy')}>
        <textarea
          value={policyText}
          onChange={(e) => setPolicyText(e.target.value)}
          rows={10}
          className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm font-mono focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none mb-3"
          placeholder='{"Version":"2012-10-17","Statement":[...]}'
        />
        <button
          onClick={handleSavePolicy}
          disabled={savingPolicy || !policyText.trim()}
          className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium transition-colors"
        >
          {savingPolicy ? t('common.saving') : t('bucket.savePolicy')}
        </button>
      </Section>

      <EncryptionPanel bucket={bucket.name} />

      <div className="mb-4">
        <SnapshotsPanel bucket={bucket.name} versioningEnabled={versioning === 'Enabled'} />
      </div>
    </div>
  )
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-5 mb-4">
      <h3 className="text-sm font-semibold text-gray-900 dark:text-white mb-3">{title}</h3>
      {children}
    </div>
  )
}

function InfoCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4">
      <p className="text-xs text-gray-500 dark:text-gray-400 mb-1">{label}</p>
      <p className="text-lg font-semibold text-gray-900 dark:text-white">{value}</p>
    </div>
  )
}

function formatSize(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0)} ${units[i]}`
}

function formatDate(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}
