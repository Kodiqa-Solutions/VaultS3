import { useState, useCallback, useMemo, useEffect } from 'react'
import { useI18n } from '../i18n'
import { useNavigate } from 'react-router-dom'
import { searchObjects, type SearchResult } from '../api/search'
import { queryVectors, getVectorStatus, type VectorMatch } from '../api/vectors'

type SortField = 'bucket' | 'key' | 'size' | 'content_type' | 'last_modified'
type SortDir = 'asc' | 'desc'
type Mode = 'keyword' | 'semantic'

const PAGE_SIZE = 50

function formatSize(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

// Land in the folder that holds the hit, not at the bucket root.
function browseUrl(bucket: string, key: string): string {
  const dir = key.slice(0, key.lastIndexOf('/') + 1)
  const base = `/buckets/${encodeURIComponent(bucket)}/files`
  return dir ? `${base}?prefix=${encodeURIComponent(dir)}` : base
}

export default function SearchPage() {
  const { t } = useI18n()
  const navigate = useNavigate()
  const [mode, setMode] = useState<Mode>('keyword')
  const [vectorEnabled, setVectorEnabled] = useState(false)
  const [query, setQuery] = useState('')
  const [bucket, setBucket] = useState('')
  const [results, setResults] = useState<SearchResult[]>([])
  const [semanticResults, setSemanticResults] = useState<VectorMatch[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [searched, setSearched] = useState(false)

  // Sort state (keyword mode)
  const [sortField, setSortField] = useState<SortField>('key')
  const [sortDir, setSortDir] = useState<SortDir>('asc')

  // Pagination (keyword mode)
  const [page, setPage] = useState(0)

  useEffect(() => {
    getVectorStatus().then(s => setVectorEnabled(s.enabled)).catch(() => setVectorEnabled(false))
  }, [])

  const handleSearch = useCallback(async () => {
    if (!query.trim()) return
    setLoading(true)
    setError('')
    setSearched(true)
    try {
      if (mode === 'semantic') {
        const data = await queryVectors(query.trim(), 50, bucket || undefined)
        setSemanticResults(data.results || [])
      } else {
        const data = await searchObjects({ q: query.trim(), bucket: bucket || undefined, limit: 100 })
        setResults(data || [])
        setPage(0)
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : t('search.failed'))
    } finally {
      setLoading(false)
    }
  }, [query, bucket, mode])

  const handleSort = (field: SortField) => {
    if (sortField === field) {
      setSortDir(d => d === 'asc' ? 'desc' : 'asc')
    } else {
      setSortField(field)
      setSortDir('asc')
    }
  }

  const sortedResults = useMemo(() => {
    const sorted = [...results]
    sorted.sort((a, b) => {
      let cmp = 0
      switch (sortField) {
        case 'bucket': cmp = a.bucket.localeCompare(b.bucket); break
        case 'key': cmp = a.key.localeCompare(b.key); break
        case 'size': cmp = a.size - b.size; break
        case 'content_type': cmp = (a.content_type || '').localeCompare(b.content_type || ''); break
        case 'last_modified': cmp = (a.last_modified || 0) - (b.last_modified || 0); break
      }
      return sortDir === 'asc' ? cmp : -cmp
    })
    return sorted
  }, [results, sortField, sortDir])

  const totalPages = Math.ceil(sortedResults.length / PAGE_SIZE)
  const pagedResults = sortedResults.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE)

  const SortHeader = ({ field, label }: { field: SortField; label: string }) => (
    <th
      onClick={() => handleSort(field)}
      className="text-left px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider cursor-pointer hover:text-indigo-600 dark:hover:text-indigo-400 select-none"
    >
      <span className="inline-flex items-center gap-1">
        {label}
        {sortField === field && (
          <span className="text-indigo-600 dark:text-indigo-400">{sortDir === 'asc' ? '↑' : '↓'}</span>
        )}
      </span>
    </th>
  )

  const switchMode = (m: Mode) => {
    setMode(m)
    setSearched(false)
    setResults([])
    setSemanticResults([])
    setError('')
  }

  return (
    <div>
      <div className="mb-6">
        <h2 className="text-xl font-semibold text-gray-900 dark:text-white">{t('search.search')}</h2>
        <p className="text-sm text-gray-500 dark:text-gray-400 mt-0.5">
          {mode === 'semantic'
            ? t('search.semanticSubtitle')
            : t('search.keywordSubtitle')}
        </p>
      </div>

      {/* Mode toggle */}
      <div className="inline-flex rounded-lg border border-gray-300 dark:border-gray-600 p-0.5 mb-4 bg-gray-100 dark:bg-gray-800">
        <button
          onClick={() => switchMode('keyword')}
          className={`px-4 py-1.5 rounded-md text-sm font-medium transition-colors ${mode === 'keyword' ? 'bg-white dark:bg-gray-700 text-indigo-600 dark:text-indigo-400 shadow-sm' : 'text-gray-500 dark:text-gray-400'}`}
        >
          {t('search.keyword')}
        </button>
        <button
          onClick={() => switchMode('semantic')}
          className={`px-4 py-1.5 rounded-md text-sm font-medium transition-colors ${mode === 'semantic' ? 'bg-white dark:bg-gray-700 text-indigo-600 dark:text-indigo-400 shadow-sm' : 'text-gray-500 dark:text-gray-400'}`}
        >
          {t('search.semantic')} <span className="text-[10px] align-top text-emerald-500">{t('search.ai')}</span>
        </button>
      </div>

      {mode === 'semantic' && !vectorEnabled && (
        <div className="mb-4 p-3 rounded-lg bg-amber-50 dark:bg-amber-900/20 text-amber-700 dark:text-amber-400 text-sm">
          {t('search.semanticDisabled1')} <code className="font-mono">vector.enabled: true</code> {t('search.semanticDisabled2')}
        </div>
      )}

      {error && (
        <div className="mb-4 p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
          {error}
        </div>
      )}

      {/* Search bar */}
      <div className="flex gap-3 mb-6">
        <input type="text"
          placeholder={mode === 'semantic' ? t('search.semanticPlaceholder') : t('search.keywordPlaceholder')}
          value={query} onChange={e => setQuery(e.target.value)}
          onKeyDown={e => e.key === 'Enter' && handleSearch()}
          className="flex-1 px-4 py-2.5 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-900 dark:text-white text-sm" />
        <input type="text" placeholder={t('search.bucketOptional')} value={bucket}
          onChange={e => setBucket(e.target.value)}
          className="w-44 px-3 py-2.5 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-900 dark:text-white text-sm" />
        <button onClick={handleSearch} disabled={loading || !query.trim() || (mode === 'semantic' && !vectorEnabled)}
          className="px-5 py-2.5 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium transition-colors">
          {loading ? t('search.searching') : t('search.search')}
        </button>
      </div>

      {/* Semantic results */}
      {searched && mode === 'semantic' && (
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
          <div className="px-4 py-3 border-b border-gray-200 dark:border-gray-700">
            <span className="text-sm text-gray-500 dark:text-gray-400">{semanticResults.length} match{semanticResults.length !== 1 ? 'es' : ''} by similarity</span>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-gray-200 dark:border-gray-700">
                  <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">{t('search.bucket')}</th>
                  <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">{t('search.key')}</th>
                  <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">{t('search.similarity')}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-100 dark:divide-gray-700/50">
                {semanticResults.map((r, i) => (
                  <tr key={i}
                    onClick={() => navigate(browseUrl(r.bucket, r.key))}
                    className="hover:bg-gray-50 dark:hover:bg-gray-700/30 transition-colors cursor-pointer">
                    <td className="px-4 py-3 font-medium text-gray-900 dark:text-white">{r.bucket}</td>
                    <td className="px-4 py-3 text-gray-700 dark:text-gray-300 font-mono text-xs max-w-md truncate">{r.key}</td>
                    <td className="px-4 py-3">
                      <div className="flex items-center gap-2">
                        <div className="w-24 h-1.5 rounded-full bg-gray-200 dark:bg-gray-700 overflow-hidden">
                          <div className="h-full bg-emerald-500" style={{ width: `${Math.max(0, Math.min(100, r.score * 100))}%` }} />
                        </div>
                        <span className="text-xs text-gray-500 dark:text-gray-400 tabular-nums">{(r.score * 100).toFixed(1)}%</span>
                      </div>
                    </td>
                  </tr>
                ))}
                {semanticResults.length === 0 && (
                  <tr><td colSpan={3} className="px-4 py-8 text-center text-gray-400">{t('search.noMatchesFound')}</td></tr>
                )}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* Keyword results */}
      {searched && mode === 'keyword' && (
        <>
          <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
            <div className="px-4 py-3 border-b border-gray-200 dark:border-gray-700">
              <span className="text-sm text-gray-500 dark:text-gray-400">{results.length} result{results.length !== 1 ? 's' : ''}</span>
            </div>
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-gray-200 dark:border-gray-700">
                    <SortHeader field="bucket" label={t('search.bucket')} />
                    <SortHeader field="key" label={t('search.key')} />
                    <SortHeader field="size" label={t('search.size')} />
                    <SortHeader field="content_type" label={t('search.contentType')} />
                    <SortHeader field="last_modified" label={t('search.lastModified')} />
                  </tr>
                </thead>
                <tbody className="divide-y divide-gray-100 dark:divide-gray-700/50">
                  {pagedResults.map((r, i) => (
                    <tr key={i}
                      onClick={() => navigate(browseUrl(r.bucket, r.key))}
                      className="hover:bg-gray-50 dark:hover:bg-gray-700/30 transition-colors cursor-pointer">
                      <td className="px-4 py-3 font-medium text-gray-900 dark:text-white">{r.bucket}</td>
                      <td className="px-4 py-3 text-gray-700 dark:text-gray-300 font-mono text-xs max-w-xs truncate">{r.key}</td>
                      <td className="px-4 py-3 text-gray-500 dark:text-gray-400">{formatSize(r.size)}</td>
                      <td className="px-4 py-3 text-gray-500 dark:text-gray-400">{r.content_type || '-'}</td>
                      <td className="px-4 py-3 text-gray-500 dark:text-gray-400 whitespace-nowrap">
                        {r.last_modified ? new Date(r.last_modified * 1000).toLocaleString() : '-'}
                      </td>
                    </tr>
                  ))}
                  {results.length === 0 && (
                    <tr><td colSpan={5} className="px-4 py-8 text-center text-gray-400">{t('search.noResultsFound')}</td></tr>
                  )}
                </tbody>
              </table>
            </div>
          </div>

          {/* Pagination */}
          {totalPages > 1 && (
            <div className="flex items-center justify-between mt-3 text-sm text-gray-500 dark:text-gray-400">
              <span>
                {sortedResults.length} results &middot; Page {page + 1} of {totalPages}
              </span>
              <div className="flex gap-1">
                <button
                  onClick={() => setPage(p => Math.max(0, p - 1))}
                  disabled={page === 0}
                  className="px-3 py-1.5 rounded-lg border border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-700 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
                >
                  {t('search.prev')}
                </button>
                <button
                  onClick={() => setPage(p => Math.min(totalPages - 1, p + 1))}
                  disabled={page >= totalPages - 1}
                  className="px-3 py-1.5 rounded-lg border border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-700 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
                >
                  {t('search.next')}
                </button>
              </div>
            </div>
          )}
        </>
      )}
    </div>
  )
}
