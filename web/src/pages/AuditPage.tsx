import { useState, useEffect, useCallback, useMemo } from 'react'
import { useI18n } from '../i18n'
import { useSort, SortHeader, usePaged, Pagination, EmptyState } from '../components/table'
import { queryAudit, type AuditEntry } from '../api/audit'

type SortField = 'time' | 'user' | 'action' | 'resource' | 'effect' | 'sourceIp' | 'statusCode'


// Newest first is the useful order for a timestamp, every other column starts A to Z.
const auditFirstDir = (field: SortField) => (field === 'time' ? 'desc' : 'asc')

export default function AuditPage() {
  const { t } = useI18n()
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const [filterUser, setFilterUser] = useState('')
  const [filterBucket, setFilterBucket] = useState('')
  const [limit, setLimit] = useState(100)

  // Sort state
  const { field: sortField, dir: sortDir, toggle: handleSort } = useSort<SortField>('time', 'desc', auditFirstDir)

  // Pagination

  const fetchAudit = useCallback(async () => {
    try {
      const data = await queryAudit({
        limit,
        user: filterUser || undefined,
        bucket: filterBucket || undefined,
      })
      setEntries(data || [])
      paging.setPage(0)
    } catch (err) {
      setError(err instanceof Error ? err.message : t('audit.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [limit, filterUser, filterBucket])

  useEffect(() => { fetchAudit() }, [fetchAudit])

  // auto-refresh every 10s
  useEffect(() => {
    const id = setInterval(fetchAudit, 10000)
    return () => clearInterval(id)
  }, [fetchAudit])

  // Sort logic

  const sortedEntries = useMemo(() => {
    const sorted = [...entries]
    sorted.sort((a, b) => {
      let cmp = 0
      switch (sortField) {
        case 'time': cmp = (a.time || '').localeCompare(b.time || ''); break
        case 'user': cmp = (a.user || '').localeCompare(b.user || ''); break
        case 'action': cmp = (a.action || '').localeCompare(b.action || ''); break
        case 'resource': cmp = (a.resource || '').localeCompare(b.resource || ''); break
        case 'effect': cmp = (a.effect || '').localeCompare(b.effect || ''); break
        case 'sourceIp': cmp = (a.sourceIp || '').localeCompare(b.sourceIp || ''); break
        case 'statusCode': cmp = (a.statusCode || 0) - (b.statusCode || 0); break
      }
      return sortDir === 'asc' ? cmp : -cmp
    })
    return sorted
  }, [entries, sortField, sortDir])

  // Pagination
  const paging = usePaged(sortedEntries)
  const pagedEntries = paging.paged


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
        <h2 className="text-xl font-semibold text-gray-900 dark:text-white">{t('audit.auditTrail')}</h2>
        <p className="text-sm text-gray-500 dark:text-gray-400 mt-0.5">{t('audit.securityAndAccessAuditLog')}</p>
      </div>

      {error && (
        <div className="mb-4 p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
          {error}
        </div>
      )}

      {/* Filter bar */}
      <div className="flex flex-wrap gap-3 mb-4">
        <input type="text" placeholder={t('audit.filterByUser')} value={filterUser}
          onChange={e => setFilterUser(e.target.value)}
          className="px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-900 dark:text-white text-sm w-44" />
        <input type="text" placeholder={t('audit.filterByBucket')} value={filterBucket}
          onChange={e => setFilterBucket(e.target.value)}
          className="px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-900 dark:text-white text-sm w-44" />
        <select value={limit} onChange={e => setLimit(Number(e.target.value))}
          className="px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-900 dark:text-white text-sm">
          <option value={50}>{t('audit.entriesOption', { count: 50 })}</option>
          <option value={100}>{t('audit.entriesOption', { count: 100 })}</option>
          <option value={500}>{t('audit.entriesOption', { count: 500 })}</option>
        </select>
      </div>

      {/* Audit table */}
      <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-gray-200 dark:border-gray-700">
                <SortHeader sortField={sortField} sortDir={sortDir} onSort={handleSort} field="time" label={t('audit.time')} />
                <SortHeader sortField={sortField} sortDir={sortDir} onSort={handleSort} field="user" label={t('audit.user')} />
                <SortHeader sortField={sortField} sortDir={sortDir} onSort={handleSort} field="action" label={t('audit.action')} />
                <SortHeader sortField={sortField} sortDir={sortDir} onSort={handleSort} field="resource" label={t('audit.resource')} />
                <SortHeader sortField={sortField} sortDir={sortDir} onSort={handleSort} field="effect" label={t('audit.effect')} />
                <SortHeader sortField={sortField} sortDir={sortDir} onSort={handleSort} field="sourceIp" label={t('audit.sourceIp')} />
                <SortHeader sortField={sortField} sortDir={sortDir} onSort={handleSort} field="statusCode" label={t('audit.status')} />
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100 dark:divide-gray-700/50">
              {pagedEntries.map((e, i) => (
                <tr key={i} className="hover:bg-gray-50 dark:hover:bg-gray-700/30 transition-colors">
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400 whitespace-nowrap">
                    {new Date(e.time).toLocaleString()}
                  </td>
                  <td className="px-4 py-3 font-medium text-gray-900 dark:text-white">{e.user || '-'}</td>
                  <td className="px-4 py-3 text-gray-700 dark:text-gray-300">{e.action}</td>
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400 font-mono text-xs max-w-xs truncate">{e.resource}</td>
                  <td className="px-4 py-3">
                    <span className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ${
                      e.effect === 'Allow'
                        ? 'bg-green-100 dark:bg-green-900/30 text-green-700 dark:text-green-400'
                        : e.effect === 'Deny'
                          ? 'bg-red-100 dark:bg-red-900/30 text-red-700 dark:text-red-400'
                          : 'bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-400'
                    }`}>
                      {e.effect || '-'}
                    </span>
                  </td>
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400 font-mono text-xs">{e.sourceIp || '-'}</td>
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400">{e.statusCode || '-'}</td>
                </tr>
              ))}
              {entries.length === 0 && <EmptyState colSpan={7} title={t('audit.noAuditEntries')} />}
            </tbody>
          </table>
        </div>
      </div>

      <Pagination paging={paging} />
    </div>
  )
}
