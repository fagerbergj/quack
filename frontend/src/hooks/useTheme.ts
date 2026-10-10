import { useCallback, useEffect, useState } from 'react'

export type Theme = 'light' | 'dark' | 'system'

const STORAGE_KEY = 'theme'
const query = () => window.matchMedia('(prefers-color-scheme: dark)')

function readTheme(): Theme {
  const stored = localStorage.getItem(STORAGE_KEY)
  return stored === 'dark' || stored === 'light' ? stored : 'system'
}

// Toggles Tailwind's `dark` class on <html>; index.css derives color-scheme from it.
// Exported so App can apply it before first paint, ahead of this hook's effect.
export function applyTheme(theme: Theme = readTheme()) {
  const dark = theme === 'dark' || (theme === 'system' && query().matches)
  document.documentElement.classList.toggle('dark', dark)
}

export function useTheme(): [Theme, (t: Theme) => void] {
  const [theme, setThemeState] = useState<Theme>(readTheme)

  useEffect(() => {
    applyTheme(theme)
    if (theme !== 'system') return
    const mql = query()
    const onChange = () => applyTheme('system')
    mql.addEventListener('change', onChange)
    return () => mql.removeEventListener('change', onChange)
  }, [theme])

  const setTheme = useCallback((t: Theme) => {
    if (t === 'system') localStorage.removeItem(STORAGE_KEY)
    else localStorage.setItem(STORAGE_KEY, t)
    setThemeState(t)
  }, [])

  return [theme, setTheme]
}
