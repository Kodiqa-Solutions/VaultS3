import type { TranslateFn } from '../i18n'
import type { BulkDeleteSummary } from '../api/objects'
import type { ToastType } from '../hooks/useToast'

// How many failed keys the toast names. Enough to see the pattern (one bad key
// or a whole folder with the same error) without a toast that fills the screen.
export const SHOWN_FAILURES = 3

// bulkDeleteMessage turns the server's per-key results into the toast. It used
// to report every requested key as deleted, so a run where some keys failed
// looked like a complete success.
export function bulkDeleteMessage(summary: BulkDeleteSummary, t: TranslateFn): { type: ToastType; text: string } {
  const { deleted, failed } = summary
  if (failed.length === 0) {
    return { type: 'success', text: t('files.bulkDeleted', { n: deleted }) }
  }
  const shown = failed
    .slice(0, SHOWN_FAILURES)
    .map(f => `${f.key}: ${f.error || t('files.bulkDeleteUnconfirmed')}`)
  if (failed.length > SHOWN_FAILURES) {
    shown.push(t('files.andNMore', { n: failed.length - SHOWN_FAILURES }))
  }
  const head = t('files.bulkDeletePartial', { deleted, total: deleted + failed.length, failed: failed.length })
  return { type: 'error', text: `${head} ${shown.join(', ')}` }
}
