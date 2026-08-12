import { useEffect } from 'react'
import Chat from './pages/Chat'
import Memory from './pages/Memory'
import ExtensionHost from './pages/ExtensionHost'
import { NavRail } from './components/NavRail'
import { useRoute, useExtName } from './router'

export default function App() {
  const route = useRoute()
  const extName = useExtName()

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
    <div className="h-dvh flex">
      {/* Persistent left rail (#746 item 1): Chats/Memory as peers, outside
          the page switch below so it's common to every route. */}
      <NavRail route={route} activeExtension={extName} />
      <div className="flex-1 min-w-0 h-full flex flex-col overflow-hidden">
        {/* Three pages: Chat (default, /chat/:chatId + the sidebar's filter
            query params), Memory (/memory - #727), and ExtensionHost
            (/ext/:name - #870, an extension's own UI in an iframe so the
            rail stays put). All routed by src/router.ts's plain path
            matcher, no router dependency. */}
        {route === 'memory' ? <Memory /> : route === 'ext' ? <ExtensionHost /> : <Chat />}
      </div>
    </div>
  )
}
