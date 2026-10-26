import { useState, useEffect, useCallback, useMemo } from 'react'
import { Link } from 'react-router-dom'
import { useI18n } from '../i18n'
import { useSort, SortHeader, usePaged, Pagination } from '../components/table'
import { featureAnchor } from '../components/featureSetup'
import { listNotifications, type NotificationConfig } from '../api/notifications'

type SortField = 'bucket' | 'webhookURL' | 'events'

export default function NotificationsPage() {
  const { t } = useI18n()
  const [configs, setConfigs] = useState<NotificationConfig[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const { field: sortField, dir: sortDir, toggle: handleSort } = useSort<SortField>('bucket', 'asc')

  const fetchData = useCallback(async () => {
    try {
      const data = await listNotifications()
      setConfigs(data || [])
    } catch (err) {
      setError(err instanceof Error ? err.message : t('notifications.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { fetchData() }, [fetchData])


  const sorted = useMemo(() => {
    const s = [...configs]
    s.sort((a, b) => {
      let cmp = 0
      switch (sortField) {
        case 'bucket': cmp = a.bucket.localeCompare(b.bucket); break
        case 'webhookURL': cmp = (a.webhookURL || '').localeCompare(b.webhookURL || ''); break
        case 'events': cmp = (a.events?.length || 0) - (b.events?.length || 0); break
      }
      return sortDir === 'asc' ? cmp : -cmp
    })
    return s
  }, [configs, sortField, sortDir])
  const paging = usePaged(sorted)


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
        <h2 className="text-xl font-semibold text-gray-900 dark:text-white">{t('notifications.notifications')}</h2>
        <p className="text-sm text-gray-500 dark:text-gray-400 mt-0.5">{t('notifications.eventNotificationConfigurations')}</p>
      </div>

      {error && (
        <div className="mb-4 p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
          {error}
        </div>
      )}

      {configs.length === 0 ? (
        <div className="mb-6 p-5 rounded-xl bg-indigo-50 dark:bg-indigo-900/20 border border-indigo-200 dark:border-indigo-800">
          <h3 className="text-sm font-semibold text-indigo-900 dark:text-indigo-300 mb-2">{t('notifications.noNotificationsConfigured')}</h3>
          <p className="text-sm text-indigo-700 dark:text-indigo-400 mb-3">{t('notifications.emptyBody')}</p>
          <Link
            to={`/settings#${featureAnchor('notifyBackends')}`}
            className="text-sm font-medium text-indigo-700 dark:text-indigo-300 underline underline-offset-2 hover:text-indigo-900 dark:hover:text-indigo-200"
          >
            {t('notifications.backendsHowTo')}
          </Link>
        </div>
      ) : (
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-gray-200 dark:border-gray-700">
                <SortHeader sortField={sortField} sortDir={sortDir} onSort={handleSort} field="bucket" label={t('notifications.bucket')} />
                <SortHeader sortField={sortField} sortDir={sortDir} onSort={handleSort} field="webhookURL" label={t('notifications.webhookUrl')} />
                <SortHeader sortField={sortField} sortDir={sortDir} onSort={handleSort} field="events" label={t('notifications.events')} />
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100 dark:divide-gray-700/50">
              {paging.paged.map((c, i) => (
                <tr key={i} className="hover:bg-gray-50 dark:hover:bg-gray-700/30 transition-colors">
                  <td className="px-4 py-3 font-medium text-gray-900 dark:text-white">{c.bucket}</td>
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400 font-mono text-xs max-w-sm truncate">{c.webhookURL}</td>
                  <td className="px-4 py-3">
                    <div className="flex flex-wrap gap-1">
                      {(c.events || []).map(ev => (
                        <span key={ev} className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-indigo-100 dark:bg-indigo-900/30 text-indigo-700 dark:text-indigo-400">
                          {ev}
                        </span>
                      ))}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <Pagination paging={paging} />
    </div>
  )
}
