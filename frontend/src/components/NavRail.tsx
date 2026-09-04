import { useEffect, useState } from 'react'
import { navigate, type Route } from '../router'
import { api, type ExtensionInfo } from '../api'
import { useDrawer } from '../hooks/useDrawer'

export interface NavRailProps {
  route: Route
  // The extension name from a /ext/:name route (App.tsx's useExtName()) -
  // which extension entry, if any, is active. Unused outside route === 'ext'.
  activeExtension?: string
  // Storybook/test seam (same pattern as MemoryTab's initialState): pre-seeds
  // the extension nav entries and skips the live GET /api/v1/extensions fetch.
  initialExtensions?: ExtensionInfo[]
  // #1171: whether the drawer is open. This is the only shape the nav has -
  // false renders nothing at all (zero DOM, zero layout weight), true mounts
  // the fixed overlay at every viewport width. App.tsx owns the state; the
  // drawer remembers nothing (always closed on load, no localStorage).
  open: boolean
  onClose: () => void
}

// NavRail is the app's navigation drawer (#1171): Chats and Memory as peers
// plus the extensions' own routes, in an off-canvas panel that floats over
// the content - the same overlay #1145 shipped below 600px, now at every
// width. There is no persistent rail anymore (the old w-40 column and the
// 40px collapsed strip are gone, and with them the navRailCollapsed
// localStorage key and the collapse/expand toggle). It opens from the
// NavToggle in each page's header leading slot, and closes on item
// selection, backdrop tap, the ✕ button, or Esc (focus trap, scroll lock,
// and focus-return come from useDrawer). The rail's old hamburger glyph
// (the second ☰ in the app) is deleted with the rest of the rail (#1175).
export function NavRail({ route, activeExtension, initialExtensions, open, onClose }: NavRailProps) {
  const [extensions, setExtensions] = useState<ExtensionInfo[]>(initialExtensions ?? [])

  // Hooks run in a fixed order regardless of `open`, so the early return
  // below can never change which hooks mount.
  useEffect(() => {
    if (initialExtensions !== undefined) return // story/test seam: static demo state, no live fetch
    let cancelled = false
    api.listExtensions().then(exts => {
      if (!cancelled) setExtensions(exts)
    }).catch(() => {
      // Nav degrades to Chats/Memory only - an extensions-list failure
      // should never block the rest of the app from rendering.
    })
    return () => {
      cancelled = true
    }
  }, [initialExtensions])

  // An extension with no UI descriptor has nowhere to navigate to - an inert
  // entry is just noise in a nav drawer, so it's dropped entirely rather
  // than shown unclickable.
  const linkedExtensions = extensions.filter(ext => !!ext.href)

  const drawerPanelRef = useDrawer(open, onClose)

  if (!open) return null

  // z-50: above ChatList's z-40 (which needs it only for its own off-canvas
  // stacking below md) so the drawer isn't buried behind it at desktop widths.
  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/50"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        ref={drawerPanelRef}
        role="dialog"
        aria-modal="true"
        aria-label="Main navigation"
        className="absolute inset-y-0 left-0 w-64 max-w-[85vw] flex flex-col bg-white dark:bg-gray-800 border-r border-gray-200 dark:border-gray-700 shadow-lg"
      >
        <div className="flex items-center justify-between p-2 border-b border-gray-200 dark:border-gray-700">
          <span className="px-1.5 text-sm font-semibold text-gray-700 dark:text-gray-200">Navigation</span>
          <button
            onClick={onClose}
            aria-label="Close navigation"
            title="Close navigation"
            className="flex items-center justify-center w-11 h-11 rounded-lg text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
          >
            ✕
          </button>
        </div>
        <div className="flex-1 py-2 px-2 space-y-1 overflow-y-auto">
          <NavItem icon="💬" label="Chats" active={route === 'chat'} onClick={() => { navigate('/chat'); onClose() }} />
          <NavItem icon="🧠" label="Memory" active={route === 'memory'} onClick={() => { navigate('/memory'); onClose() }} />
          {linkedExtensions.length > 0 && (
            <div className="pt-1 mt-1 border-t border-gray-100 dark:border-gray-700 space-y-1">
              {linkedExtensions.map(ext => (
                <ExtensionNavItem key={ext.name} ext={ext} active={activeExtension === ext.name} onNavigate={onClose} />
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function NavItem({
  icon, label, active, onClick,
}: {
  icon: string
  label: string
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      onClick={onClick}
      aria-current={active ? 'page' : undefined}
      aria-label={label}
      className={`w-full flex items-center gap-2.5 rounded-lg px-2.5 py-2 min-h-[44px] text-sm transition-colors ${
        active
          ? 'bg-blue-50 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400 font-medium'
          : 'text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700'
      }`}
    >
      <span aria-hidden="true" className="text-base shrink-0 leading-none">{icon}</span>
      <span className="truncate">{label}</span>
    </button>
  )
}

function extensionIcon(ext: ExtensionInfo): string {
  return ext.icon ?? '🧩'
}

// ExtensionNavItem navigates client-side to this app's own /ext/:name host
// page (#870, ExtensionHost) rather than a real <a href> - that would leave
// the SPA behind entirely. The extension's own server route is still
// reachable directly; this is purely an in-app wrapper around it. Only
// href-bearing extensions ever reach this component - see linkedExtensions.
function ExtensionNavItem({ ext, active, onNavigate }: { ext: ExtensionInfo; active: boolean; onNavigate?: () => void }) {
  const label = ext.title ?? ext.name
  return (
    <button
      onClick={() => { navigate(`/ext/${encodeURIComponent(ext.name)}`); onNavigate?.() }}
      aria-current={active ? 'page' : undefined}
      aria-label={label}
      title={label}
      className={`w-full flex items-center gap-2.5 rounded-lg px-2.5 py-2 min-h-[44px] text-sm transition-colors ${
        active
          ? 'bg-blue-50 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400 font-medium'
          : 'text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700'
      }`}
    >
      <span aria-hidden="true" className="text-base shrink-0 leading-none">{extensionIcon(ext)}</span>
      <span className="truncate">{label}</span>
    </button>
  )
}
