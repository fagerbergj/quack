import { Suspense, lazy, useEffect, useState } from 'react'
import Chat from './pages/Chat'
import { NavRail } from './components/NavRail'
import { LazyLoadBoundary } from './components/LazyLoadBoundary'

// Chat is the default route and loads eagerly; Memory and the extension
// iframe host are route-split out of the entry chunk (~19.7 kB gzip).
const Memory = lazy(() => import('./pages/Memory'))
const ExtensionHost = lazy(() => import('./pages/ExtensionHost'))

// A null Suspense fallback leaves a blank slot while a route chunk loads, and
// a screen reader hears nothing at all; role=status announces the wait.
const routeFallback = (
  <div role="status" aria-live="polite" className="flex-1 flex items-center justify-center text-sm text-gray-500 dark:text-gray-400">Loading…</div>
)
import { useRoute, useExtName } from './router'
import { applyTheme } from './hooks/useTheme'
import { useVisualViewportHeight } from './hooks/useVisualViewportHeight'

export default function App() {
  const route = useRoute()
  const extName = useExtName()
  const vvHeight = useVisualViewportHeight()

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
    // h-dvh, not h-screen: 100vh is the layout viewport, still tall while
    // browser chrome covers part of it, hiding the pinned composer. iOS
    // Safari's keyboard shrinks visualViewport, not dvh (#1248) - hence the inline height override.
    <div className="h-dvh flex" style={vvHeight != null ? { height: vvHeight } : undefined}>
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
        {/* key=route: memory and ext share this JSX slot, so without a key
            React reuses the same LazyLoadBoundary instance across a
            navigation between them - a failure on one route would then
            keep showing its error screen on the other, unrelated route. */}
        {route === 'memory'
          ? <LazyLoadBoundary key={route}><Suspense fallback={routeFallback}><Memory navOpen={navOpen} onToggleNav={() => setNavOpen(o => !o)} /></Suspense></LazyLoadBoundary>
          : route === 'ext'
            ? <LazyLoadBoundary key={route}><Suspense fallback={routeFallback}><ExtensionHost navOpen={navOpen} onToggleNav={() => setNavOpen(o => !o)} /></Suspense></LazyLoadBoundary>
            : <Chat navOpen={navOpen} onToggleNav={() => setNavOpen(o => !o)} />}
      </div>
    </div>
  )
}
