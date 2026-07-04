import { useState, useRef, useCallback } from 'react'
import { uploadFiles, type UploadResult } from '../api/objects'

interface Props {
  bucket: string
  prefix: string
  onUploaded: (results: UploadResult[]) => void
}

async function readDirectoryEntries(entry: FileSystemDirectoryEntry): Promise<File[]> {
  const reader = entry.createReader()
  const files: File[] = []

  const readBatch = (): Promise<FileSystemEntry[]> =>
    new Promise((resolve, reject) => reader.readEntries(resolve, reject))

  let batch = await readBatch()
  while (batch.length > 0) {
    for (const child of batch) {
      if (child.isFile) {
        const file = await new Promise<File>((resolve, reject) =>
          (child as FileSystemFileEntry).file(resolve, reject)
        )
        const relativePath = child.fullPath.replace(/^\//, '')
        Object.defineProperty(file, 'webkitRelativePath', { value: relativePath })
        files.push(file)
      } else if (child.isDirectory) {
        const subFiles = await readDirectoryEntries(child as FileSystemDirectoryEntry)
        files.push(...subFiles)
      }
    }
    batch = await readBatch()
  }
  return files
}

async function collectFiles(dataTransfer: DataTransfer): Promise<{ files: File[]; hasFolder: boolean }> {
  const items = dataTransfer.items
  let hasFolder = false
  if (items && items.length > 0 && typeof items[0].webkitGetAsEntry === 'function') {
    const allFiles: File[] = []
    const entries: FileSystemEntry[] = []

    for (let i = 0; i < items.length; i++) {
      const entry = items[i].webkitGetAsEntry()
      if (entry) entries.push(entry)
    }

    for (const entry of entries) {
      if (entry.isDirectory) {
        hasFolder = true
        const dirFiles = await readDirectoryEntries(entry as FileSystemDirectoryEntry)
        allFiles.push(...dirFiles)
      } else if (entry.isFile) {
        const file = await new Promise<File>((resolve, reject) =>
          (entry as FileSystemFileEntry).file(resolve, reject)
        )
        allFiles.push(file)
      }
    }

    if (entries.length > 0) return { files: allFiles, hasFolder }
  }

  return { files: Array.from(dataTransfer.files), hasFolder: false }
}

export default function UploadDropzone({ bucket, prefix, onUploaded }: Props) {
  const [dragging, setDragging] = useState(false)
  const [uploading, setUploading] = useState(false)
  const [progress, setProgress] = useState(0)
  const [error, setError] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)

  const doUpload = useCallback(async (files: File[], preservePaths: boolean) => {
    if (files.length === 0) {
      setError('No files found to upload (folder may be empty)')
      return
    }
    setUploading(true)
    setProgress(0)
    setError('')
    try {
      if (preservePaths) {
        const formData = new FormData()
        for (const file of files) {
          const relPath = (file as File & { webkitRelativePath?: string }).webkitRelativePath || file.name
          formData.append('file', new File([file], relPath, { type: file.type }))
        }

        const token = localStorage.getItem('vaults3_token')
        const xhr = new XMLHttpRequest()
        xhr.open('POST', `/api/v1/buckets/${bucket}/upload?prefix=${encodeURIComponent(prefix)}`)
        if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)

        const results = await new Promise<UploadResult[]>((resolve, reject) => {
          xhr.upload.onprogress = (e) => {
            if (e.lengthComputable) setProgress(Math.round((e.loaded / e.total) * 100))
          }
          xhr.onload = () => {
            if (xhr.status >= 200 && xhr.status < 300) {
              resolve(JSON.parse(xhr.responseText))
            } else {
              reject(new Error(`Upload failed: ${xhr.statusText}`))
            }
          }
          xhr.onerror = () => reject(new Error('Upload failed'))
          xhr.send(formData)
        })
        onUploaded(results)
      } else {
        const results = await uploadFiles(bucket, files, prefix, setProgress)
        onUploaded(results)
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Upload failed')
    } finally {
      setUploading(false)
      setProgress(0)
    }
  }, [bucket, prefix, onUploaded])

  const handleDrop = useCallback(async (e: React.DragEvent) => {
    e.preventDefault()
    setDragging(false)
    const { files, hasFolder } = await collectFiles(e.dataTransfer)
    doUpload(files, hasFolder)
  }, [doUpload])

  const handleFileSelect = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(e.target.files || [])
    doUpload(files, false)
    e.target.value = ''
  }, [doUpload])

  return (
    <div
      onDragOver={(e) => { e.preventDefault(); setDragging(true) }}
      onDragLeave={() => setDragging(false)}
      onDrop={handleDrop}
      className={`relative border-2 border-dashed rounded-2xl p-8 text-center transition-all duration-300 ${dragging
        ? 'border-indigo-500 bg-indigo-50/80 dark:bg-indigo-900/30 scale-[1.01]'
        : 'border-gray-300 dark:border-gray-600 hover:border-indigo-300 dark:hover:border-indigo-700 bg-gray-50/50 dark:bg-gray-800/30'
        }`}
    >
      {uploading ? (
        <div>
          <p className="text-sm text-gray-600 dark:text-gray-400 mb-2">Uploading... {progress}%</p>
          <div className="w-full bg-gray-200 dark:bg-gray-700 rounded-full h-2">
            <div
              className="bg-indigo-600 h-2 rounded-full transition-all duration-300"
              style={{ width: `${progress}%` }}
            />
          </div>
        </div>
      ) : (
        <div>
          <svg className="w-8 h-8 mx-auto text-gray-400 dark:text-gray-500 mb-2" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={1.5}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M3 16.5v2.25A2.25 2.25 0 005.25 21h13.5A2.25 2.25 0 0021 18.75V16.5m-13.5-9L12 3m0 0l4.5 4.5M12 3v13.5" />
          </svg>
          <p className="text-sm text-gray-600 dark:text-gray-400">
            Drag & drop files or folders here or{' '}
            <button
              onClick={() => inputRef.current?.click()}
              className="text-indigo-600 dark:text-indigo-400 hover:underline font-medium"
            >
              browse
            </button>
          </p>
          <input
            ref={inputRef}
            type="file"
            multiple
            onChange={handleFileSelect}
            className="hidden"
          />
        </div>
      )}
      {error && (
        <p className="mt-2 text-sm text-red-600 dark:text-red-400">{error}</p>
      )}
    </div>
  )
}
