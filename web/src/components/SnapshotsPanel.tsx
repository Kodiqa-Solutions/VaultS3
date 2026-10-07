import { useState, useEffect, useCallback } from 'react'
import { useI18n } from '../i18n'
import {
  listSnapshots, createSnapshot, diffSnapshot, restoreSnapshot, deleteSnapshot,
  type Snapshot, type SnapshotDiff,
} from '../api/snapshots'
import { useToast } from '../hooks/useToast'
import { useInFlight } from '../hooks/useInFlight'

function formatSize(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

interface Props {
  bucket: string
  versioningEnabled: boolean
}

export default function SnapshotsPanel({ bucket, versioningEnabled }: Props) {
  const { t } = useI18n()
  const { addToast } = useToast()
  const [snaps, setSnaps] = useState<Snapshot[]>([])
  const [message, setMessage] = useState('')
  // One guard for create, restore and delete: each changes the snapshot list
  // the others act on, so only one runs at a time.
  const [busy, runBusy] = useInFlight()
  const [diffFor, setDiffFor] = useState<string | null>(null)
  const [diff, setDiff] = useState<SnapshotDiff | null>(null)

  const refresh = useCallback(async () => {
    try {
      setSnaps((await listSnapshots(bucket)) || [])
    } catch {
      /* ignore */
    }
  }, [bucket])

  useEffect(() => { refresh() }, [refresh])

  const handleCreate = () => runBusy(async () => {
    try {
      await createSnapshot(bucket, message.trim())
      setMessage('')
      addToast('success', t('snapshots.snapshotCreated'))
      await refresh()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('snapshots.failedToCreateSnapshot'))
    }
  })

  const handleDiff = async (id: string) => {
    if (diffFor === id) { setDiffFor(null); setDiff(null); return }
    try {
      const d = await diffSnapshot(bucket, id)
      setDiff(d)
      setDiffFor(id)
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('snapshots.diffFailed'))
    }
  }

  const handleRestore = (id: string) => runBusy(async () => {
    if (!window.confirm(t('snapshots.restoreConfirm'))) return
    try {
      const r = await restoreSnapshot(bucket, id)
      addToast('success', t('snapshots.restored', { reverted: r.reverted, removed: r.removed }) +
        (r.skipped ? t('snapshots.restoredSkipped', { skipped: r.skipped }) : ''))
      setDiffFor(null); setDiff(null)
      await refresh()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('snapshots.restoreFailed'))
    }
  })

  const handleDelete = (id: string) => runBusy(async () => {
    if (!window.confirm(t('snapshots.deleteThisSnapshotObjectDataIs'))) return
    try {
      await deleteSnapshot(bucket, id)
      addToast('success', t('snapshots.snapshotDeleted'))
      await refresh()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : t('snapshots.deleteFailed'))
    }
  })

  return (
    <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-5">
      <h3 className="text-sm font-semibold text-gray-900 dark:text-white mb-1">{t('snapshots.snapshots')}</h3>
      <p className="text-xs text-gray-500 dark:text-gray-400 mb-4">
        Capture the bucket's state, diff against it, and roll back to it — git-style history for your data.
      </p>

      {!versioningEnabled ? (
        <div className="p-3 rounded-lg bg-amber-50 dark:bg-amber-900/20 text-amber-700 dark:text-amber-400 text-sm">
          {t('snapshots.needsVersioning')}
        </div>
      ) : (
        <>
          <div className="flex gap-2 mb-4">
            <input
              type="text" placeholder={t('snapshots.snapshotMessageEGBeforeEtl')} value={message}
              onChange={e => setMessage(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleCreate()}
              className="flex-1 px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-900 text-gray-900 dark:text-white text-sm"
            />
            <button onClick={handleCreate} disabled={busy}
              className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium">
              {t('snapshots.takeSnapshot')}
            </button>
          </div>

          {snaps.length === 0 ? (
            <p className="text-sm text-gray-400">{t('snapshots.noSnapshotsYet')}</p>
          ) : (
            <div className="divide-y divide-gray-100 dark:divide-gray-700/50 border border-gray-100 dark:border-gray-700/50 rounded-lg">
              {snaps.map(s => (
                <div key={s.id} className="p-3">
                  <div className="flex items-center justify-between gap-3">
                    <div className="min-w-0">
                      <div className="text-sm text-gray-900 dark:text-white truncate">
                        {s.message || <span className="text-gray-400 italic">{t('snapshots.noMessage')}</span>}
                      </div>
                      <div className="text-xs text-gray-400 mt-0.5 font-mono">
                        {s.id} · {t('snapshots.objectCount', { count: s.objects })} · {formatSize(s.size)} · {new Date(s.createdAt * 1000).toLocaleString()}
                      </div>
                    </div>
                    <div className="flex gap-1 flex-shrink-0">
                      <button onClick={() => handleDiff(s.id)}
                        className="px-2.5 py-1 rounded-md text-xs border border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-700">
                        {diffFor === s.id ? t('snapshots.hideDiff') : t('snapshots.diff')}
                      </button>
                      <button onClick={() => handleRestore(s.id)} disabled={busy}
                        className="px-2.5 py-1 rounded-md text-xs bg-emerald-600 hover:bg-emerald-700 disabled:opacity-50 text-white">
                        {t('snapshots.restore')}
                      </button>
                      <button onClick={() => handleDelete(s.id)} disabled={busy}
                        className="px-2.5 py-1 rounded-md text-xs text-red-600 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-900/20 disabled:opacity-50">
                        {t('snapshots.delete')}
                      </button>
                    </div>
                  </div>

                  {diffFor === s.id && diff && (
                    <div className="mt-3 p-3 rounded-lg bg-gray-50 dark:bg-gray-900/40 text-xs">
                      <div className="mb-2 text-gray-500 dark:text-gray-400">
                        {t('snapshots.changesSince')}
                        <span className="text-emerald-600 dark:text-emerald-400 ml-2">{t('snapshots.addedCount', { count: diff.added })}</span>
                        <span className="text-amber-600 dark:text-amber-400 ml-2">{t('snapshots.modifiedCount', { count: diff.modified })}</span>
                        <span className="text-red-600 dark:text-red-400 ml-2">{t('snapshots.removedCount', { count: diff.removed })}</span>
                      </div>
                      {(diff.changes || []).length === 0 ? (
                        <div className="text-gray-400">{t('snapshots.noChangesTheBucketMatchesThis')}</div>
                      ) : (
                        <ul className="space-y-0.5 max-h-48 overflow-y-auto font-mono">
                          {(diff.changes || []).map(c => (
                            <li key={c.key} className="flex items-center gap-2">
                              <span className={
                                c.kind === 'added' ? 'text-emerald-600 dark:text-emerald-400'
                                : c.kind === 'removed' ? 'text-red-600 dark:text-red-400'
                                : 'text-amber-600 dark:text-amber-400'}>
                                {c.kind === 'added' ? '+' : c.kind === 'removed' ? '−' : '~'}
                              </span>
                              <span className="text-gray-700 dark:text-gray-300 truncate">{c.key}</span>
                            </li>
                          ))}
                        </ul>
                      )}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  )
}
