import { useState, useEffect } from 'react'

// Minimal History-API routing: the app has one view (Chat) and one URL param,
// the chat id in /chat/:chatId, plus the sidebar's filter/search state as
// query params. No router dependency needed.

function readChatId(): string | undefined {
  const m = window.location.pathname.match(/^\/chat\/([^/]+)/)
  return m ? decodeURIComponent(m[1]) : undefined
}

// navigate(path) preserves the current query string when path doesn't specify
// its own (no '?') - so switching chats never drops the sidebar's filter state.
export function navigate(path: string, opts?: { replace?: boolean }) {
  const full = path.includes('?') ? path : path + window.location.search
  if (opts?.replace) window.history.replaceState(null, '', full)
  else window.history.pushState(null, '', full)
  window.dispatchEvent(new PopStateEvent('popstate'))
}

// Current chatId from the URL; re-renders on navigate() and browser back/forward.
export function useChatId(): string | undefined {
  const [chatId, setChatId] = useState(readChatId)
  useEffect(() => {
    const onPop = () => setChatId(readChatId())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  return chatId
}

// The app's pages: a plain path match, same spirit as readChatId - no route
// table, no dependency, just the paths this needs. 'ext' (#870) hosts an
// extension's own UI inside the SPA shell at /ext/<name>, rather than a real
// <a href> that would navigate away from the app entirely.
export type Route = 'chat' | 'memory' | 'ext'

// Pure (no window access) so it's directly testable - see router.test.ts.
// Anchored to the full segment (/memory or /memory/...), not a bare prefix:
// startsWith('/memory') would also match /memory-export.
export function routeFor(pathname: string): Route {
  if (/^\/ext(\/|$)/.test(pathname)) return 'ext'
  return /^\/memory(\/|$)/.test(pathname) ? 'memory' : 'chat'
}

function readRoute(): Route {
  return routeFor(window.location.pathname)
}

// Current top-level route from the URL; re-renders on navigate() and browser back/forward.
export function useRoute(): Route {
  const [route, setRoute] = useState(readRoute)
  useEffect(() => {
    const onPop = () => setRoute(readRoute())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  return route
}

function readExtName(): string | undefined {
  const m = window.location.pathname.match(/^\/ext\/([^/]+)/)
  return m ? decodeURIComponent(m[1]) : undefined
}

// Current extension name from a /ext/:name URL; re-renders on navigate() and
// browser back/forward - the ExtensionHost page's counterpart to useChatId.
export function useExtName(): string | undefined {
  const [name, setName] = useState(readExtName)
  useEffect(() => {
    const onPop = () => setName(readExtName())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  return name
}

// Current query string (e.g. "?q=foo&status=running"), the sidebar's filter
// state; re-renders on navigate() and browser back/forward. Write with
// navigate(pathname + '?' + qs, { replace: true }).
export function useSearch(): string {
  const [search, setSearch] = useState(() => window.location.search)
  useEffect(() => {
    const onPop = () => setSearch(window.location.search)
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  return search
}
