import { useState, useEffect, useCallback, useMemo } from 'react'
import { useI18n } from '../i18n'
import { useInFlight } from '../hooks/useInFlight'
import { Link } from 'react-router-dom'
import { listBuckets, createBucket, deleteBucket, setBucketVersioning } from '../api/buckets'
import { setLifecycleRule } from '../api/buckets'
import type { Bucket } from '../api/buckets'
import { useToast } from '../hooks/useToast'

type SortField = 'name' | 'objectCount' | 'size' | 'createdAt'
type SortDir = 'asc' | 'desc'

export default function BucketsPage() {
  const { t } = useI18n()
  const [buckets, setBuckets] = useState<Bucket[]>([])
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const [newName, setNewName] = useState('')
  const [creating, setCreating] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)
  const [deleting, runDelete] = useInFlight()
  const [error, setError] = useState('')
  const { addToast } = useToast()

  // Enhanced create options
  const [enableVersioning, setEnableVersioning] = useState(false)
  const [autoExpireDays, setAutoExpireDays] = useState('')

  // Sort state
  const [sortField, setSortField] = useState<SortField>('name')
  const [sortDir, setSortDir] = useState<SortDir>('asc')

  const fetchBuckets = useCallback(async () => {
    try {
      const data = await listBuckets()
      setBuckets(data)
    } catch (err) {
      setError(err instanceof Error ? err.message : t('buckets.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { fetchBuckets() }, [fetchBuckets])

  const handleCreate = async () => {
    if (!newName.trim()) return
    setCreating(true)
    setError('')
    try {
      await createBucket(newName.trim())
      // Apply optional versioning
      if (enableVersioning) {
        try { await setBucketVersioning(newName.trim(), 'Enabled') } catch {}
      }
      // Apply optional lifecycle
      if (autoExpireDays && Number(autoExpireDays) > 0) {
        try { await setLifecycleRule(newName.trim(), { expirationDays: Number(autoExpireDays), prefix: '', status: 'Enabled' }) } catch {}
      }
      setNewName('')
      setShowCreate(false)
      setEnableVersioning(false)
      setAutoExpireDays('')
      addToast('success', t('buckets.created', { name: newName.trim() }))
      fetchBuckets()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('buckets.createFailed'))
    } finally {
      setCreating(false)
    }
  }

  const handleDelete = (name: string) => runDelete(async () => {
    setError('')
    try {
      await deleteBucket(name)
      setDeleteTarget(null)
      addToast('success', t('buckets.deleted', { name }))
      fetchBuckets()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('buckets.deleteFailed'))
    }
  })

  const handleSort = (field: SortField) => {
    if (sortField === field) {
      setSortDir(d => d === 'asc' ? 'desc' : 'asc')
    } else {
      setSortField(field)
      setSortDir('asc')
    }
  }

  const sortedBuckets = useMemo(() => {
    const sorted = [...buckets]
    sorted.sort((a, b) => {
      let cmp = 0
      switch (sortField) {
        case 'name': cmp = a.name.localeCompare(b.name); break
        case 'objectCount': cmp = a.objectCount - b.objectCount; break
        case 'size': cmp = a.size - b.size; break
        case 'createdAt': cmp = (a.createdAt || '').localeCompare(b.createdAt || ''); break
      }
      return sortDir === 'asc' ? cmp : -cmp
    })
    return sorted
  }, [buckets, sortField, sortDir])

  const SortHeader = ({ field, label }: { field: SortField; label: string }) => (
    <th
      onClick={() => handleSort(field)}
      className="text-left px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider cursor-pointer hover:text-indigo-600 dark:hover:text-indigo-400 select-none"
    >
      <span className="inline-flex items-center gap-1">
        {label}
        {sortField === field && (
          <span className="text-indigo-600 dark:text-indigo-400">{sortDir === 'asc' ? '\u2191' : '\u2193'}</span>
        )}
      </span>
    </th>
  )

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-indigo-600" />
      </div>
    )
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <div>
          <h2 className="text-xl font-semibold text-gray-900 dark:text-white">{t('nav.buckets')}</h2>
          <p className="text-sm text-gray-500 dark:text-gray-400 mt-0.5">{buckets.length} bucket{buckets.length !== 1 ? 's' : ''}</p>
        </div>
        <button
          onClick={() => setShowCreate(true)}
          className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 text-white text-sm font-medium transition-colors"
        >
          {t('buckets.create')}
        </button>
      </div>

      {error && (
        <div className="mb-4 p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
          {error}
        </div>
      )}

      {/* Create bucket modal */}
      {showCreate && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
          <div className="bg-white dark:bg-gray-800 rounded-xl shadow-xl border border-gray-200 dark:border-gray-700 p-6 w-full max-w-sm mx-4">
            <h3 className="text-lg font-semibold text-gray-900 dark:text-white mb-4">{t('buckets.create')}</h3>
            <input
              type="text"
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && handleCreate()}
              className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none mb-4"
              placeholder={t('buckets.namePlaceholder')}
              autoFocus
            />
            <div className="space-y-3 mb-4">
              <label className="flex items-center gap-2 cursor-pointer">
                <input
                  type="checkbox"
                  checked={enableVersioning}
                  onChange={e => setEnableVersioning(e.target.checked)}
                  className="rounded border-gray-300 dark:border-gray-600 text-indigo-600 focus:ring-indigo-500"
                />
                <span className="text-sm text-gray-700 dark:text-gray-300">{t('buckets.enableVersioning')}</span>
              </label>
              <div>
                <label className="block text-xs font-medium text-gray-500 dark:text-gray-400 mb-1">{t('buckets.autoExpire')}</label>
                <input
                  type="number"
                  value={autoExpireDays}
                  onChange={e => setAutoExpireDays(e.target.value)}
                  className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none"
                  placeholder={t('buckets.autoExpirePlaceholder')}
                  min="1"
                />
              </div>
            </div>
            <div className="flex gap-2 justify-end">
              <button
                onClick={() => { setShowCreate(false); setNewName(''); setEnableVersioning(false); setAutoExpireDays('') }}
                className="px-4 py-2 rounded-lg text-sm text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
              >
                {t('common.cancel')}
              </button>
              <button
                onClick={handleCreate}
                disabled={creating || !newName.trim()}
                className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium transition-colors"
              >
                {creating ? t('buckets.creating') : t('common.create')}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Delete confirmation modal */}
      {deleteTarget && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
          <div className="bg-white dark:bg-gray-800 rounded-xl shadow-xl border border-gray-200 dark:border-gray-700 p-6 w-full max-w-sm mx-4">
            <h3 className="text-lg font-semibold text-gray-900 dark:text-white mb-2">{t('buckets.deleteTitle')}</h3>
            <p className="text-sm text-gray-600 dark:text-gray-400 mb-4">
              {t('buckets.deleteConfirm')} <strong>{deleteTarget}</strong>? {t('common.cannotBeUndone')}
            </p>
            <div className="flex gap-2 justify-end">
              <button
                onClick={() => setDeleteTarget(null)}
                disabled={deleting}
                className="px-4 py-2 rounded-lg text-sm text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors disabled:opacity-50"
              >
                {t('common.cancel')}
              </button>
              <button
                onClick={() => handleDelete(deleteTarget)}
                disabled={deleting}
                className="px-4 py-2 rounded-lg bg-red-600 hover:bg-red-700 text-white text-sm font-medium transition-colors disabled:opacity-50"
              >
                {t('common.delete')}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Bucket table */}
      {buckets.length === 0 ? (
        <div className="text-center py-16 bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700">
          <svg className="w-12 h-12 mx-auto text-gray-400 dark:text-gray-500 mb-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={1.5}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M20 7l-8-4-8 4m16 0l-8 4m8-4v10l-8 4m0-10L4 7m8 4v10M4 7v10l8 4" />
          </svg>
          <p className="text-gray-500 dark:text-gray-400 text-sm">{t('buckets.empty')}</p>
          <button
            onClick={() => setShowCreate(true)}
            className="mt-3 text-sm text-indigo-600 dark:text-indigo-400 hover:underline"
          >
            {t('buckets.createFirst')}
          </button>
        </div>
      ) : (
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-gray-200 dark:border-gray-700">
                <SortHeader field="name" label={t('common.name')} />
                <SortHeader field="objectCount" label={t('home.objects')} />
                <SortHeader field="size" label={t('common.size')} />
                <SortHeader field="createdAt" label={t('common.created')} />
                <th className="text-right px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">{t('common.actions')}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100 dark:divide-gray-700/50">
              {sortedBuckets.map((b) => (
                <tr key={b.name} className="hover:bg-gray-50 dark:hover:bg-gray-700/30 transition-colors">
                  <td className="px-4 py-3">
                    <Link
                      to={`/buckets/${b.name}`}
                      className="text-indigo-600 dark:text-indigo-400 font-medium hover:underline"
                    >
                      {b.name}
                    </Link>
                  </td>
                  <td className="px-4 py-3 text-gray-700 dark:text-gray-300">{b.objectCount}</td>
                  <td className="px-4 py-3 text-gray-700 dark:text-gray-300">{formatSize(b.size)}</td>
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400">{formatDate(b.createdAt)}</td>
                  <td className="px-4 py-3 text-right">
                    <button
                      onClick={() => setDeleteTarget(b.name)}
                      className="text-gray-400 hover:text-red-600 dark:hover:text-red-400 transition-colors"
                      title={t('buckets.deleteBucket')}
                    >
                      <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                        <path strokeLinecap="round" strokeLinejoin="round" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
                      </svg>
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
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
