import { useState, useEffect, useCallback, useMemo } from 'react'
import { listKeys, createKey, deleteKey, type AccessKey, type CreatedKey } from '../api/keys'
import { listBuckets, type Bucket } from '../api/buckets'
import { useToast } from '../hooks/useToast'

type SortField = 'accessKey' | 'createdAt' | 'type'
type SortDir = 'asc' | 'desc'

export default function AccessKeysPage() {
  const [keys, setKeys] = useState<AccessKey[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const [showCreateModal, setShowCreateModal] = useState(false)
  const [newUserId, setNewUserId] = useState('')
  const [buckets, setBuckets] = useState<Bucket[]>([])
  const [selectedBuckets, setSelectedBuckets] = useState<string[]>([])
  const [newKey, setNewKey] = useState<CreatedKey | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)
  const [copied, setCopied] = useState('')
  const { addToast } = useToast()

  // Sort state
  const [sortField, setSortField] = useState<SortField>('accessKey')
  const [sortDir, setSortDir] = useState<SortDir>('asc')

  const fetchKeys = useCallback(async () => {
    try {
      const data = await listKeys()
      setKeys(data)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load keys')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { fetchKeys() }, [fetchKeys])

  const openCreateModal = async () => {
    setNewUserId('')
    setSelectedBuckets([])
    setShowCreateModal(true)
    try {
      const b = await listBuckets()
      setBuckets(b)
    } catch {
      setBuckets([])
    }
  }

  const handleCreate = async () => {
    if (!newUserId.trim()) {
      addToast('error', 'User ID is required')
      return
    }
    setCreating(true)
    setError('')
    try {
      const key = await createKey(newUserId.trim(), selectedBuckets)
      setShowCreateModal(false)
      setNewKey(key)
      addToast('success', 'Access key created')
      fetchKeys()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : 'Failed to create key')
    } finally {
      setCreating(false)
    }
  }

  const toggleBucket = (name: string) => {
    setSelectedBuckets(prev =>
      prev.includes(name) ? prev.filter(b => b !== name) : [...prev, name]
    )
  }

  const handleDelete = async (accessKey: string) => {
    setError('')
    try {
      await deleteKey(accessKey)
      setDeleteTarget(null)
      addToast('success', 'Access key revoked')
      fetchKeys()
    } catch (err) {
      addToast('error', err instanceof Error ? err.message : 'Failed to delete key')
    }
  }

  const copyToClipboard = (text: string, label: string) => {
    navigator.clipboard.writeText(text)
    setCopied(label)
    setTimeout(() => setCopied(''), 2000)
  }

  const handleSort = (field: SortField) => {
    if (sortField === field) {
      setSortDir(d => d === 'asc' ? 'desc' : 'asc')
    } else {
      setSortField(field)
      setSortDir('asc')
    }
  }

  const sortedKeys = useMemo(() => {
    const sorted = [...keys]
    sorted.sort((a, b) => {
      let cmp = 0
      switch (sortField) {
        case 'accessKey': cmp = a.accessKey.localeCompare(b.accessKey); break
        case 'createdAt': cmp = (a.createdAt || '').localeCompare(b.createdAt || ''); break
        case 'type': cmp = (a.isAdmin ? 1 : 0) - (b.isAdmin ? 1 : 0); break
      }
      return sortDir === 'asc' ? cmp : -cmp
    })
    return sorted
  }, [keys, sortField, sortDir])

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
          <h2 className="text-xl font-semibold text-gray-900 dark:text-white">Access Keys</h2>
          <p className="text-sm text-gray-500 dark:text-gray-400 mt-0.5">Manage S3 API access keys</p>
        </div>
        <button
          onClick={openCreateModal}
          className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 text-white text-sm font-medium transition-colors"
        >
          Create Key
        </button>
      </div>

      {error && (
        <div className="mb-4 p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
          {error}
        </div>
      )}

      {/* Create key modal */}
      {showCreateModal && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
          <div className="bg-white dark:bg-gray-800 rounded-xl shadow-xl border border-gray-200 dark:border-gray-700 p-6 w-full max-w-md mx-4">
            <h3 className="text-lg font-semibold text-gray-900 dark:text-white mb-4">Create Access Key</h3>
            <div className="space-y-4">
              <div>
                <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">User ID <span className="text-red-500">*</span></label>
                <input
                  type="text"
                  value={newUserId}
                  onChange={e => setNewUserId(e.target.value)}
                  placeholder="e.g. app-user, backup-agent"
                  className="w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-900 dark:text-white text-sm focus:ring-2 focus:ring-indigo-500 focus:border-indigo-500 outline-none"
                  autoFocus
                  onKeyDown={e => { if (e.key === 'Enter' && newUserId.trim()) handleCreate() }}
                />
                <p className="mt-1 text-xs text-gray-500 dark:text-gray-400">An IAM user will be auto-created if it doesn't exist.</p>
              </div>
              {buckets.length > 0 && (
                <div>
                  <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">Bucket Access <span className="text-xs font-normal text-gray-500">(optional)</span></label>
                  <p className="text-xs text-gray-500 dark:text-gray-400 mb-2">Leave empty for full S3 access, or select specific buckets.</p>
                  <div className="max-h-40 overflow-y-auto space-y-1 border border-gray-200 dark:border-gray-600 rounded-lg p-2">
                    {buckets.map(b => (
                      <label key={b.name} className="flex items-center gap-2 px-2 py-1 rounded hover:bg-gray-50 dark:hover:bg-gray-700/50 cursor-pointer">
                        <input
                          type="checkbox"
                          checked={selectedBuckets.includes(b.name)}
                          onChange={() => toggleBucket(b.name)}
                          className="rounded border-gray-300 dark:border-gray-600 text-indigo-600 focus:ring-indigo-500"
                        />
                        <span className="text-sm text-gray-700 dark:text-gray-300">{b.name}</span>
                      </label>
                    ))}
                  </div>
                </div>
              )}
            </div>
            <div className="flex gap-2 justify-end mt-6">
              <button
                onClick={() => setShowCreateModal(false)}
                className="px-4 py-2 rounded-lg text-sm text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={handleCreate}
                disabled={creating || !newUserId.trim()}
                className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium transition-colors"
              >
                {creating ? 'Creating...' : 'Create Key'}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* New key modal */}
      {newKey && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
          <div className="bg-white dark:bg-gray-800 rounded-xl shadow-xl border border-gray-200 dark:border-gray-700 p-6 w-full max-w-md mx-4">
            <h3 className="text-lg font-semibold text-gray-900 dark:text-white mb-2">Key Created</h3>
            <p className="text-sm text-amber-600 dark:text-amber-400 mb-4">
              Save the secret key now. It will not be shown again.
            </p>
            <div className="space-y-3 mb-4">
              <div>
                <label className="text-xs text-gray-500 dark:text-gray-400 font-medium">Access Key</label>
                <div className="flex items-center gap-2 mt-1">
                  <code className="flex-1 px-3 py-2 rounded-lg bg-gray-100 dark:bg-gray-700 text-sm text-gray-900 dark:text-white font-mono break-all">
                    {newKey.accessKey}
                  </code>
                  <button
                    onClick={() => copyToClipboard(newKey.accessKey, 'access')}
                    className="px-2 py-2 text-xs text-gray-500 hover:text-gray-700 dark:hover:text-gray-300"
                  >
                    {copied === 'access' ? 'Copied!' : 'Copy'}
                  </button>
                </div>
              </div>
              <div>
                <label className="text-xs text-gray-500 dark:text-gray-400 font-medium">Secret Key</label>
                <div className="flex items-center gap-2 mt-1">
                  <code className="flex-1 px-3 py-2 rounded-lg bg-gray-100 dark:bg-gray-700 text-sm text-gray-900 dark:text-white font-mono break-all">
                    {newKey.secretKey}
                  </code>
                  <button
                    onClick={() => copyToClipboard(newKey.secretKey, 'secret')}
                    className="px-2 py-2 text-xs text-gray-500 hover:text-gray-700 dark:hover:text-gray-300"
                  >
                    {copied === 'secret' ? 'Copied!' : 'Copy'}
                  </button>
                </div>
              </div>
            </div>
            <div className="flex justify-end">
              <button
                onClick={() => setNewKey(null)}
                className="px-4 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-700 text-white text-sm font-medium transition-colors"
              >
                Done
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Delete confirmation modal */}
      {deleteTarget && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
          <div className="bg-white dark:bg-gray-800 rounded-xl shadow-xl border border-gray-200 dark:border-gray-700 p-6 w-full max-w-sm mx-4">
            <h3 className="text-lg font-semibold text-gray-900 dark:text-white mb-2">Delete Key</h3>
            <p className="text-sm text-gray-600 dark:text-gray-400 mb-4">
              Are you sure you want to revoke key <strong className="font-mono">{deleteTarget}</strong>?
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
                Revoke
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Keys table */}
      <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-gray-200 dark:border-gray-700">
              <SortHeader field="accessKey" label="Access Key" />
              <th className="text-left px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">Secret</th>
              <SortHeader field="createdAt" label="Created" />
              <SortHeader field="type" label="Type" />
              <th className="text-right px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">Actions</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-gray-100 dark:divide-gray-700/50">
            {sortedKeys.map((k) => (
              <tr key={k.accessKey} className="hover:bg-gray-50 dark:hover:bg-gray-700/30 transition-colors">
                <td className="px-4 py-3 font-mono text-gray-900 dark:text-white">{k.accessKey}</td>
                <td className="px-4 py-3 font-mono text-gray-500 dark:text-gray-400">{k.maskedSecret}</td>
                <td className="px-4 py-3 text-gray-500 dark:text-gray-400">
                  {k.createdAt ? new Date(k.createdAt).toLocaleDateString() : 'Built-in'}
                </td>
                <td className="px-4 py-3">
                  {k.isAdmin ? (
                    <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-amber-100 dark:bg-amber-900/30 text-amber-700 dark:text-amber-400">
                      Admin
                    </span>
                  ) : (
                    <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-400">
                      Standard
                    </span>
                  )}
                </td>
                <td className="px-4 py-3 text-right">
                  {!k.isAdmin && (
                    <button
                      onClick={() => setDeleteTarget(k.accessKey)}
                      className="text-gray-400 hover:text-red-600 dark:hover:text-red-400 transition-colors"
                      title="Revoke key"
                    >
                      <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                        <path strokeLinecap="round" strokeLinejoin="round" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
                      </svg>
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
