import { useEffect, useState } from 'react'
import Chat from './pages/Chat'
import Memory from './pages/Memory'
import ExtensionHost from './pages/ExtensionHost'
import { NavRail } from './components/NavRail'
import { useRoute, useExtName } from './router'
import { applyTheme } from './hooks/useTheme'

export default function App() {
  const route = useRoute()
  const extName = useExtName()

  // #1171: the nav drawer is the app's only navigation shape - always closed
  // on load and never persisted (the old navRailCollapsed localStorage key
  // is gone). The NavToggle in each page's header leading slot flips it.
  const [navOpen, setNavOpen] = useState(false)

  // Theme init must run here (not in Chat) so it applies before first paint
  // regardless of which chat route is active. The kebab's useTheme() picks
  // up from here and also handles OS-change/in-app switching (#1173).
  useEffect(() => {
    applyTheme()
  }, [])

  return (
    // h-dvh, not h-screen: 100vh is the layout viewport, which on mobile
    // stays taller than what's actually visible while browser chrome (URL
    // bar/toolbar) covers part of the screen - pinning the composer at the
    // bottom of a too-tall box puts it underneath that chrome. 100dvh
    // tracks the real visible viewport instead.
    <div className="h-dvh flex">
      {/* Nav drawer (#1171): Chats/Memory/extensions as a peer list, outside
          the page switch below so it's common to every route. It renders
          nothing (or a fixed overlay) - never layout - so the content
          column is the viewport minus the chat list at every width. */}
      <NavRail
        route={route}
        activeExtension={extName}
        open={navOpen}
        onClose={() => setNavOpen(false)}
      />
      <div className="flex-1 min-w-0 h-full flex flex-col overflow-hidden">
        {/* Three pages: Chat (default, /chat/:chatId + the sidebar's filter
            query params), Memory (/memory - #727), and ExtensionHost
            (/ext/:name - #870, an extension's own UI in an iframe). All
            routed by src/router.ts's plain path matcher, no router
            dependency. Each page's header leading slot carries the NavToggle
            for the drawer above, fed by this one state. */}
        {route === 'memory'
          ? <Memory navOpen={navOpen} onToggleNav={() => setNavOpen(o => !o)} />
          : route === 'ext'
            ? <ExtensionHost navOpen={navOpen} onToggleNav={() => setNavOpen(o => !o)} />
            : <Chat navOpen={navOpen} onToggleNav={() => setNavOpen(o => !o)} />}
      </div>
    </div>
  )
}
