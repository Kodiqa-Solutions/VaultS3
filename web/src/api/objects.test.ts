import { describe, it, expect, beforeEach, vi } from 'vitest'
import {
  deleteObject, listObjects, searchObjectsInPrefix, bulkDeleteObjects, getDownloadUrl, getDownloadZipUrl,
  fetchObjectText, uploadUrl, summarizeBulkDelete,
} from './objects'

function createStorage(): Storage {
  const m = new Map<string, string>()
  return {
    get length() { return m.size },
    clear: () => m.clear(),
    getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
    key: (i: number) => Array.from(m.keys())[i] ?? null,
    removeItem: (k: string) => { m.delete(k) },
    setItem: (k: string, v: string) => { m.set(k, String(v)) },
  }
}

// Records every request so a test can check exactly which URL went out.
function captureFetch(body: unknown = null) {
  const calls: { url: string; init?: RequestInit }[] = []
  globalThis.fetch = vi.fn(async (url: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(url), init })
    return new Response(body === null ? null : JSON.stringify(body), { status: body === null ? 204 : 200 })
  }) as unknown as typeof fetch
  return calls
}

beforeEach(() => {
  globalThis.localStorage = createStorage()
  globalThis.sessionStorage = createStorage()
  localStorage.setItem('vaults3_token', 'jwt-abc')
})

describe('object paths', () => {
  // The #62 bug class: an unencoded '?' ended the path, so deleting "a?b"
  // deleted the object "a" sitting next to it.
  it('deleteObject targets the named key, not a bystander', async () => {
    const calls = captureFetch()
    await deleteObject('photos', 'dir/a?b#c 100%.txt')
    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/buckets/photos/objects/dir/a%3Fb%23c%20100%25.txt')
    expect(calls[0].init?.method).toBe('DELETE')
  })

  it('deleteObject refuses a key the URL would rewrite, and sends nothing', async () => {
    const calls = captureFetch()
    await expect(deleteObject('photos', 'dir/../secret')).rejects.toThrow()
    expect(calls).toHaveLength(0)
  })

  it('list and search encode the bucket and put the prefix in the query', async () => {
    const calls = captureFetch({ objects: [], truncated: false, prefix: '' })
    await listObjects('photos', 'a&b=c/')
    await searchObjectsInPrefix('photos', 'x#/', 'q?')
    expect(calls[0].url).toBe('/api/v1/buckets/photos/objects?prefix=a%26b%3Dc%2F')
    expect(calls[1].url).toBe('/api/v1/buckets/photos/search?q=q%3F&prefix=x%23%2F')
  })

  it('bulk delete sends keys in the body, untouched', async () => {
    const calls = captureFetch([])
    await bulkDeleteObjects('photos', ['a?b', 'x/../y'])
    expect(calls[0].url).toBe('/api/v1/buckets/photos/bulk-delete')
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ keys: ['a?b', 'x/../y'] })
  })

  it('getDownloadUrl encodes the key and keeps the token for a plain link', () => {
    expect(getDownloadUrl('photos', 'dir/a?b.txt')).toBe('/api/v1/buckets/photos/download/dir/a%3Fb.txt?token=jwt-abc')
  })

  it('getDownloadUrl gives no link for a key a path cannot carry', () => {
    expect(getDownloadUrl('photos', 'a/../b')).toBeNull()
  })

  it('uploadUrl encodes the prefix as a query value', () => {
    expect(uploadUrl('photos', 'a b/#1/')).toBe('/api/v1/buckets/photos/upload?prefix=a+b%2F%231%2F')
  })
})

describe('getDownloadZipUrl', () => {
  // Joining with ',' turned the key "a,b" into the two keys "a" and "b".
  it('sends one key parameter per key, so a comma stays inside its key', () => {
    const url = getDownloadZipUrl('photos', ['a,b.txt', 'dir/c&d.txt'])
    const parsed = new URL(url, 'http://x')
    expect(parsed.pathname).toBe('/api/v1/buckets/photos/download-zip')
    expect(parsed.searchParams.getAll('key')).toEqual(['a,b.txt', 'dir/c&d.txt'])
    expect(parsed.searchParams.has('keys')).toBe(false)
    expect(parsed.searchParams.get('token')).toBe('jwt-abc')
  })
})

describe('fetchObjectText', () => {
  // A preview is a fetch, so it can send the token as a header. In the URL it
  // would land in every proxy access log on each preview click.
  it('sends the token as a header and never in the URL', async () => {
    const calls: { url: string; init?: RequestInit }[] = []
    globalThis.fetch = vi.fn(async (url: string | URL | Request, init?: RequestInit) => {
      calls.push({ url: String(url), init })
      return new Response('hello', { status: 200 })
    }) as unknown as typeof fetch
    await expect(fetchObjectText('photos', 'notes/a?.md')).resolves.toBe('hello')
    expect(calls[0].url).toBe('/api/v1/buckets/photos/download/notes/a%3F.md')
    expect(calls[0].url).not.toContain('token')
    expect((calls[0].init?.headers as Record<string, string>).Authorization).toBe('Bearer jwt-abc')
  })

  it('returns null on an error status', async () => {
    globalThis.fetch = vi.fn(async () => new Response('no', { status: 403 })) as unknown as typeof fetch
    await expect(fetchObjectText('photos', 'a.txt')).resolves.toBeNull()
  })
})

describe('summarizeBulkDelete', () => {
  it('counts only the keys the server confirmed', () => {
    const s = summarizeBulkDelete(['a', 'b', 'c'], [
      { key: 'a', deleted: true },
      { key: 'b', deleted: false, error: 'not found' },
      { key: 'c', deleted: true },
    ])
    expect(s.deleted).toBe(2)
    expect(s.failed).toEqual([{ key: 'b', deleted: false, error: 'not found' }])
  })

  it('treats a key with no result as failed', () => {
    const s = summarizeBulkDelete(['a', 'b'], [{ key: 'a', deleted: true }])
    expect(s.deleted).toBe(1)
    expect(s.failed.map(f => f.key)).toEqual(['b'])
  })

  it('survives a null body', () => {
    expect(summarizeBulkDelete(['a'], null)).toEqual({ deleted: 0, failed: [{ key: 'a', deleted: false }] })
  })
})
