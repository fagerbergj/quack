import { useEffect, useState } from 'react'
import { api, type ExtensionInfo } from '../api'
import { navigate, useExtName } from '../router'
import { NavToggle } from '../components/NavToggle'

export interface ExtensionHostProps {
  // Storybook/test seam: overrides the URL-derived extension name.
  name?: string
  // Storybook/test seam: pre-seeds the extensions list and skips the live fetch.
  initialExtensions?: ExtensionInfo[]
  // Optional so stories/tests without the app shell render the bare iframe with no header bar.
  navOpen?: boolean
  onToggleNav?: () => void
}

// A same-origin iframe keeps the app and its back-nav; the extension's server route still works navigated
// to directly.
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

  // A one-button bar keeps the drawer reachable; omitted outside the app shell (no nav props).
  const header = navOpen !== undefined && onToggleNav !== undefined ? (
    <div className="flex-shrink-0 flex items-center gap-2 px-2 py-1 border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800">
      <NavToggle open={navOpen} onToggle={onToggleNav} />
    </div>
  ) : null

  if (extensions === undefined) {
    return (
      <>
        {header}
        <div className="flex-1 flex items-center justify-center text-sm text-gray-500 dark:text-gray-400">
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
        <div className="flex-1 flex flex-col items-center justify-center gap-4 text-sm text-gray-500 dark:text-gray-400">
          Extension not found
          <a href="/chat" onClick={e => { e.preventDefault(); navigate('/chat') }} className="min-h-[44px] inline-flex items-center px-4 rounded-lg text-blue-600 dark:text-blue-400 hover:underline">
            Back to chats
          </a>
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
