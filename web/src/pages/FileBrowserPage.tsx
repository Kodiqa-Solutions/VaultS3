import { useState, useEffect, useCallback, useMemo } from 'react'
import { useParams, useSearchParams, Link } from 'react-router-dom'
import { listObjects, deleteObject, bulkDeleteObjects, getDownloadUrl, getDownloadZipUrl, type ObjectItem } from '../api/objects'
import { getBucketVersioning } from '../api/buckets'
import { listVersions, getVersionTags, createVersionTag, deleteVersionTag, rollbackVersion, type Version, type VersionTag } from '../api/versions'
import UploadDropzone from '../components/UploadDropzone'
import CopyButton from '../components/CopyButton'
import VersionDiffViewer from '../components/VersionDiffViewer'
import FileTypeIcon from '../components/FileTypeIcon'
import FileGridView from '../components/FileGridView'
import { useToast } from '../hooks/useToast'

type SortField = 'name' | 'size' | 'type' | 'modified'
type SortDir = 'asc' | 'desc'
type ViewMode = 'table' | 'grid'

const PAGE_SIZE = 50
const FETCH_SIZE = 1000 // objects pulled from the server per request
const VIEW_MODE_KEY = 'vaults3:fileBrowserViewMode'

export default function FileBrowserPage() {
  const { name: bucket } = useParams<{ name: string }>()
  const [searchParams, setSearchParams] = useSearchParams()
  const prefix = searchParams.get('prefix') || ''

  const [objects, setObjects] = useState<ObjectItem[]>([])
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [truncated, setTruncated] = useState(false)
  const [nextCursor, setNextCursor] = useState('')
  const [error, setError] = useState('')
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)
  const { addToast } = useToast()

  // Sort state
  const [sortField, setSortField] = useState<SortField>('name')
  const [sortDir, setSortDir] = useState<SortDir>('asc')

  // Pagination
  const [page, setPage] = useState(0)

  // View mode (table / grid), persisted; defaults to table
  const [viewMode, setViewMode] = useState<ViewMode>(() => {
    const saved = localStorage.getItem(VIEW_MODE_KEY)
    return saved === 'grid' ? 'grid' : 'table'
  })

  const setView = (mode: ViewMode) => {
    setViewMode(mode)
    localStorage.setItem(VIEW_MODE_KEY, mode)
  }

  // Preview / metadata panel
  const [selectedFile, setSelectedFile] = useState<ObjectItem | null>(null)
  const [previewContent, setPreviewContent] = useState<string | null>(null)
  const [previewLoading, setPreviewLoading] = useState(false)

  // Multi-select
  const [selectedKeys, setSelectedKeys] = useState<Set<string>>(new Set())
  const [bulkDeleting, setBulkDeleting] = useState(false)
  const [showBulkDeleteModal, setShowBulkDeleteModal] = useState(false)

  // Versioning
  const [versioningEnabled, setVersioningEnabled] = useState(false)
  const [sideTab, setSideTab] = useState<'info' | 'versions'>('info')
  const [versions, setVersions] = useState<Version[]>([])
  const [versionsLoading, setVersionsLoading] = useState(false)
  const [versionTags, setVersionTags] = useState<VersionTag[]>([])
  const [newTagVersion, setNewTagVersion] = useState<string | null>(null)
  const [newTagName, setNewTagName] = useState('')
  const [rollbackTarget, setRollbackTarget] = useState<string | null>(null)
  const [diffVersions, setDiffVersions] = useState<[string, string] | null>(null)

  // Check if versioning is enabled on this bucket
  useEffect(() => {
    if (!bucket) return
    getBucketVersioning(bucket)
      .then(v => setVersioningEnabled(v.versioning === 'Enabled'))
      .catch(() => setVersioningEnabled(false))
  }, [bucket])

  // Load versions when a file is selected and versions tab is active
  useEffect(() => {
    if (!bucket || !selectedFile || selectedFile.isPrefix || sideTab !== 'versions') return
    setVersionsLoading(true)
    Promise.all([
      listVersions(bucket, selectedFile.key),
      getVersionTags(bucket, selectedFile.key),
    ])
      .then(([v, t]) => { setVersions(v); setVersionTags(t) })
      .catch(() => { setVersions([]); setVersionTags([]) })
      .finally(() => setVersionsLoading(false))
  }, [bucket, selectedFile, sideTab])

  const handleRollback = async (versionId: string) => {
    if (!bucket || !selectedFile) return
    setError('')
    try {
      await rollbackVersion(bucket, selectedFile.key, versionId)
      setRollbackTarget(null)
      addToast('success', 'Version rolled back')
      fetchObjects()
      // Refresh versions
      const [v, t] = await Promise.all([
        listVersions(bucket, selectedFile.key),
        getVersionTags(bucket, selectedFile.key),
      ])
      setVersions(v)
      setVersionTags(t)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Rollback failed')
    }
  }

  const handleAddTag = async (versionId: string) => {
    if (!bucket || !selectedFile || !newTagName.trim()) return
    try {
      await createVersionTag(bucket, selectedFile.key, versionId, newTagName.trim())
      setNewTagVersion(null)
      setNewTagName('')
      const t = await getVersionTags(bucket, selectedFile.key)
      setVersionTags(t)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to add tag')
    }
  }

  const handleDeleteTag = async (tagName: string) => {
    if (!bucket || !selectedFile) return
    try {
      await deleteVersionTag(bucket, selectedFile.key, tagName)
      const t = await getVersionTags(bucket, selectedFile.key)
      setVersionTags(t)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to delete tag')
    }
  }

  const fetchObjects = useCallback(async () => {
    if (!bucket) return
    setLoading(true)
    setError('')
    try {
      const data = await listObjects(bucket, prefix, FETCH_SIZE)
      setObjects(data.objects || [])
      setTruncated(data.truncated)
      setNextCursor(data.nextStartAfter || '')
      setPage(0)
      setSelectedKeys(new Set())
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to list objects')
    } finally {
      setLoading(false)
    }
  }, [bucket, prefix])

  // Pull the next page from the server and append it, de-duplicating folder
  // roll-ups that can recur when a folder's objects span a page boundary.
  const loadMore = useCallback(async () => {
    if (!bucket || !truncated || loadingMore) return
    setLoadingMore(true)
    setError('')
    try {
      const data = await listObjects(bucket, prefix, FETCH_SIZE, nextCursor)
      setObjects(prev => {
        const seen = new Set(prev.map(o => o.key))
        return [...prev, ...(data.objects || []).filter(o => !seen.has(o.key))]
      })
      setTruncated(data.truncated)
      setNextCursor(data.nextStartAfter || '')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load more objects')
    } finally {
      setLoadingMore(false)
    }
  }, [bucket, prefix, truncated, nextCursor, loadingMore])

  useEffect(() => { fetchObjects() }, [fetchObjects])

  // Reset selection when navigating
  useEffect(() => { setSelectedFile(null); setPreviewContent(null); setSelectedKeys(new Set()) }, [prefix])

  const handleDelete = async (key: string) => {
    if (!bucket) return
    setError('')
    try {
      await deleteObject(bucket, key)
      setDeleteTarget(null)
      if (selectedFile?.key === key) { setSelectedFile(null); setPreviewContent(null) }
      addToast('success', 'Object deleted')
      fetchObjects()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : 'Failed to delete object')
    }
  }

  const handleBulkDelete = async () => {
    if (!bucket || selectedKeys.size === 0) return
    setBulkDeleting(true)
    setError('')
    try {
      const count = selectedKeys.size
      await bulkDeleteObjects(bucket, Array.from(selectedKeys))
      setShowBulkDeleteModal(false)
      setSelectedKeys(new Set())
      if (selectedFile && selectedKeys.has(selectedFile.key)) {
        setSelectedFile(null)
        setPreviewContent(null)
      }
      addToast('success', `${count} object${count !== 1 ? 's' : ''} deleted`)
      fetchObjects()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : 'Bulk delete failed')
    } finally {
      setBulkDeleting(false)
    }
  }

  const navigatePrefix = (p: string) => {
    if (p) {
      setSearchParams({ prefix: p })
    } else {
      setSearchParams({})
    }
  }

  // Sort logic
  const handleSort = (field: SortField) => {
    if (sortField === field) {
      setSortDir(d => d === 'asc' ? 'desc' : 'asc')
    } else {
      setSortField(field)
      setSortDir('asc')
    }
  }

  const sortedObjects = useMemo(() => {
    const folders = objects.filter(o => o.isPrefix)
    const files = objects.filter(o => !o.isPrefix)

    files.sort((a, b) => {
      let cmp = 0
      switch (sortField) {
        case 'name': cmp = a.key.localeCompare(b.key); break
        case 'size': cmp = a.size - b.size; break
        case 'type': cmp = (a.contentType || '').localeCompare(b.contentType || ''); break
        case 'modified': cmp = (a.lastModified || '').localeCompare(b.lastModified || ''); break
      }
      return sortDir === 'asc' ? cmp : -cmp
    })

    return [...folders, ...files]
  }, [objects, sortField, sortDir])

  // Pagination
  const totalPages = Math.ceil(sortedObjects.length / PAGE_SIZE)
  const pagedObjects = sortedObjects.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE)

  // Select all logic (files only, not folders)
  const selectableFiles = sortedObjects.filter(o => !o.isPrefix)
  const allSelected = selectableFiles.length > 0 && selectableFiles.every(o => selectedKeys.has(o.key))

  const toggleSelectAll = () => {
    if (allSelected) {
      setSelectedKeys(new Set())
    } else {
      setSelectedKeys(new Set(selectableFiles.map(o => o.key)))
    }
  }

  const toggleSelect = (key: string) => {
    setSelectedKeys(prev => {
      const next = new Set(prev)
      if (next.has(key)) {
        next.delete(key)
      } else {
        next.add(key)
      }
      return next
    })
  }

  // Preview logic
  const handleSelectFile = async (obj: ObjectItem) => {
    if (obj.isPrefix) return
    setSelectedFile(obj)
    setPreviewContent(null)

    const ct = obj.contentType || ''
    const ext = obj.key.split('.').pop()?.toLowerCase() || ''

    // Image preview — use download URL directly
    if (ct.startsWith('image/')) {
      setPreviewContent('__image__')
      return
    }

    // Text-based preview
    const textTypes = ['text/', 'application/json', 'application/xml', 'application/javascript', 'application/yaml', 'application/x-yaml']
    const textExts = ['txt', 'md', 'json', 'xml', 'yaml', 'yml', 'csv', 'log', 'js', 'ts', 'tsx', 'jsx', 'html', 'css', 'go', 'py', 'sh', 'toml', 'ini', 'cfg', 'env', 'sql']
    const isText = textTypes.some(t => ct.startsWith(t)) || textExts.includes(ext)

    if (isText && obj.size < 512 * 1024) {
      setPreviewLoading(true)
      try {
        const url = getDownloadUrl(bucket!, obj.key)
        const resp = await fetch(url)
        if (resp.ok) {
          const text = await resp.text()
          setPreviewContent(text)
        }
      } catch {
        setPreviewContent(null)
      } finally {
        setPreviewLoading(false)
      }
      return
    }

    // No preview available — just show metadata
  }

  // Breadcrumbs
  const breadcrumbs = prefix
    ? prefix.split('/').filter(Boolean).map((seg, i, arr) => ({
        label: seg,
        prefix: arr.slice(0, i + 1).join('/') + '/',
      }))
    : []

  if (!bucket) return null

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

  return (
    <div className="flex gap-4">
      {/* Main content */}
      <div className={`flex-1 min-w-0 ${selectedFile ? 'max-w-[calc(100%-320px)]' : ''}`}>
        <div className="mb-4">
          <nav className="flex items-center gap-1 text-sm mb-2 flex-wrap">
            <Link
              to={`/buckets`}
              className="text-gray-400 dark:text-gray-500 hover:text-indigo-600 dark:hover:text-indigo-400 transition-colors"
            >
              <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M2.25 12l8.954-8.955c.44-.439 1.152-.439 1.591 0L21.75 12M4.5 9.75v10.125c0 .621.504 1.125 1.125 1.125H9.75v-4.875c0-.621.504-1.125 1.125-1.125h2.25c.621 0 1.125.504 1.125 1.125V21h4.125c.621 0 1.125-.504 1.125-1.125V9.75M8.25 21h8.25" />
              </svg>
            </Link>
            <ChevronRight />
            <Link
              to={`/buckets/${bucket}`}
              className="text-gray-500 dark:text-gray-400 hover:text-indigo-600 dark:hover:text-indigo-400 font-medium transition-colors"
            >
              {bucket}
            </Link>
            <ChevronRight />
            <button
              onClick={() => navigatePrefix('')}
              className={`font-medium transition-colors ${
                breadcrumbs.length === 0
                  ? 'text-gray-900 dark:text-white'
                  : 'text-gray-500 dark:text-gray-400 hover:text-indigo-600 dark:hover:text-indigo-400'
              }`}
            >
              files
            </button>
            {breadcrumbs.map((bc, i) => (
              <span key={bc.prefix} className="flex items-center gap-1">
                <ChevronRight />
                <button
                  onClick={() => navigatePrefix(bc.prefix)}
                  className={`font-medium transition-colors ${
                    i === breadcrumbs.length - 1
                      ? 'text-gray-900 dark:text-white'
                      : 'text-gray-500 dark:text-gray-400 hover:text-indigo-600 dark:hover:text-indigo-400'
                  }`}
                >
                  {bc.label}
                </button>
              </span>
            ))}
          </nav>
          <h2 className="text-xl font-semibold text-gray-900 dark:text-white">Files</h2>
        </div>

        <div className="flex items-center justify-end mb-3">
          <div className="inline-flex rounded-lg border border-gray-200 dark:border-gray-700 overflow-hidden">
            <button
              onClick={() => setView('table')}
              title="List view"
              aria-pressed={viewMode === 'table'}
              className={`flex items-center gap-1.5 px-2.5 py-1.5 text-xs font-medium transition-colors ${
                viewMode === 'table'
                  ? 'bg-indigo-600 text-white'
                  : 'bg-white dark:bg-gray-800 text-gray-500 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-gray-700'
              }`}
            >
              <svg className="w-3.5 h-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M3.75 6.75h16.5M3.75 12h16.5M3.75 17.25h16.5" />
              </svg>
              List
            </button>
            <button
              onClick={() => setView('grid')}
              title="Grid view"
              aria-pressed={viewMode === 'grid'}
              className={`flex items-center gap-1.5 px-2.5 py-1.5 text-xs font-medium border-l border-gray-200 dark:border-gray-700 transition-colors ${
                viewMode === 'grid'
                  ? 'bg-indigo-600 text-white'
                  : 'bg-white dark:bg-gray-800 text-gray-500 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-gray-700'
              }`}
            >
              <svg className="w-3.5 h-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M3.75 6a2.25 2.25 0 012.25-2.25h.75a2.25 2.25 0 012.25 2.25v.75a2.25 2.25 0 01-2.25 2.25h-.75A2.25 2.25 0 013.75 6.75V6zM3.75 15a2.25 2.25 0 012.25-2.25h.75a2.25 2.25 0 012.25 2.25v.75a2.25 2.25 0 01-2.25 2.25h-.75A2.25 2.25 0 013.75 17.25V15zM13.5 6a2.25 2.25 0 012.25-2.25h.75A2.25 2.25 0 0118.75 6v.75a2.25 2.25 0 01-2.25 2.25h-.75a2.25 2.25 0 01-2.25-2.25V6zM13.5 15a2.25 2.25 0 012.25-2.25h.75a2.25 2.25 0 012.25 2.25v.75a2.25 2.25 0 01-2.25 2.25h-.75a2.25 2.25 0 01-2.25-2.25V15z" />
              </svg>
              Grid
            </button>
          </div>
        </div>

        <div className="mb-4">
          <UploadDropzone bucket={bucket} prefix={prefix} onUploaded={() => fetchObjects()} />
        </div>

        {/* Bulk action bar */}
        {selectedKeys.size > 0 && (
          <div className="mb-4 flex items-center gap-3 px-4 py-2.5 rounded-lg bg-indigo-50 dark:bg-indigo-900/20 border border-indigo-200 dark:border-indigo-800">
            <span className="text-sm font-medium text-indigo-700 dark:text-indigo-300">
              {selectedKeys.size} selected
            </span>
            <button
              onClick={() => setShowBulkDeleteModal(true)}
              className="px-3 py-1 rounded-lg bg-red-600 hover:bg-red-700 text-white text-xs font-medium transition-colors"
            >
              Delete Selected
            </button>
            <a
              href={getDownloadZipUrl(bucket, Array.from(selectedKeys))}
              className="px-3 py-1 rounded-lg bg-indigo-600 hover:bg-indigo-700 text-white text-xs font-medium transition-colors"
            >
              Download Zip
            </a>
            <button
              onClick={() => setSelectedKeys(new Set())}
              className="ml-auto text-xs text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200"
            >
              Clear
            </button>
          </div>
        )}

        {error && (
          <div className="mb-4 p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
            {error}
          </div>
        )}

        {/* Delete confirmation modal */}
        {deleteTarget && (
          <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
            <div className="bg-white dark:bg-gray-800 rounded-xl shadow-xl border border-gray-200 dark:border-gray-700 p-6 w-full max-w-sm mx-4">
              <h3 className="text-lg font-semibold text-gray-900 dark:text-white mb-2">Delete Object</h3>
              <p className="text-sm text-gray-600 dark:text-gray-400 mb-4">
                Are you sure you want to delete <strong className="break-all">{deleteTarget}</strong>?
              </p>
              <div className="flex gap-2 justify-end">
                <button
                  onClick={() => setDeleteTarget(null)}
                  className="px-4 py-2 rounded-lg text-sm text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
                >
                  Cancel
                </button>
                <button
                  onClick={() => handleDelete(deleteTarget)}
                  className="px-4 py-2 rounded-lg bg-red-600 hover:bg-red-700 text-white text-sm font-medium transition-colors"
                >
                  Delete
                </button>
              </div>
            </div>
          </div>
        )}

        {/* Bulk delete confirmation modal */}
        {showBulkDeleteModal && (
          <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
            <div className="bg-white dark:bg-gray-800 rounded-xl shadow-xl border border-gray-200 dark:border-gray-700 p-6 w-full max-w-sm mx-4">
              <h3 className="text-lg font-semibold text-gray-900 dark:text-white mb-2">Bulk Delete</h3>
              <p className="text-sm text-gray-600 dark:text-gray-400 mb-4">
                Are you sure you want to delete <strong>{selectedKeys.size}</strong> object{selectedKeys.size !== 1 ? 's' : ''}?
              </p>
              <div className="flex gap-2 justify-end">
                <button
                  onClick={() => setShowBulkDeleteModal(false)}
                  disabled={bulkDeleting}
                  className="px-4 py-2 rounded-lg text-sm text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors disabled:opacity-50"
                >
                  Cancel
                </button>
                <button
                  onClick={handleBulkDelete}
                  disabled={bulkDeleting}
                  className="px-4 py-2 rounded-lg bg-red-600 hover:bg-red-700 text-white text-sm font-medium transition-colors disabled:opacity-50"
                >
                  {bulkDeleting ? 'Deleting...' : `Delete ${selectedKeys.size} objects`}
                </button>
              </div>
            </div>
          </div>
        )}

        {loading ? (
          <div className="flex items-center justify-center h-64">
            <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-indigo-600" />
          </div>
        ) : objects.length === 0 ? (
          <div className="text-center py-16 bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700">
            <svg className="w-12 h-12 mx-auto text-gray-400 dark:text-gray-500 mb-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={1.5}>
              <path strokeLinecap="round" strokeLinejoin="round" d="M2.25 12.75V12A2.25 2.25 0 014.5 9.75h15A2.25 2.25 0 0121.75 12v.75m-8.69-6.44l-2.12-2.12a1.5 1.5 0 00-1.061-.44H4.5A2.25 2.25 0 002.25 6v12a2.25 2.25 0 002.25 2.25h15A2.25 2.25 0 0021.75 18V9a2.25 2.25 0 00-2.25-2.25h-5.379a1.5 1.5 0 01-1.06-.44z" />
            </svg>
            <p className="text-gray-500 dark:text-gray-400 text-sm">No files here yet</p>
            <p className="text-gray-400 dark:text-gray-500 text-xs mt-1">Upload files using the dropzone above</p>
          </div>
        ) : (
          <>
            {viewMode === 'table' ? (
            <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-gray-200 dark:border-gray-700">
                    <th className="w-10 px-3 py-3">
                      <input
                        type="checkbox"
                        checked={allSelected}
                        onChange={toggleSelectAll}
                        className="rounded border-gray-300 dark:border-gray-600 text-indigo-600 focus:ring-indigo-500"
                      />
                    </th>
                    <SortHeader field="name" label="Name" />
                    <SortHeader field="size" label="Size" />
                    <SortHeader field="type" label="Type" />
                    <SortHeader field="modified" label="Modified" />
                    <th className="text-right px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">Actions</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-gray-100 dark:divide-gray-700/50">
                  {pagedObjects.map((obj) => (
                    <tr
                      key={obj.key}
                      className={`hover:bg-gray-50 dark:hover:bg-gray-700/30 transition-colors cursor-pointer ${
                        selectedFile?.key === obj.key ? 'bg-indigo-50 dark:bg-indigo-900/20' : ''
                      } ${selectedKeys.has(obj.key) ? 'bg-indigo-50/50 dark:bg-indigo-900/10' : ''}`}
                      onClick={() => obj.isPrefix ? navigatePrefix(obj.key) : handleSelectFile(obj)}
                    >
                      <td className="w-10 px-3 py-3" onClick={e => e.stopPropagation()}>
                        {!obj.isPrefix && (
                          <input
                            type="checkbox"
                            checked={selectedKeys.has(obj.key)}
                            onChange={() => toggleSelect(obj.key)}
                            className="rounded border-gray-300 dark:border-gray-600 text-indigo-600 focus:ring-indigo-500"
                          />
                        )}
                      </td>
                      <td className="px-4 py-3">
                        {obj.isPrefix ? (
                          <span className="flex items-center gap-2 text-indigo-600 dark:text-indigo-400 font-medium">
                            <FileTypeIcon name={displayName(obj.key, prefix)} isFolder />
                            {displayName(obj.key, prefix)}
                          </span>
                        ) : (
                          <span className="flex items-center gap-2 text-gray-900 dark:text-white">
                            <FileTypeIcon name={displayName(obj.key, prefix)} />
                            {displayName(obj.key, prefix)}
                          </span>
                        )}
                      </td>
                      <td className="px-4 py-3 text-gray-700 dark:text-gray-300">
                        {obj.isPrefix ? '-' : formatSize(obj.size)}
                      </td>
                      <td className="px-4 py-3 text-gray-500 dark:text-gray-400">
                        {obj.isPrefix ? 'Folder' : (obj.contentType || '-')}
                      </td>
                      <td className="px-4 py-3 text-gray-500 dark:text-gray-400">
                        {obj.isPrefix ? '-' : formatDate(obj.lastModified)}
                      </td>
                      <td className="px-4 py-3 text-right" onClick={e => e.stopPropagation()}>
                        {!obj.isPrefix && (
                          <div className="flex items-center justify-end gap-2">
                            <CopyButton text={`s3://${bucket}/${obj.key}`} />
                            <a
                              href={getDownloadUrl(bucket, obj.key)}
                              className="text-gray-400 hover:text-indigo-600 dark:hover:text-indigo-400 transition-colors"
                              title="Download"
                            >
                              <DownloadIcon />
                            </a>
                            <button
                              onClick={() => setDeleteTarget(obj.key)}
                              className="text-gray-400 hover:text-red-600 dark:hover:text-red-400 transition-colors"
                              title="Delete"
                            >
                              <TrashIcon />
                            </button>
                          </div>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            ) : (
              <FileGridView
                objects={pagedObjects}
                prefix={prefix}
                bucket={bucket!}
                selectedFileKey={selectedFile?.key ?? null}
                selectedKeys={selectedKeys}
                onNavigate={navigatePrefix}
                onSelectFile={handleSelectFile}
                onToggleSelect={toggleSelect}
                onDeleteRequest={setDeleteTarget}
                getDownloadUrl={getDownloadUrl}
                displayName={displayName}
                formatSize={formatSize}
              />
            )}

            {/* Pagination */}
            {(totalPages > 1 || truncated) && (
              <div className="flex items-center justify-between mt-3 text-sm text-gray-500 dark:text-gray-400">
                <span>
                  {sortedObjects.length}{truncated ? '+' : ''} items
                  {totalPages > 1 && <> &middot; Page {page + 1} of {totalPages}</>}
                </span>
                <div className="flex gap-1">
                  {truncated && (
                    <button
                      onClick={loadMore}
                      disabled={loadingMore}
                      className="px-3 py-1.5 rounded-lg border border-indigo-300 dark:border-indigo-700 text-indigo-600 dark:text-indigo-300 hover:bg-indigo-50 dark:hover:bg-indigo-900/30 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
                    >
                      {loadingMore ? 'Loading…' : `Load ${FETCH_SIZE} more`}
                    </button>
                  )}
                  <button
                    onClick={() => setPage(p => Math.max(0, p - 1))}
                    disabled={page === 0}
                    className="px-3 py-1.5 rounded-lg border border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-700 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
                  >
                    Prev
                  </button>
                  <button
                    onClick={() => setPage(p => Math.min(totalPages - 1, p + 1))}
                    disabled={page >= totalPages - 1}
                    className="px-3 py-1.5 rounded-lg border border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-700 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
                  >
                    Next
                  </button>
                </div>
              </div>
            )}
          </>
        )}
      </div>

      {/* Diff viewer modal */}
      {diffVersions && selectedFile && (
        <VersionDiffViewer
          bucket={bucket}
          objectKey={selectedFile.key}
          v1={diffVersions[0]}
          v2={diffVersions[1]}
          onClose={() => setDiffVersions(null)}
        />
      )}

      {/* Rollback confirmation modal */}
      {rollbackTarget && selectedFile && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
          <div className="bg-white dark:bg-gray-800 rounded-xl shadow-xl border border-gray-200 dark:border-gray-700 p-6 w-full max-w-sm mx-4">
            <h3 className="text-lg font-semibold text-gray-900 dark:text-white mb-2">Rollback Version</h3>
            <p className="text-sm text-gray-600 dark:text-gray-400 mb-4">
              Restore <strong className="break-all">{displayName(selectedFile.key, prefix)}</strong> to version <code className="text-xs bg-gray-100 dark:bg-gray-700 px-1 rounded">{rollbackTarget.slice(0, 16)}...</code>?
            </p>
            <div className="flex gap-2 justify-end">
              <button
                onClick={() => setRollbackTarget(null)}
                className="px-4 py-2 rounded-lg text-sm text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={() => handleRollback(rollbackTarget)}
                className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 text-white text-sm font-medium transition-colors"
              >
                Rollback
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Side panel — file metadata & preview */}
      {selectedFile && (
        <div className="w-80 flex-shrink-0">
          <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4 sticky top-4">
            <div className="flex items-center justify-between mb-3">
              <h3 className="text-sm font-semibold text-gray-900 dark:text-white truncate">{displayName(selectedFile.key, prefix)}</h3>
              <button
                onClick={() => { setSelectedFile(null); setPreviewContent(null); setSideTab('info') }}
                className="text-gray-400 hover:text-gray-600 dark:hover:text-gray-300"
              >
                <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                  <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>

            {/* Tabs */}
            {versioningEnabled && (
              <div className="flex gap-1 mb-3 border-b border-gray-200 dark:border-gray-700">
                <button
                  onClick={() => setSideTab('info')}
                  className={`px-3 py-1.5 text-xs font-medium border-b-2 transition-colors ${
                    sideTab === 'info'
                      ? 'border-indigo-600 text-indigo-600 dark:text-indigo-400'
                      : 'border-transparent text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-300'
                  }`}
                >
                  Info
                </button>
                <button
                  onClick={() => setSideTab('versions')}
                  className={`px-3 py-1.5 text-xs font-medium border-b-2 transition-colors ${
                    sideTab === 'versions'
                      ? 'border-indigo-600 text-indigo-600 dark:text-indigo-400'
                      : 'border-transparent text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-300'
                  }`}
                >
                  Versions
                </button>
              </div>
            )}

            {/* Info tab */}
            {sideTab === 'info' && (
              <>
                <div className="space-y-2 text-xs mb-4">
                  <div className="flex justify-between items-center">
                    <span className="text-gray-500 dark:text-gray-400">Key</span>
                    <span className="flex items-center gap-1">
                      <span className="text-gray-900 dark:text-white font-mono truncate max-w-[150px]" title={selectedFile.key}>{selectedFile.key}</span>
                      <CopyButton text={selectedFile.key} />
                    </span>
                  </div>
                  <div className="flex justify-between items-center">
                    <span className="text-gray-500 dark:text-gray-400">S3 URI</span>
                    <span className="flex items-center gap-1">
                      <span className="text-gray-900 dark:text-white font-mono truncate max-w-[150px]" title={`s3://${bucket}/${selectedFile.key}`}>s3://{bucket}/...</span>
                      <CopyButton text={`s3://${bucket}/${selectedFile.key}`} />
                    </span>
                  </div>
                  <MetaRow label="Size" value={formatSize(selectedFile.size)} />
                  <MetaRow label="Type" value={selectedFile.contentType || '-'} />
                  <MetaRow label="Modified" value={selectedFile.lastModified ? new Date(selectedFile.lastModified).toLocaleString() : '-'} />
                </div>

                <div className="flex gap-2 mb-4">
                  <a
                    href={getDownloadUrl(bucket, selectedFile.key)}
                    className="flex-1 text-center px-3 py-1.5 rounded-lg bg-indigo-600 hover:bg-indigo-700 text-white text-xs font-medium transition-colors"
                  >
                    Download
                  </a>
                  <button
                    onClick={() => setDeleteTarget(selectedFile.key)}
                    className="px-3 py-1.5 rounded-lg border border-red-300 dark:border-red-700 text-red-600 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-900/20 text-xs font-medium transition-colors"
                  >
                    Delete
                  </button>
                </div>

                {previewLoading && (
                  <div className="flex justify-center py-8">
                    <div className="animate-spin rounded-full h-6 w-6 border-b-2 border-indigo-600" />
                  </div>
                )}

                {previewContent === '__image__' && (
                  <div className="border border-gray-200 dark:border-gray-700 rounded-lg overflow-hidden">
                    <img
                      src={getDownloadUrl(bucket, selectedFile.key)}
                      alt={selectedFile.key}
                      className="w-full h-auto max-h-64 object-contain bg-gray-100 dark:bg-gray-900"
                    />
                  </div>
                )}

                {previewContent && previewContent !== '__image__' && (
                  <div className="border border-gray-200 dark:border-gray-700 rounded-lg overflow-hidden">
                    <div className="px-3 py-1.5 bg-gray-50 dark:bg-gray-900 border-b border-gray-200 dark:border-gray-700 text-xs text-gray-500 dark:text-gray-400">
                      Preview
                    </div>
                    <pre className="p-3 text-xs text-gray-800 dark:text-gray-200 overflow-auto max-h-80 whitespace-pre-wrap font-mono bg-white dark:bg-gray-800">
                      {previewContent.slice(0, 10000)}{previewContent.length > 10000 ? '\n\n... truncated ...' : ''}
                    </pre>
                  </div>
                )}

                {!previewLoading && previewContent === null && selectedFile && (
                  <div className="text-center py-6 text-xs text-gray-400 dark:text-gray-500">
                    No preview available
                  </div>
                )}
              </>
            )}

            {/* Versions tab */}
            {sideTab === 'versions' && (
              <div className="text-xs">
                {versionsLoading ? (
                  <div className="flex justify-center py-8">
                    <div className="animate-spin rounded-full h-6 w-6 border-b-2 border-indigo-600" />
                  </div>
                ) : versions.length === 0 ? (
                  <div className="text-center py-6 text-gray-400 dark:text-gray-500">
                    No versions found
                  </div>
                ) : (
                  <div className="space-y-2">
                    {versions.map((v, i) => {
                      const tagsForVersion = versionTags.filter(t => t.versionId === v.versionId)
                      return (
                        <div key={v.versionId} className="border border-gray-200 dark:border-gray-700 rounded-lg p-2.5">
                          <div className="flex items-center gap-1.5 mb-1">
                            <span className="font-mono text-gray-700 dark:text-gray-300 truncate" title={v.versionId}>
                              {v.versionId.slice(0, 16)}...
                            </span>
                            {v.isLatest && (
                              <span className="px-1.5 py-0.5 rounded bg-green-100 dark:bg-green-900/30 text-green-700 dark:text-green-400 text-[10px] font-medium">
                                Latest
                              </span>
                            )}
                            {v.deleteMarker && (
                              <span className="px-1.5 py-0.5 rounded bg-red-100 dark:bg-red-900/30 text-red-700 dark:text-red-400 text-[10px] font-medium">
                                Deleted
                              </span>
                            )}
                          </div>
                          <div className="text-gray-500 dark:text-gray-400 mb-1.5">
                            {formatSize(v.size)} &middot; {new Date(v.lastModified * 1000).toLocaleString()}
                          </div>

                          {/* Tags */}
                          {tagsForVersion.length > 0 && (
                            <div className="flex flex-wrap gap-1 mb-1.5">
                              {tagsForVersion.map(t => (
                                <span key={t.name} className="inline-flex items-center gap-0.5 px-1.5 py-0.5 rounded bg-indigo-100 dark:bg-indigo-900/30 text-indigo-700 dark:text-indigo-400 text-[10px]">
                                  {t.name}
                                  <button onClick={() => handleDeleteTag(t.name)} className="hover:text-red-500 ml-0.5">&times;</button>
                                </span>
                              ))}
                            </div>
                          )}

                          {/* Add tag form */}
                          {newTagVersion === v.versionId ? (
                            <div className="flex gap-1 mb-1.5">
                              <input
                                type="text"
                                value={newTagName}
                                onChange={e => setNewTagName(e.target.value)}
                                placeholder="Tag name..."
                                className="flex-1 px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-[11px] outline-none focus:ring-1 focus:ring-indigo-500"
                                onKeyDown={e => e.key === 'Enter' && handleAddTag(v.versionId)}
                                autoFocus
                              />
                              <button
                                onClick={() => handleAddTag(v.versionId)}
                                className="px-2 py-1 rounded bg-indigo-600 text-white text-[10px] font-medium hover:bg-indigo-700"
                              >
                                Add
                              </button>
                              <button
                                onClick={() => { setNewTagVersion(null); setNewTagName('') }}
                                className="px-1.5 py-1 text-gray-400 hover:text-gray-600 dark:hover:text-gray-300"
                              >
                                &times;
                              </button>
                            </div>
                          ) : null}

                          {/* Version actions */}
                          <div className="flex gap-1.5">
                            <button
                              onClick={() => { setNewTagVersion(v.versionId); setNewTagName('') }}
                              className="px-2 py-0.5 rounded text-[10px] text-gray-600 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700 border border-gray-200 dark:border-gray-600"
                              title="Add tag"
                            >
                              Tag
                            </button>
                            {i < versions.length - 1 && (
                              <button
                                onClick={() => setDiffVersions([v.versionId, versions[i + 1].versionId])}
                                className="px-2 py-0.5 rounded text-[10px] text-gray-600 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700 border border-gray-200 dark:border-gray-600"
                                title="Diff with next version"
                              >
                                Diff
                              </button>
                            )}
                            {!v.isLatest && !v.deleteMarker && (
                              <button
                                onClick={() => setRollbackTarget(v.versionId)}
                                className="px-2 py-0.5 rounded text-[10px] text-indigo-600 dark:text-indigo-400 hover:bg-indigo-50 dark:hover:bg-indigo-900/20 border border-indigo-200 dark:border-indigo-700"
                                title="Rollback to this version"
                              >
                                Rollback
                              </button>
                            )}
                          </div>
                        </div>
                      )
                    })}
                  </div>
                )}
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

function MetaRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between">
      <span className="text-gray-500 dark:text-gray-400">{label}</span>
      <span className="text-gray-900 dark:text-white font-mono truncate max-w-[180px]" title={value}>{value}</span>
    </div>
  )
}

function displayName(key: string, prefix: string): string {
  const rel = key.slice(prefix.length)
  return rel.endsWith('/') ? rel.slice(0, -1) : rel
}

function formatSize(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0)} ${units[i]}`
}

function formatDate(iso: string): string {
  if (!iso) return '-'
  return new Date(iso).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

function DownloadIcon() {
  return (
    <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
      <path strokeLinecap="round" strokeLinejoin="round" d="M3 16.5v2.25A2.25 2.25 0 005.25 21h13.5A2.25 2.25 0 0021 18.75V16.5M16.5 12L12 16.5m0 0L7.5 12m4.5 4.5V3" />
    </svg>
  )
}

function TrashIcon() {
  return (
    <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
      <path strokeLinecap="round" strokeLinejoin="round" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
    </svg>
  )
}

function ChevronRight() {
  return (
    <svg className="w-3.5 h-3.5 text-gray-400 dark:text-gray-500 flex-shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
      <path strokeLinecap="round" strokeLinejoin="round" d="M8.25 4.5l7.5 7.5-7.5 7.5" />
    </svg>
  )
}
