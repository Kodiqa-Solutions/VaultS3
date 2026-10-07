import { useRef, useState } from 'react'

export interface InFlightGuard {
  run: <T>(fn: () => Promise<T>) => Promise<T | undefined>
}

// createInFlightGuard lets one call through at a time and drops any call made
// while it is still running. A disabled button alone is not enough: the button
// only disables after React re-renders, so a fast double click on a slow
// network can send the same DELETE or rollback twice before that happens.
export function createInFlightGuard(onChange: (pending: boolean) => void): InFlightGuard {
  let pending = false
  return {
    run: async <T>(fn: () => Promise<T>): Promise<T | undefined> => {
      if (pending) return undefined
      pending = true
      onChange(true)
      try {
        return await fn()
      } finally {
        pending = false
        onChange(false)
      }
    },
  }
}

// useInFlight returns whether a guarded action is running, for the disabled
// state of its buttons, and the function that runs an action under the guard.
export function useInFlight(): [boolean, InFlightGuard['run']] {
  const [pending, setPending] = useState(false)
  const guard = useRef<InFlightGuard | null>(null)
  if (!guard.current) guard.current = createInFlightGuard(setPending)
  return [pending, guard.current.run]
}
