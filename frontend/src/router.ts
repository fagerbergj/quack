import { useSyncExternalStore } from 'react'

// Minimal History-API routing: the chat id in /chat/:chatId plus the sidebar's filter/search query params.

function readChatId(): string | undefined {
  const m = window.location.pathname.match(/^\/chat\/([^/]+)/)
  return m ? decodeURIComponent(m[1]) : undefined
}

// navigate(path) preserves the current query string when path doesn't specify
// its own (no '?') - so switching chats never drops the sidebar's filter state.
export function navigate(path: string, opts?: { replace?: boolean }) {
  // A path ending in a bare '?' means "no query" explicitly - without that
  // marker, clearing the last filter char would re-append the old search.
  const full = path.includes('?') ? path.replace(/\?$/, '') : path + window.location.search
  if (opts?.replace) window.history.replaceState(null, '', full)
  else window.history.pushState(null, '', full)
  window.dispatchEvent(new PopStateEvent('popstate'))
}

function subscribeLocation(onChange: () => void): () => void {
  window.addEventListener('popstate', onChange)
  return () => window.removeEventListener('popstate', onChange)
}

// read must return a primitive: useSyncExternalStore compares snapshots by identity.
function useLocation<T extends string | undefined>(read: () => T): T {
  return useSyncExternalStore(subscribeLocation, read)
}

// Each hook re-renders on navigate() and browser back/forward.
export const useChatId = () => useLocation(readChatId)

// Plain path match, no route table. 'ext' hosts an extension's UI inside the SPA shell at /ext/<name>.
export type Route = 'chat' | 'memory' | 'plugins' | 'ext'

// Pure (no window access) so it's directly testable. Anchored to the full segment:
// startsWith('/memory') would also match /memory-export.
export function routeFor(pathname: string): Route {
  if (/^\/ext(\/|$)/.test(pathname)) return 'ext'
  if (/^\/memory(\/|$)/.test(pathname)) return 'memory'
  if (/^\/plugins(\/|$)/.test(pathname)) return 'plugins'
  return 'chat'
}

function readRoute(): Route {
  return routeFor(window.location.pathname)
}

export const useRoute = () => useLocation(readRoute)

function readExtName(): string | undefined {
  const m = window.location.pathname.match(/^\/ext\/([^/]+)/)
  return m ? decodeURIComponent(m[1]) : undefined
}

export const useExtName = () => useLocation(readExtName)

// The sidebar's filter state, e.g. "?q=foo&status=running". Write with
// navigate(pathname + '?' + qs, { replace: true }).
export const useSearch = () => useLocation(() => window.location.search)
