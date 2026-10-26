import { useState, useEffect, useCallback, useMemo } from 'react'
import { useI18n } from '../i18n'
import { useSort, SortHeader, usePaged, Pagination, EmptyState } from '../components/table'
import { listBackups, getBackupStatus, triggerBackup, type BackupRecord, type BackupStatus } from '../api/backup'
import { useToast } from '../hooks/useToast'
import FeatureOffNotice from '../components/FeatureOffNotice'

type BSortField = 'type' | 'target' | 'startTime' | 'objects' | 'size' | 'status'

function formatSize(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}


function StatusCard({ label, value, color }: { label: string; value: string; color?: string }) {
  return (
    <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4">
      <p className="text-xs text-gray-500 dark:text-gray-400">{label}</p>
      <p className={`text-2xl font-bold mt-1 ${color || 'text-gray-900 dark:text-white'}`}>{value}</p>
    </div>
  )
}

export default function BackupPage() {
  const { t } = useI18n()
  const [backups, setBackups] = useState<BackupRecord[]>([])
  const [status, setStatus] = useState<BackupStatus | null>(null)
  const [loading, setLoading] = useState(true)
  const [triggering, setTriggering] = useState(false)
  const { addToast } = useToast()
  const { field: sortField, dir: sortDir, toggle: handleSort } = useSort<BSortField>('startTime', 'desc')

  const fetchData = useCallback(async () => {
    try {
      const s = await getBackupStatus()
      setStatus(s)
    } catch {
      setStatus({ enabled: false, running: false, targets: 0 })
    }
    try {
      const b = await listBackups()
      setBackups(Array.isArray(b) ? b : [])
    } catch {
      setBackups([])
    }
    setLoading(false)
  }, [])

  useEffect(() => { fetchData() }, [fetchData])

  const handleTrigger = async () => {
    setTriggering(true)
    try {
      await triggerBackup()
      addToast('success', t('backup.triggered'))
      fetchData()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('backup.triggerFailed'))
    } finally {
      setTriggering(false)
    }
  }


  const sorted = useMemo(() => [...backups].sort((a, b) => {
    let cmp = 0
    switch (sortField) {
      case 'type': cmp = (a.type || '').localeCompare(b.type || ''); break
      case 'target': cmp = (a.target || '').localeCompare(b.target || ''); break
      case 'startTime': cmp = (a.startTime || '').localeCompare(b.startTime || ''); break
      case 'objects': cmp = (a.objects || 0) - (b.objects || 0); break
      case 'size': cmp = (a.size || 0) - (b.size || 0); break
      case 'status': cmp = (a.status || '').localeCompare(b.status || ''); break
    }
    return sortDir === 'asc' ? cmp : -cmp
  }), [backups, sortField, sortDir])
  const paging = usePaged(sorted)

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-indigo-600" />
      </div>
    )
  }


  const statusColor = (s: string) => {
    switch (s) {
      case 'completed': return 'bg-green-100 dark:bg-green-900/30 text-green-700 dark:text-green-400'
      case 'running': return 'bg-amber-100 dark:bg-amber-900/30 text-amber-700 dark:text-amber-400'
      case 'failed': return 'bg-red-100 dark:bg-red-900/30 text-red-700 dark:text-red-400'
      default: return 'bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-400'
    }
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <div>
          <h2 className="text-xl font-semibold text-gray-900 dark:text-white">{t('backup.backups')}</h2>
          <p className="text-sm text-gray-500 dark:text-gray-400 mt-0.5">{t('backup.backupHistoryAndManagement')}</p>
        </div>
        <button onClick={handleTrigger} disabled={triggering}
          className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium transition-colors">
          {triggering ? t('backup.starting') : t('backup.triggerBackup')}
        </button>
      </div>

      {status && !status.enabled && (
        <FeatureOffNotice feature="backup" titleKey="backup.backupsNotEnabled" bodyKey="backup.notEnabledBody" />
      )}

      {/* Status cards */}
      <div className="grid grid-cols-3 gap-4 mb-6">
        <StatusCard
          label={t('backup.status')}
          value={status?.enabled ? t('common.enabled') : t('common.disabled')}
          color={status?.enabled ? 'text-green-600 dark:text-green-400' : 'text-gray-500'}
        />
        <StatusCard
          label={t('backup.running')}
          value={status?.running ? 'Yes' : 'No'}
          color={status?.running ? 'text-amber-600 dark:text-amber-400' : undefined}
        />
        <StatusCard
          label={t('backup.targets')}
          value={String(status?.targets ?? 0)}
        />
      </div>

      {/* Backup history table */}
      <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-gray-200 dark:border-gray-700">
                <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">{t('backup.id')}</th>
                <SortHeader field="type" label={t('backup.type')} sortField={sortField} sortDir={sortDir} onSort={handleSort} />
                <SortHeader field="target" label={t('backup.target')} sortField={sortField} sortDir={sortDir} onSort={handleSort} />
                <SortHeader field="startTime" label={t('backup.started')} sortField={sortField} sortDir={sortDir} onSort={handleSort} />
                <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">{t('backup.ended')}</th>
                <SortHeader field="objects" label={t('backup.objects')} sortField={sortField} sortDir={sortDir} onSort={handleSort} />
                <SortHeader field="size" label={t('backup.size')} sortField={sortField} sortDir={sortDir} onSort={handleSort} />
                <SortHeader field="status" label={t('backup.status')} sortField={sortField} sortDir={sortDir} onSort={handleSort} />
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100 dark:divide-gray-700/50">
              {paging.paged.map(b => (
                <tr key={b.id} className="hover:bg-gray-50 dark:hover:bg-gray-700/30 transition-colors">
                  <td className="px-4 py-3 font-mono text-xs text-gray-900 dark:text-white">{(b.id || '').slice(0, 8)}</td>
                  <td className="px-4 py-3 text-gray-700 dark:text-gray-300">{b.type}</td>
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400">{b.target}</td>
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400 text-xs whitespace-nowrap">
                    {b.startTime ? new Date(b.startTime).toLocaleString() : '-'}
                  </td>
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400 text-xs whitespace-nowrap">
                    {b.endTime ? new Date(b.endTime).toLocaleString() : '-'}
                  </td>
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400">{b.objects}</td>
                  <td className="px-4 py-3 text-gray-500 dark:text-gray-400">{formatSize(b.size)}</td>
                  <td className="px-4 py-3">
                    <span className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ${statusColor(b.status)}`}>
                      {b.status}
                    </span>
                  </td>
                </tr>
              ))}
              {backups.length === 0 && <EmptyState colSpan={8} title={t('backup.noBackupsYet')} />}
            </tbody>
          </table>
        </div>
      </div>
      <Pagination paging={paging} />
    </div>
  )
}
