import { useEffect, useState } from 'react'
import { api, type ExtensionInfo } from '../api'
import { useExtName } from '../router'

export interface ExtensionHostProps {
  // Storybook/test seam: overrides the URL-derived extension name.
  name?: string
  // Storybook/test seam (same pattern as NavRail's initialExtensions):
  // pre-seeds the extensions list and skips the live GET /api/v1/extensions fetch.
  initialExtensions?: ExtensionInfo[]
}

// Hosts an extension's own UI inside the SPA shell (#870), routed at
// /ext/:name - a same-origin iframe in the content pane instead of NavRail's
// old <a href>, which left the app (and its own back-nav) behind entirely.
// The extension's server-side route (e.g. /usage/) is untouched and still
// works navigated to directly; this is purely an additional SPA-side wrapper.
export default function ExtensionHost({ name: nameOverride, initialExtensions }: ExtensionHostProps) {
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

  if (extensions === undefined) {
    return (
      <div className="flex-1 flex items-center justify-center text-sm text-gray-400 dark:text-gray-500">
        Loading…
      </div>
    )
  }

  const ext = extensions.find(e => e.name === name && e.href)

  if (!ext || !ext.href) {
    return (
      <div className="flex-1 flex items-center justify-center text-sm text-gray-400 dark:text-gray-500">
        Extension not found
      </div>
    )
  }

  return (
    <iframe
      src={ext.href}
      title={ext.title ?? ext.name}
      // allow-top-navigation-by-user-activation: an extension page can link
      // out to a real SPA route (e.g. remarkable's status page linking a
      // document to its /chat/ext:remarkable:<id> chat) - user-activation-
      // gated only, so the extension itself can't force a top navigation.
      sandbox="allow-same-origin allow-scripts allow-forms allow-top-navigation-by-user-activation"
      className="flex-1 w-full h-full border-0"
    />
  )
}
