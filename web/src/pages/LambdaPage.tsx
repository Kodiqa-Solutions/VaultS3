import { useState, useEffect, useCallback, useMemo } from 'react'
import { useI18n } from '../i18n'
import { useSort, SortHeader, usePaged, Pagination, EmptyState } from '../components/table'
import { useInFlight } from '../hooks/useInFlight'
import { getLambdaStatus, listLambdaTriggers, deleteBucketTriggers, type LambdaStatus, type BucketTriggers } from '../api/lambda'
import { useToast } from '../hooks/useToast'
import FeatureOffNotice from '../components/FeatureOffNotice'

type LSortField = 'bucket' | 'functionURL' | 'events' | 'keyFilter'

export default function LambdaPage() {
  const { t } = useI18n()
  const [status, setStatus] = useState<LambdaStatus | null>(null)
  const [triggers, setTriggers] = useState<BucketTriggers[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)
  const [deleting, runDelete] = useInFlight()
  const { addToast } = useToast()
  const { field: lSortField, dir: lSortDir, toggle: handleLSort } = useSort<LSortField>('bucket', 'asc')

  const fetchData = useCallback(async () => {
    try {
      const s = await getLambdaStatus()
      setStatus(s)
    } catch {
      setStatus({ enabled: false, totalTriggers: 0, buckets: 0, queueDepth: 0 })
    }
    try {
      const t = await listLambdaTriggers()
      setTriggers(t || [])
    } catch {
      setTriggers([])
    }
    setLoading(false)
  }, [])

  useEffect(() => { fetchData() }, [fetchData])

  const handleDeleteTriggers = (bucket: string) => runDelete(async () => {
    setError('')
    try {
      await deleteBucketTriggers(bucket)
      setDeleteTarget(null)
      addToast('success', t('lambda.triggersDeleted', { bucket }))
      fetchData()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('lambda.deleteFailed'))
    }
  })


  // Flatten triggers for sorting
  const flatTriggers = useMemo(() => {
    const flat = triggers.flatMap(bt => (bt.triggers || []).map(t => ({ bucket: bt.bucket, ...t })))
    flat.sort((a, b) => {
      let cmp = 0
      switch (lSortField) {
        case 'bucket': cmp = a.bucket.localeCompare(b.bucket); break
        case 'functionURL': cmp = (a.functionURL || '').localeCompare(b.functionURL || ''); break
        case 'events': cmp = (a.events?.length || 0) - (b.events?.length || 0); break
        case 'keyFilter': cmp = (a.keyFilter || '').localeCompare(b.keyFilter || ''); break
      }
      return lSortDir === 'asc' ? cmp : -cmp
    })
    return flat
  }, [triggers, lSortField, lSortDir])
  const paging = usePaged(flatTriggers)


  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-indigo-600" />
      </div>
    )
  }

  return (
    <div>
      <div className="mb-6">
        <h2 className="text-xl font-semibold text-gray-900 dark:text-white">{t('lambda.lambdaTriggers')}</h2>
        <p className="text-sm text-gray-500 dark:text-gray-400 mt-0.5">{t('lambda.eventDrivenFunctionTriggers')}</p>
      </div>

      {error && (
        <div className="mb-4 p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
          {error}
        </div>
      )}

      {status && !status.enabled && (
        <FeatureOffNotice feature="lambda" titleKey="lambda.lambdaTriggersNotEnabled" bodyKey="lambda.notEnabledBody" />
      )}

      {/* Status cards */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-4 mb-6">
        {[
          { label: t('common.status'), value: status?.enabled ? t('common.enabled') : t('common.disabled'), color: status?.enabled ? 'text-green-600 dark:text-green-400' : 'text-gray-500' },
          { label: t('lambda.totalTriggers'), value: status?.totalTriggers ?? 0 },
          { label: t('nav.buckets'), value: status?.buckets ?? 0 },
          { label: t('lambda.queueDepth'), value: status?.queueDepth ?? 0 },
        ].map(card => (
          <div key={card.label} className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4">
            <p className="text-xs text-gray-500 dark:text-gray-400">{card.label}</p>
            <p className={`text-2xl font-bold mt-1 ${card.color || 'text-gray-900 dark:text-white'}`}>{card.value}</p>
          </div>
        ))}
      </div>

      {/* Triggers table */}
      <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-gray-200 dark:border-gray-700">
              <SortHeader sortField={lSortField} sortDir={lSortDir} onSort={handleLSort} field="bucket" label={t('lambda.bucket')} />
              <SortHeader sortField={lSortField} sortDir={lSortDir} onSort={handleLSort} field="functionURL" label={t('lambda.functionUrl')} />
              <SortHeader sortField={lSortField} sortDir={lSortDir} onSort={handleLSort} field="events" label={t('lambda.events')} />
              <SortHeader sortField={lSortField} sortDir={lSortDir} onSort={handleLSort} field="keyFilter" label={t('lambda.keyFilter')} />
              <th className="text-right px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">{t('lambda.actions')}</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-gray-100 dark:divide-gray-700/50">
            {paging.paged.map((trigger, i) => (
              <tr key={`${trigger.bucket}-${i}`} className="hover:bg-gray-50 dark:hover:bg-gray-700/30 transition-colors">
                <td className="px-4 py-3 font-medium text-gray-900 dark:text-white">{trigger.bucket}</td>
                <td className="px-4 py-3 text-gray-500 dark:text-gray-400 font-mono text-xs max-w-xs truncate">{trigger.functionURL}</td>
                <td className="px-4 py-3">
                  <div className="flex flex-wrap gap-1">
                    {(trigger.events || []).map(ev => (
                      <span key={ev} className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-purple-100 dark:bg-purple-900/30 text-purple-700 dark:text-purple-400">
                        {ev}
                      </span>
                    ))}
                  </div>
                </td>
                <td className="px-4 py-3 text-gray-500 dark:text-gray-400 font-mono text-xs">{trigger.keyFilter || '*'}</td>
                <td className="px-4 py-3 text-right">
                  <button onClick={() => setDeleteTarget(trigger.bucket)}
                    className="text-gray-400 hover:text-red-600 dark:hover:text-red-400 transition-colors" title={t('lambda.deleteTriggers')}>
                    <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                      <path strokeLinecap="round" strokeLinejoin="round" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
                    </svg>
                  </button>
                </td>
              </tr>
            ))}
            {flatTriggers.length === 0 && <EmptyState colSpan={5} title={t('lambda.noLambdaTriggersConfigured')} />}
          </tbody>
        </table>
      </div>
      <Pagination paging={paging} />

      {/* Delete confirmation */}
      {deleteTarget && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
          <div className="bg-white dark:bg-gray-800 rounded-xl shadow-xl border border-gray-200 dark:border-gray-700 p-6 w-full max-w-sm mx-4">
            <h3 className="text-lg font-semibold text-gray-900 dark:text-white mb-2">{t('lambda.deleteTriggers2')}</h3>
            <p className="text-sm text-gray-600 dark:text-gray-400 mb-4">
              {t('lambda.removeAllFor')} <strong>{deleteTarget}</strong>?
            </p>
            <div className="flex gap-2 justify-end">
              <button onClick={() => setDeleteTarget(null)} disabled={deleting}
                className="px-4 py-2 rounded-lg text-sm text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors disabled:opacity-50">{t('lambda.cancel')}</button>
              <button onClick={() => handleDeleteTriggers(deleteTarget)} disabled={deleting}
                className="px-4 py-2 rounded-lg bg-red-600 hover:bg-red-700 text-white text-sm font-medium transition-colors disabled:opacity-50">{t('lambda.delete')}</button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
