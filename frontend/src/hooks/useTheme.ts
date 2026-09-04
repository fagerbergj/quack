import { useCallback, useEffect, useState } from 'react'

export type Theme = 'light' | 'dark' | 'system'

const STORAGE_KEY = 'theme'
const query = () => window.matchMedia('(prefers-color-scheme: dark)')

function readTheme(): Theme {
  const stored = localStorage.getItem(STORAGE_KEY)
  return stored === 'dark' || stored === 'light' ? stored : 'system'
}

// Applies the resolved theme to <html> - both the `dark` class (Tailwind's
// dark: variant) and color-scheme (native controls/scrollbars/dialog
// backdrops, #1173) so they never drift apart. Exported so App can call it
// synchronously before first paint, ahead of this hook's own effect.
export function applyTheme(theme: Theme = readTheme()) {
  const dark = theme === 'dark' || (theme === 'system' && query().matches)
  document.documentElement.classList.toggle('dark', dark)
  document.documentElement.style.colorScheme = dark ? 'dark' : 'light'
}

// #1173: theme init/apply lives here (not inline in App) so the kebab menu
// can drive the same state. App still calls apply() synchronously before
// first paint; this hook is for anything that needs to read/change it after.
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
