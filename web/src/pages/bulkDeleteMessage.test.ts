import { describe, it, expect } from 'vitest'
import { makeTranslator } from '../i18n'
import { summarizeBulkDelete } from '../api/objects'
import { bulkDeleteMessage, SHOWN_FAILURES } from './bulkDeleteMessage'

const t = makeTranslator('en')

describe('bulkDeleteMessage', () => {
  it('reports success with the confirmed count when every key was deleted', () => {
    const summary = summarizeBulkDelete(['a', 'b'], [{ key: 'a', deleted: true }, { key: 'b', deleted: true }])
    expect(bulkDeleteMessage(summary, t)).toEqual({ type: 'success', text: 'Deleted 2 object(s)' })
  })

  // The old toast said "3 objects deleted" here although one key survived.
  it('reports the real count and names the failed keys with their error', () => {
    const summary = summarizeBulkDelete(['a', 'b', 'c'], [
      { key: 'a', deleted: true },
      { key: 'b', deleted: false, error: 'not found' },
      { key: 'c', deleted: true },
    ])
    const msg = bulkDeleteMessage(summary, t)
    expect(msg.type).toBe('error')
    expect(msg.text).toBe('Deleted 2 of 3 object(s). 1 could not be deleted: b: not found')
  })

  it('lists only the first few failures and counts the rest', () => {
    const keys = ['k1', 'k2', 'k3', 'k4', 'k5']
    const summary = summarizeBulkDelete(keys, keys.map(key => ({ key, deleted: false, error: 'denied' })))
    const msg = bulkDeleteMessage(summary, t)
    for (const k of keys.slice(0, SHOWN_FAILURES)) expect(msg.text).toContain(`${k}: denied`)
    expect(msg.text).not.toContain('k4: denied')
    expect(msg.text).toContain('and 2 more')
    expect(msg.text).toContain('Deleted 0 of 5')
  })

  it('explains a key the server gave no result for', () => {
    const msg = bulkDeleteMessage(summarizeBulkDelete(['a'], []), t)
    expect(msg.text).toContain('a: not confirmed by the server')
  })

  it('is translated', () => {
    const summary = summarizeBulkDelete(['a'], [{ key: 'a', deleted: true }])
    expect(bulkDeleteMessage(summary, makeTranslator('de')).text).toBe('1 Objekt(e) gelöscht')
  })
})
