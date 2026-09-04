import { useEffect, useState } from 'react'
import { api, type ExtensionInfo } from '../api'
import { useExtName } from '../router'
import { NavToggle } from '../components/NavToggle'

export interface ExtensionHostProps {
  // Storybook/test seam: overrides the URL-derived extension name.
  name?: string
  // Storybook/test seam (same pattern as NavRail's initialExtensions):
  // pre-seeds the extensions list and skips the live GET /api/v1/extensions fetch.
  initialExtensions?: ExtensionInfo[]
  // #1171: the app's nav drawer - App.tsx owns the state and hands it down.
  // Both optional so standalone Storybook stories/tests (no app shell) render
  // the bare full-bleed iframe exactly as before, without a header bar.
  navOpen?: boolean
  onToggleNav?: () => void
}

// Hosts an extension's own UI inside the SPA shell (#870), routed at
// /ext/:name - a same-origin iframe in the content pane instead of the rail's
// old <a href>, which left the app (and its own back-nav) behind entirely.
// The extension's server-side route (e.g. /usage/) is untouched and still
// works navigated to directly; this is purely an additional SPA-side wrapper.
// #1171 gave the route a minimal header bar (the app has no persistent rail
// anymore): it carries only the NavToggle, and the iframe fills the column
// below it.
export default function ExtensionHost({ name: nameOverride, initialExtensions, navOpen, onToggleNav }: ExtensionHostProps) {
  const routeName = useExtName()
  const name = nameOverride ?? routeName
  const [extensions, setExtensions] = useState<ExtensionInfo[] | undefined>(initialExtensions)

  useEffect(() => {
    if (initialExtensions !== undefined) return // story/test seam: static demo state, no live fetch
    let cancelled = false
    api.listExtensions().then(exts => {
      if (!cancelled) setExtensions(exts)
    }).catch(() => {
      if (!cancelled) setExtensions([])
    })
    return () => {
      cancelled = true
    }
  }, [initialExtensions])

  // The route's only chrome (#1171): a one-button bar so the drawer stays
  // reachable while an extension's own document (with its own in-iframe
  // title) fills the column. Omitted outside the app shell (standalone
  // stories/tests pass no nav props).
  const header = navOpen !== undefined && onToggleNav !== undefined ? (
    <div className="flex-shrink-0 flex items-center gap-2 px-2 py-1 border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800">
      <NavToggle open={navOpen} onToggle={onToggleNav} />
    </div>
  ) : null

  if (extensions === undefined) {
    return (
      <>
        {header}
        <div className="flex-1 flex items-center justify-center text-sm text-gray-400 dark:text-gray-500">
          Loading…
        </div>
      </>
    )
  }

  const ext = extensions.find(e => e.name === name && e.href)

  if (!ext || !ext.href) {
    return (
      <>
        {header}
        <div className="flex-1 flex items-center justify-center text-sm text-gray-400 dark:text-gray-500">
          Extension not found
        </div>
      </>
    )
  }

  return (
    <>
      {header}
      <iframe
        src={ext.href}
        title={ext.title ?? ext.name}
        // allow-top-navigation-by-user-activation: lets a click link out to a
        // real SPA route (e.g. remarkable's doc -> chat), but never the extension itself.
        sandbox="allow-same-origin allow-scripts allow-forms allow-top-navigation-by-user-activation"
        className="flex-1 min-h-0 w-full border-0"
      />
    </>
  )
}
