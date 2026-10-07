import { describe, it, expect, vi } from 'vitest'
import { createInFlightGuard } from './useInFlight'

function deferred() {
  let resolve!: () => void
  let reject!: (e: Error) => void
  const promise = new Promise<void>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

describe('createInFlightGuard', () => {
  // A double click lands before React re-renders the button as disabled, so the
  // guard itself must drop the second call or the DELETE is sent twice.
  it('drops a second call while the first is still running', async () => {
    const guard = createInFlightGuard(() => {})
    const d = deferred()
    const action = vi.fn(() => d.promise)
    const first = guard.run(action)
    const second = guard.run(action)
    expect(action).toHaveBeenCalledTimes(1)
    await expect(second).resolves.toBeUndefined()
    d.resolve()
    await first
    expect(action).toHaveBeenCalledTimes(1)
  })

  it('lets the next call through once the first has finished', async () => {
    const guard = createInFlightGuard(() => {})
    const action = vi.fn(async () => 'ok')
    await expect(guard.run(action)).resolves.toBe('ok')
    await expect(guard.run(action)).resolves.toBe('ok')
    expect(action).toHaveBeenCalledTimes(2)
  })

  it('reports pending for the disabled state and clears it after a failure', async () => {
    const states: boolean[] = []
    const guard = createInFlightGuard(p => states.push(p))
    await expect(guard.run(async () => { throw new Error('boom') })).rejects.toThrow('boom')
    expect(states).toEqual([true, false])
    const action = vi.fn(async () => {})
    await guard.run(action)
    expect(action).toHaveBeenCalledTimes(1)
  })
})
