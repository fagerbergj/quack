import { useEffect } from 'react'
import Chat from './pages/Chat'
import Memory from './pages/Memory'
import { useRoute } from './router'

export default function App() {
  const route = useRoute()

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
      {/* Two pages: Chat (default, /chat/:chatId + the sidebar's filter query
          params) and Memory (/memory - #727). Both routed by src/router.ts's
          plain path matcher, no router dependency. */}
      {route === 'memory' ? <Memory /> : <Chat />}
    </div>
  )
}
