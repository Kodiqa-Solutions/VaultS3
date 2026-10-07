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

  it('shows everything to the admin', () => {
    expect(visibleNavItems(true).map(i => i.to)).toEqual(['/', '/buckets', ...ADMIN_PAGES])
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
