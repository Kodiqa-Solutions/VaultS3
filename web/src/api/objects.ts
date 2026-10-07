import { apiFetch, getToken } from './client'
import { API_BASE } from '../basePath'
import { encodeKeyPath, encodeSegment, isAddressableKey } from './paths'

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
  error?: string
}

// uploadErrorMessage extracts a human-readable reason from a failed upload XHR.
// The server returns per-file reasons in the JSON body (e.g. "write object: no
// space left on device"), so surface the first one instead of a blank
// "Upload failed".
export function uploadErrorMessage(xhr: XMLHttpRequest): string {
  try {
    const results = JSON.parse(xhr.responseText) as UploadResult[]
    const failed = results.find((r) => r.error)
    if (failed?.error) return `Upload failed: ${failed.key}: ${failed.error}`
  } catch {
    // body was not the expected JSON array; fall through to the status text
  }
  return `Upload failed${xhr.statusText ? `: ${xhr.statusText}` : ''}`
}

export interface BulkDeleteResult {
  key: string
  deleted: boolean
  error?: string
}

export interface BulkDeleteSummary {
  deleted: number
  failed: BulkDeleteResult[]
}

// summarizeBulkDelete counts what the server actually did. It answers 200 with
// one result per key even when some keys failed, so the number of keys sent is
// not the number deleted. A key with no result at all is counted as failed:
// nothing confirmed it is gone.
export function summarizeBulkDelete(requested: string[], results: BulkDeleteResult[] | null | undefined): BulkDeleteSummary {
  const byKey = new Map((results ?? []).map(r => [r.key, r]))
  let deleted = 0
  const failed: BulkDeleteResult[] = []
  for (const key of requested) {
    const r = byKey.get(key)
    if (r?.deleted) deleted++
    else failed.push(r ?? { key, deleted: false })
  }
  return { deleted, failed }
}

export async function listObjects(bucket: string, prefix = '', maxKeys = 200, startAfter = ''): Promise<ObjectListResponse> {
  const params = new URLSearchParams()
  if (prefix) params.set('prefix', prefix)
  if (maxKeys !== 200) params.set('maxKeys', String(maxKeys))
  if (startAfter) params.set('startAfter', startAfter)
  const qs = params.toString()
  return apiFetch<ObjectListResponse>(`/buckets/${encodeSegment(bucket)}/objects${qs ? '?' + qs : ''}`)
}

// searchObjectsInPrefix filters ONE folder level — the direct children of
// `prefix`, files and sub-folders — by name, with the same query grammar as the
// global search (case-insensitive substrings, AND of terms, tag:k=v, type:x).
// The response has the listing's shape so the browser renders it unchanged;
// `truncated` + `nextStartAfter` continue a scan the server stopped early.
export async function searchObjectsInPrefix(bucket: string, prefix: string, q: string, maxKeys = 200, startAfter = ''): Promise<ObjectListResponse> {
  const params = new URLSearchParams()
  params.set('q', q)
  if (prefix) params.set('prefix', prefix)
  if (maxKeys !== 200) params.set('maxKeys', String(maxKeys))
  if (startAfter) params.set('startAfter', startAfter)
  return apiFetch<ObjectListResponse>(`/buckets/${encodeSegment(bucket)}/search?${params.toString()}`)
}

export async function deleteObject(bucket: string, key: string): Promise<void> {
  return apiFetch<void>(`/buckets/${encodeSegment(bucket)}/objects/${encodeKeyPath(key)}`, { method: 'DELETE' })
}

// Keys travel in the JSON body here, so any key is safe, including one that
// cannot be put in a path.
export async function bulkDeleteObjects(bucket: string, keys: string[]): Promise<BulkDeleteResult[]> {
  return apiFetch<BulkDeleteResult[]>(`/buckets/${encodeSegment(bucket)}/bulk-delete`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ keys }),
  })
}

function downloadPath(bucket: string, key: string): string {
  return `${API_BASE}/buckets/${encodeSegment(bucket)}/download/${encodeKeyPath(key)}`
}

// getDownloadUrl builds the link for a plain <a href> or <img src>. A browser
// navigation cannot send an Authorization header, so the session token rides in
// the query string, which the server accepts on the download routes only.
// Returns null for a key that cannot be put in a path (see isAddressableKey),
// so the page hides the link instead of downloading a different object.
export function getDownloadUrl(bucket: string, key: string): string | null {
  if (!isAddressableKey(key)) return null
  const params = new URLSearchParams()
  params.set('token', getToken() ?? '')
  return `${downloadPath(bucket, key)}?${params.toString()}`
}

// fetchObjectText reads an object for the inline preview. Unlike a link, a
// fetch can send the token as a header, which keeps it out of the URL and so
// out of proxy access logs on every preview click.
export async function fetchObjectText(bucket: string, key: string): Promise<string | null> {
  const token = getToken()
  const resp = await fetch(downloadPath(bucket, key), {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  })
  return resp.ok ? resp.text() : null
}

// One key parameter per key. Joining them with ',' split any key that itself
// contains a comma into two keys that do not exist.
export function getDownloadZipUrl(bucket: string, keys: string[]): string {
  const params = new URLSearchParams()
  for (const key of keys) params.append('key', key)
  params.set('token', getToken() ?? '')
  return `${API_BASE}/buckets/${encodeSegment(bucket)}/download-zip?${params.toString()}`
}

// uploadUrl is shared with the folder upload in UploadDropzone, which drives its
// own XHR for progress, so neither builds the path by hand.
export function uploadUrl(bucket: string, prefix: string): string {
  const params = new URLSearchParams()
  params.set('prefix', prefix)
  return `${API_BASE}/buckets/${encodeSegment(bucket)}/upload?${params.toString()}`
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

    let url: string
    try {
      url = uploadUrl(bucket, prefix)
    } catch (err) {
      reject(err)
      return
    }
    const token = getToken()
    const xhr = new XMLHttpRequest()
    xhr.open('POST', url)
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
        reject(new Error(uploadErrorMessage(xhr)))
      }
    }

    xhr.onerror = () => reject(new Error('Upload failed'))
    xhr.send(formData)
  })
}
