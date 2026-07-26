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
    // h-dvh, not h-screen: 100vh is the layout viewport, which on mobile
    // stays taller than what's actually visible while browser chrome (URL
    // bar/toolbar) covers part of the screen - pinning the composer at the
    // bottom of a too-tall box puts it underneath that chrome. 100dvh
    // tracks the real visible viewport instead.
    <div className="h-dvh flex flex-col">
      {/* The Chat page owns the full-screen layout and its own chat-list
          sidebar; routing is a single URL param (/chat/:chatId) plus the
          sidebar's filter query params, handled in src/router.ts. */}
      <Chat />
    </div>
  )
}
