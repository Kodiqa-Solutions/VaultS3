import type { ObjectItem } from '../api/objects'
import FileTypeIcon from './FileTypeIcon'
import CopyButton from './CopyButton'

interface Props {
  objects: ObjectItem[]
  prefix: string
  bucket: string
  selectedFileKey: string | null
  selectedKeys: Set<string>
  onNavigate: (prefix: string) => void
  onSelectFile: (obj: ObjectItem) => void
  onToggleSelect: (key: string) => void
  onDeleteRequest: (key: string) => void
  getDownloadUrl: (bucket: string, key: string) => string
  displayName: (key: string, prefix: string) => string
  formatSize: (bytes: number) => string
}

export default function FileGridView({
  objects,
  prefix,
  bucket,
  selectedFileKey,
  selectedKeys,
  onNavigate,
  onSelectFile,
  onToggleSelect,
  onDeleteRequest,
  getDownloadUrl,
  displayName,
  formatSize,
}: Props) {
  return (
    <div className="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-3">
      {objects.map(obj => {
        const name = displayName(obj.key, prefix)
        const isSelected = selectedFileKey === obj.key
        const isChecked = selectedKeys.has(obj.key)

        return (
          <div
            key={obj.key}
            onClick={() => (obj.isPrefix ? onNavigate(obj.key) : onSelectFile(obj))}
            className={`group relative flex flex-col items-center gap-2.5 rounded-2xl border p-4 pt-5 cursor-pointer shadow-sm hover:shadow-md transition-all duration-200 hover:-translate-y-0.5 ${
              isSelected
                ? 'border-indigo-400 dark:border-indigo-500 bg-indigo-50 dark:bg-indigo-900/20'
                : isChecked
                ? 'border-indigo-200 dark:border-indigo-700 bg-indigo-50/50 dark:bg-indigo-900/10'
                : 'border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 hover:border-indigo-300 dark:hover:border-indigo-600 hover:bg-gray-50/80 dark:hover:bg-gray-700/40'
            }`}
            title={name}
          >
            {/* Selection checkbox (files only) */}
            {!obj.isPrefix && (
              <input
                type="checkbox"
                checked={isChecked}
                onClick={e => e.stopPropagation()}
                onChange={() => onToggleSelect(obj.key)}
                className="absolute top-2 left-2 rounded border-gray-300 dark:border-gray-600 text-indigo-600 focus:ring-indigo-500 opacity-0 group-hover:opacity-100 checked:opacity-100 transition-opacity"
              />
            )}

            {/* Hover actions (files only) */}
            {!obj.isPrefix && (
              <div
                className="absolute top-1.5 right-1.5 flex items-center gap-1 opacity-0 group-hover:opacity-100 transition-opacity"
                onClick={e => e.stopPropagation()}
              >
                <CopyButton text={`s3://${bucket}/${obj.key}`} />
                <a
                  href={getDownloadUrl(bucket, obj.key)}
                  className="text-gray-400 hover:text-indigo-600 dark:hover:text-indigo-400 transition-colors"
                  title="Download"
                >
                  <svg className="w-3.5 h-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                    <path strokeLinecap="round" strokeLinejoin="round" d="M3 16.5v2.25A2.25 2.25 0 005.25 21h13.5A2.25 2.25 0 0021 18.75V16.5M16.5 12L12 16.5m0 0L7.5 12m4.5 4.5V3" />
                  </svg>
                </a>
                <button
                  onClick={() => onDeleteRequest(obj.key)}
                  className="text-gray-400 hover:text-red-600 dark:hover:text-red-400 transition-colors"
                  title="Delete"
                >
                  <svg className="w-3.5 h-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                    <path strokeLinecap="round" strokeLinejoin="round" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
                  </svg>
                </button>
              </div>
            )}

            <FileTypeIcon name={name} isFolder={obj.isPrefix} className="w-12 h-12 mb-1" />

            <span
              className={`text-xs text-center break-all line-clamp-2 leading-tight ${
                obj.isPrefix ? 'text-indigo-600 dark:text-indigo-400 font-medium' : 'text-gray-700 dark:text-gray-200'
              }`}
            >
              {name}
            </span>

            {!obj.isPrefix && (
              <span className="text-[10px] text-gray-400 dark:text-gray-500">{formatSize(obj.size)}</span>
            )}
          </div>
        )
      })}
    </div>
  )
}
