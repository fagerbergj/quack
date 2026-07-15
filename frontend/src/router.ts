import { useState, useEffect } from 'react'

// Minimal History-API routing: the app has two views (Chat, GitHub) and one
// URL param, the chat id in /chat/:chatId. No router dependency needed.

export type View = 'chat' | 'github'

function readChatId(): string | undefined {
  const m = window.location.pathname.match(/^\/chat\/([^/]+)/)
  return m ? decodeURIComponent(m[1]) : undefined
}

function readView(): View {
  return /^\/github(\/|$)/.test(window.location.pathname) ? 'github' : 'chat'
}

export function navigate(path: string, opts?: { replace?: boolean }) {
  if (opts?.replace) window.history.replaceState(null, '', path)
  else window.history.pushState(null, '', path)
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

// Current top-level view ('chat' or 'github') from the URL; re-renders on
// navigate() and browser back/forward.
export function useView(): View {
  const [view, setView] = useState(readView)
  useEffect(() => {
    const onPop = () => setView(readView())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  return view
}
