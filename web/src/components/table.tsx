import { useState, useMemo, useCallback, type ReactNode } from 'react'
import { useI18n } from '../i18n'

// Shared list controls, so every table sorts, pages and says "nothing here" the
// same way (issue #63). Pages keep their own comparators: what a column means
// is page specific, how it is clicked is not.

export type SortDir = 'asc' | 'desc'

// firstDir picks the direction a column starts in when it is first clicked,
// for columns such as a timestamp where newest first is the useful order.
export function useSort<F extends string>(initialField: F, initialDir: SortDir = 'asc', firstDir?: (field: F) => SortDir) {
  const [field, setField] = useState<F>(initialField)
  const [dir, setDir] = useState<SortDir>(initialDir)
  const toggle = useCallback((next: F) => {
    if (next === field) {
      setDir(d => (d === 'asc' ? 'desc' : 'asc'))
    } else {
      setField(next)
      setDir(firstDir ? firstDir(next) : 'asc')
    }
  }, [field, firstDir])
  return { field, dir, toggle }
}

export function SortHeader<F extends string>({ field, label, sortField, sortDir, onSort }: {
  field: F
  label: string
  sortField: F
  sortDir: SortDir
  onSort: (field: F) => void
}) {
  const active = sortField === field
  return (
    <th
      aria-sort={active ? (sortDir === 'asc' ? 'ascending' : 'descending') : undefined}
      className="text-left px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider select-none"
    >
      <button
        type="button"
        onClick={() => onSort(field)}
        className="inline-flex items-center gap-1 uppercase tracking-wider hover:text-indigo-600 dark:hover:text-indigo-400 focus:outline-none focus-visible:ring-2 focus-visible:ring-indigo-500 rounded"
      >
        {label}
        {active && (
          <span className="text-indigo-600 dark:text-indigo-400">{sortDir === 'asc' ? '↑' : '↓'}</span>
        )}
      </button>
    </th>
  )
}

export const PAGE_SIZES = [25, 50, 100]

// Client side paging over a list the page already holds. The page is clamped,
// so a list that shrinks under it (a delete, a new filter) never leaves the
// reader on an empty page past the end.
export function pageWindow(total: number, page: number, pageSize: number) {
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  return { totalPages, current: Math.min(Math.max(0, page), totalPages - 1) }
}

export function usePaged<T>(items: T[], initialSize = 50) {
  const [page, setPage] = useState(0)
  const [pageSize, setSize] = useState(initialSize)
  const { totalPages, current } = pageWindow(items.length, page, pageSize)
  const paged = useMemo(() => items.slice(current * pageSize, (current + 1) * pageSize), [items, current, pageSize])
  const setPageSize = useCallback((n: number) => {
    setSize(n)
    setPage(0)
  }, [])
  return { page: current, setPage, pageSize, setPageSize, totalPages, total: items.length, paged }
}

export type Paging = Pick<ReturnType<typeof usePaged>, 'page' | 'setPage' | 'pageSize' | 'setPageSize' | 'totalPages' | 'total'>

// Hidden while everything fits on the smallest page, unless the page has an
// extra control to show (the file browser's "load more").
export function Pagination({ paging, more, extra }: { paging: Paging; more?: boolean; extra?: ReactNode }) {
  const { t } = useI18n()
  const { page, setPage, pageSize, setPageSize, totalPages, total } = paging
  if (total <= PAGE_SIZES[0] && !extra) return null
  const button = 'px-3 py-1.5 rounded-lg border border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-700 disabled:opacity-40 disabled:cursor-not-allowed transition-colors'
  return (
    <div className="flex flex-wrap items-center justify-between gap-2 mt-3 text-sm text-gray-500 dark:text-gray-400">
      <span>
        {t('table.itemCount', { n: `${total}${more ? '+' : ''}` })}
        {totalPages > 1 && <> &middot; {t('table.pageOf', { page: page + 1, total: totalPages })}</>}
      </span>
      <div className="flex flex-wrap items-center gap-1">
        {extra}
        <select
          value={pageSize}
          onChange={e => setPageSize(Number(e.target.value))}
          aria-label={t('table.pageSize')}
          className="min-w-[8.5rem] px-3 py-1.5 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-300"
        >
          {PAGE_SIZES.map(n => <option key={n} value={n}>{t('table.perPage', { n })}</option>)}
        </select>
        <button type="button" onClick={() => setPage(Math.max(0, page - 1))} disabled={page === 0} className={button}>
          {t('table.prev')}
        </button>
        <button type="button" onClick={() => setPage(Math.min(totalPages - 1, page + 1))} disabled={page >= totalPages - 1} className={button}>
          {t('table.next')}
        </button>
      </div>
    </div>
  )
}

// One empty state for every list. Pass a table's column count to render it as
// a row, or leave it out for a list that is not a table.
export function EmptyState({ title, hint, colSpan, children }: { title: string; hint?: string; colSpan?: number; children?: ReactNode }) {
  const body = (
    <div className="py-10 px-4 text-center">
      <p className="text-sm font-medium text-gray-600 dark:text-gray-300">{title}</p>
      {hint && <p className="mt-1 text-xs text-gray-400 dark:text-gray-500">{hint}</p>}
      {children && <div className="mt-3">{children}</div>}
    </div>
  )
  if (colSpan) {
    return <tr><td colSpan={colSpan}>{body}</td></tr>
  }
  return body
}
