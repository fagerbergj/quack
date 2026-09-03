import { useEffect, useState } from 'react'

// Shared matchMedia subscription - useCompact() and ChatList's own
// md:-matching query both build on this instead of each rolling their own
// listener/cleanup.
export function useMediaQuery(query: string): boolean {
  const supported = typeof window !== 'undefined' && typeof window.matchMedia === 'function'
  const [matches, setMatches] = useState(() => supported && window.matchMedia(query).matches)
  useEffect(() => {
    if (!supported) return
    const mql = window.matchMedia(query)
    const onChange = () => setMatches(mql.matches)
    onChange()
    mql.addEventListener('change', onChange)
    return () => mql.removeEventListener('change', onChange)
  }, [supported, query])
  return matches
}
