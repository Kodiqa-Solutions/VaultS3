import { describe, it, expect } from 'vitest'
import React from 'react'
import { renderToString } from 'react-dom/server'
import { I18nProvider } from '../i18n'
import { Pagination, pageWindow, PAGE_SIZES, type Paging } from './table'

describe('pageWindow', () => {
  it('counts pages', () => {
    expect(pageWindow(0, 0, 50).totalPages).toBe(1)
    expect(pageWindow(50, 0, 50).totalPages).toBe(1)
    expect(pageWindow(51, 0, 50).totalPages).toBe(2)
  })

  // A delete or a new filter can shrink the list under the reader. They must
  // land on the new last page, not on an empty one past the end.
  it('clamps a page past the end of a list that shrank', () => {
    expect(pageWindow(120, 5, 50).current).toBe(2)
    expect(pageWindow(0, 3, 50).current).toBe(0)
  })
})

describe('Pagination', () => {
  function render(paging: Partial<Paging>, locale = 'zh-CN') {
    const def = (name: string, value: unknown) =>
      Object.defineProperty(globalThis, name, { value, configurable: true, writable: true })
    def('localStorage', { getItem: () => locale, setItem: () => {} })
    def('navigator', { languages: [locale], language: locale })
    def('document', { documentElement: {} })
    const full: Paging = { page: 0, setPage: () => {}, pageSize: 50, setPageSize: () => {}, totalPages: 1, total: 0, ...paging }
    return renderToString(React.createElement(I18nProvider, null, React.createElement(Pagination, { paging: full })))
  }

  it('renders nothing while the list fits on the smallest page', () => {
    expect(render({ total: PAGE_SIZES[0] })).toBe('')
  })

  // Issue #63: the audit page-size control was English in a translated UI.
  it('renders every control in the active language', () => {
    const html = render({ total: 120, totalPages: 3 })
    expect(html).toContain('120 项')
    expect(html).toContain('第 1 / 3 页')
    for (const n of PAGE_SIZES) expect(html).toContain(`每页 ${n} 条`)
    expect(html).toContain('上一页')
    expect(html).toContain('下一页')
    expect(html).not.toMatch(/entries|per page|Prev|Next/)
  })
})
