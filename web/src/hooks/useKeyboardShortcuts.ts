import { useState, useEffect, useCallback } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from './useAuth'
import { isAdminSession } from '../api/auth'

export function useKeyboardShortcuts() {
  // Search is admin-only on the server, so a non-admin pressing / landed on a
  // page whose every request was refused.
  const { user } = useAuth()
  const canSearch = isAdminSession(user)
  const [showHelp, setShowHelp] = useState(false)
  const navigate = useNavigate()

  const handleKeyDown = useCallback((e: KeyboardEvent) => {
    // Ignore when typing in inputs
    const tag = (e.target as HTMLElement).tagName
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') {
      if (e.key === 'Escape') {
        ;(e.target as HTMLElement).blur()
      }
      return
    }

    // Ignore with modifier keys (allow Shift for ?)
    if (e.ctrlKey || e.metaKey || e.altKey) return

    switch (e.key) {
      case '/': {
        if (!canSearch) break
        e.preventDefault()
        navigate('/search')
        // Focus the search input after navigation
        setTimeout(() => {
          const input = document.querySelector<HTMLInputElement>('input[type="text"], input[type="search"]')
          input?.focus()
        }, 100)
        break
      }
      case '?': {
        e.preventDefault()
        setShowHelp(prev => !prev)
        break
      }
      case 'Escape': {
        setShowHelp(false)
        break
      }
    }
  }, [navigate, canSearch])

  useEffect(() => {
    document.addEventListener('keydown', handleKeyDown)
    return () => document.removeEventListener('keydown', handleKeyDown)
  }, [handleKeyDown])

  return { showHelp, setShowHelp }
}

export const shortcuts = [
  { key: '/', descKey: 'shortcuts.search', adminOnly: true },
  { key: '?', descKey: 'shortcuts.help', adminOnly: false },
  { key: 'Esc', descKey: 'shortcuts.escape', adminOnly: false },
]
