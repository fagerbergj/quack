import { Suspense, lazy, useEffect, useState } from 'react'
import Chat from './pages/Chat'
import { NavRail } from './components/NavRail'
import { LazyLoadBoundary } from './components/LazyLoadBoundary'

// Chat is the default route and loads eagerly; Memory, Plugins, and the
// extension iframe host are route-split out of the entry chunk (~19.7 kB gzip).
const Memory = lazy(() => import('./pages/Memory'))
const Plugins = lazy(() => import('./pages/Plugins'))
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

  // The nav drawer is the only navigation: closed on load, never persisted; each page header's NavToggle flips it.
  const [navOpen, setNavOpen] = useState(false)

  // Theme init runs here (not in Chat) so it applies before first paint on any route; the kebab's useTheme()
  // handles OS-change and in-app switching.
  useEffect(() => {
    applyTheme()
  }, [])

  return (
    // h-dvh, not h-screen: 100vh stays tall while browser chrome hides the pinned composer.
    // iOS Safari's keyboard shrinks visualViewport, not dvh, hence the inline height override.
    <div className="h-dvh flex" style={vvHeight != null ? { height: vvHeight } : undefined}>
      {/* Nav drawer, common to every route; renders nothing or a fixed overlay, never layout. */}
      <NavRail
        route={route}
        activeExtension={extName}
        open={navOpen}
        onClose={() => setNavOpen(false)}
      />
      <div className="flex-1 min-w-0 h-full flex flex-col overflow-hidden">
        {/* key=route: the lazy routes share this slot, so without a key React reuses one LazyLoadBoundary and a
            failure on one route keeps showing on the next. */}
        {route === 'memory'
          ? <LazyLoadBoundary key={route}><Suspense fallback={routeFallback}><Memory navOpen={navOpen} onToggleNav={() => setNavOpen(o => !o)} /></Suspense></LazyLoadBoundary>
          : route === 'plugins'
            ? <LazyLoadBoundary key={route}><Suspense fallback={routeFallback}><Plugins navOpen={navOpen} onToggleNav={() => setNavOpen(o => !o)} /></Suspense></LazyLoadBoundary>
            : route === 'ext'
              ? <LazyLoadBoundary key={route}><Suspense fallback={routeFallback}><ExtensionHost navOpen={navOpen} onToggleNav={() => setNavOpen(o => !o)} /></Suspense></LazyLoadBoundary>
              : <Chat navOpen={navOpen} onToggleNav={() => setNavOpen(o => !o)} />}
      </div>
    </div>
  )
}
