import { describe, it, expect, beforeEach, vi } from 'vitest'
import {
  deleteUser, attachUserPolicy, detachUserPolicy, addUserToGroup, removeUserFromGroup,
  deleteGroup, attachGroupPolicy, detachGroupPolicy, deletePolicy, setIPRestrictions,
} from './iam'
import { deleteKey } from './keys'
import { deleteBucket, getBucketVersioning } from './buckets'
import { deleteBucketTriggers, setBucketTriggers } from './lambda'
import { deleteSnapshot, restoreSnapshot, diffSnapshot, listSnapshots } from './snapshots'

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

let calls: { url: string; method: string }[] = []

beforeEach(() => {
  globalThis.localStorage = createStorage()
  globalThis.sessionStorage = createStorage()
  calls = []
  globalThis.fetch = vi.fn(async (url: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(url), method: init?.method ?? 'GET' })
    return new Response(null, { status: 204 })
  }) as unknown as typeof fetch
})

// Each row is a builder that used to interpolate a raw name. With "a?b" the
// server saw "a", so the action hit whoever is called "a" (issue #62).
describe('name paths', () => {
  const rows: [string, () => Promise<unknown>, string][] = [
    ['deleteUser', () => deleteUser('a?b'), '/api/v1/iam/users/a%3Fb'],
    ['attachUserPolicy', () => attachUserPolicy('a#b', 'p'), '/api/v1/iam/users/a%23b/policies'],
    ['detachUserPolicy', () => detachUserPolicy('a b', 'read?only'), '/api/v1/iam/users/a%20b/policies/read%3Fonly'],
    ['addUserToGroup', () => addUserToGroup('u%41', 'g'), '/api/v1/iam/users/u%2541/groups'],
    ['removeUserFromGroup', () => removeUserFromGroup('u', 'dev#ops'), '/api/v1/iam/users/u/groups/dev%23ops'],
    ['deleteGroup', () => deleteGroup('ops?x'), '/api/v1/iam/groups/ops%3Fx'],
    ['attachGroupPolicy', () => attachGroupPolicy('g+1', 'p'), '/api/v1/iam/groups/g%2B1/policies'],
    ['detachGroupPolicy', () => detachGroupPolicy('g', 'p?q'), '/api/v1/iam/groups/g/policies/p%3Fq'],
    ['deletePolicy', () => deletePolicy('full#access'), '/api/v1/iam/policies/full%23access'],
    ['setIPRestrictions', () => setIPRestrictions('a?b', []), '/api/v1/iam/users/a%3Fb/ip-restrictions'],
    ['deleteKey', () => deleteKey('AK?X'), '/api/v1/keys/AK%3FX'],
    ['deleteBucket', () => deleteBucket('my.bucket'), '/api/v1/buckets/my.bucket'],
    ['getBucketVersioning', () => getBucketVersioning('b'), '/api/v1/buckets/b/versioning'],
    ['setBucketTriggers', () => setBucketTriggers('b#1', []), '/api/v1/lambda/triggers/b%231'],
    ['deleteBucketTriggers', () => deleteBucketTriggers('b?1'), '/api/v1/lambda/triggers/b%3F1'],
    ['listSnapshots', () => listSnapshots('b'), '/api/v1/buckets/b/snapshots'],
    ['diffSnapshot', () => diffSnapshot('b', 's?1'), '/api/v1/buckets/b/snapshots/s%3F1/diff'],
    ['restoreSnapshot', () => restoreSnapshot('b', 's#1'), '/api/v1/buckets/b/snapshots/s%231/restore'],
    ['deleteSnapshot', () => deleteSnapshot('b', 's 1'), '/api/v1/buckets/b/snapshots/s%201'],
  ]
  for (const [name, call, want] of rows) {
    it(`${name} sends the whole name`, async () => {
      await call()
      expect(calls.map(c => c.url)).toEqual([want])
    })
  }

  // %2F would be decoded back into a separator by the server, so "x/groups/admins"
  // would remove user x from the admins group instead of deleting a user.
  it('refuses a name with a slash and sends nothing', async () => {
    await expect(deleteUser('x/groups/admins')).rejects.toThrow()
    await expect(deleteKey('../settings')).rejects.toThrow()
    expect(calls).toHaveLength(0)
  })
})
