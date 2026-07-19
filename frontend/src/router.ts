import { useState, useEffect } from 'react'

// Minimal History-API routing: the app has one view (Chat) and one URL param,
// the chat id in /chat/:chatId, plus the sidebar's filter/search state as
// query params. No router dependency needed.

function readChatId(): string | undefined {
  const m = window.location.pathname.match(/^\/chat\/([^/]+)/)
  return m ? decodeURIComponent(m[1]) : undefined
}

// navigate(path) preserves the current query string when path doesn't specify
// its own (no '?') — so switching chats never drops the sidebar's filter state.
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
