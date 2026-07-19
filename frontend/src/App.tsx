import { useEffect } from 'react'
import Chat from './pages/Chat'

export default function App() {
  // Theme init must run here (not in Chat) so it applies before first paint
  // regardless of which chat route is active.
  useEffect(() => {
    const stored = localStorage.getItem('theme')
    if (stored === 'dark' || (!stored && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
      document.documentElement.classList.add('dark')
    }
  }, [])

  return (
    <div className="h-screen flex flex-col">
      {/* The Chat page owns the full-screen layout and its own chat-list
          sidebar; routing is a single URL param (/chat/:chatId) plus the
          sidebar's filter query params, handled in src/router.ts. */}
      <Chat />
    </div>
  )
}
