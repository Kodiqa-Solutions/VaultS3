import { describe, it, expect } from 'vitest'
import { visibleNavItems, isAdminOnlyPath } from './Sidebar'
import { isAdminSession, sessionLabel } from '../api/auth'

// Pages whose API routes the server refuses with 403 for any subject but
// "admin" (the adminPaths list in internal/api/api.go), plus the server wide
// activity, stats, cost and notification views.
const ADMIN_PAGES = [
  '/search', '/access-keys', '/iam', '/audit', '/notifications', '/lambda', '/replication',
  '/migrate', '/backup', '/activity', '/stats', '/cost', '/settings',
]

describe('sidebar for a non-admin session', () => {
  it('shows only the pages a non-admin can use', () => {
    expect(visibleNavItems(false).map(i => i.to)).toEqual(['/', '/buckets'])
  })

  it('hides every admin page', () => {
    const shown = visibleNavItems(false).map(i => i.to)
    for (const p of ADMIN_PAGES) {
      expect(shown).not.toContain(p)
      expect(isAdminOnlyPath(p)).toBe(true)
    }
  })

  it('shows everything to the admin once every feature is on', () => {
    const allOn = { lambda: true, replication: true, backup: true }
    expect(visibleNavItems(true, allOn).map(i => i.to)).toEqual(['/', '/buckets', ...ADMIN_PAGES])
  })
})

// Issue #63: a page for a feature that is off only said "enable this in
// vaults3.yaml". It is listed once the feature is on, and Settings says how.
describe('sidebar pages that need a feature', () => {
  const FEATURE_PAGES = ['/lambda', '/replication', '/backup']
  const shown = (f: Parameters<typeof visibleNavItems>[1]) => visibleNavItems(true, f).map(i => i.to)

  it('hides a feature page while its feature is off', () => {
    const off = shown({ lambda: false, replication: false, backup: false })
    for (const p of FEATURE_PAGES) expect(off).not.toContain(p)
    expect(off).toContain('/notifications')
  })

  it('shows only the pages whose feature is on', () => {
    const some = shown({ lambda: false, replication: true, backup: false })
    expect(some).toContain('/replication')
    expect(some).not.toContain('/lambda')
    expect(some).not.toContain('/backup')
  })

  // Hidden while loading so the entries do not flash in and out.
  it('hides feature pages until the flags have loaded', () => {
    const loading = shown(undefined)
    for (const p of FEATURE_PAGES) expect(loading).not.toContain(p)
  })

  // A failed settings request must not take pages away from the admin.
  it('shows feature pages when the flags could not be read', () => {
    const failed = shown(null)
    for (const p of FEATURE_PAGES) expect(failed).toContain(p)
  })

  it('never shows a feature page to a non-admin, even with the feature on', () => {
    const on = visibleNavItems(false, { lambda: true, replication: true, backup: true }).map(i => i.to)
    expect(on).toEqual(['/', '/buckets'])
  })
})

describe('session helpers', () => {
  it('only the admin subject is an admin session', () => {
    expect(isAdminSession({ user: 'admin', accessKey: 'vaul****ret' })).toBe(true)
    expect(isAdminSession({ user: 'alice', accessKey: '' })).toBe(false)
    expect(isAdminSession(null)).toBe(false)
  })

  // /auth/me sends the access key for the admin only, so the label was blank
  // for every other user.
  it('labels a non-admin session with the user name', () => {
    expect(sessionLabel({ user: 'alice', accessKey: '' })).toBe('alice')
    expect(sessionLabel({ user: 'admin', accessKey: 'vaul****ret' })).toBe('vaul****ret')
    expect(sessionLabel(null)).toBe('')
  })
})
