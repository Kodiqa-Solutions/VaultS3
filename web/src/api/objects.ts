import { apiFetch, getToken } from './client'

export interface ObjectItem {
  key: string
  size: number
  lastModified: string
  contentType: string
  isPrefix: boolean
}

export interface ObjectListResponse {
  objects: ObjectItem[] | null
  truncated: boolean
  prefix: string
  nextStartAfter?: string
}

export interface UploadResult {
  key: string
  size: number
  contentType: string
}

export interface BulkDeleteResult {
  key: string
  deleted: boolean
  error?: string
}

export function listObjects(bucket: string, prefix = '', maxKeys = 200, startAfter = ''): Promise<ObjectListResponse> {
  const params = new URLSearchParams()
  if (prefix) params.set('prefix', prefix)
  if (maxKeys !== 200) params.set('maxKeys', String(maxKeys))
  if (startAfter) params.set('startAfter', startAfter)
  const qs = params.toString()
  return apiFetch<ObjectListResponse>(`/buckets/${bucket}/objects${qs ? '?' + qs : ''}`)
}

export function deleteObject(bucket: string, key: string): Promise<void> {
  return apiFetch<void>(`/buckets/${bucket}/objects/${key}`, { method: 'DELETE' })
}

export function bulkDeleteObjects(bucket: string, keys: string[]): Promise<BulkDeleteResult[]> {
  return apiFetch<BulkDeleteResult[]>(`/buckets/${bucket}/bulk-delete`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ keys }),
  })
}

export function getDownloadUrl(bucket: string, key: string): string {
  const token = getToken()
  return `/api/v1/buckets/${bucket}/download/${key}?token=${token}`
}

export function getDownloadZipUrl(bucket: string, keys: string[]): string {
  const token = getToken()
  return `/api/v1/buckets/${bucket}/download-zip?keys=${encodeURIComponent(keys.join(','))}&token=${token}`
}

export function uploadFiles(
  bucket: string,
  files: File[],
  prefix: string,
  onProgress?: (pct: number) => void,
): Promise<UploadResult[]> {
  return new Promise((resolve, reject) => {
    const formData = new FormData()
    for (const file of files) {
      formData.append('file', file)
    }

    const token = getToken()
    const xhr = new XMLHttpRequest()
    xhr.open('POST', `/api/v1/buckets/${bucket}/upload?prefix=${encodeURIComponent(prefix)}`)
    if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && onProgress) {
        onProgress(Math.round((e.loaded / e.total) * 100))
      }
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
}
