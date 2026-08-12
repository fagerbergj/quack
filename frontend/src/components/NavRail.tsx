import { useEffect, useState } from 'react'
import { navigate, type Route } from '../router'
import { api, type ExtensionInfo } from '../api'

const STORAGE_KEY = 'navRailCollapsed'

function readCollapsed(): boolean {
  return localStorage.getItem(STORAGE_KEY) === '1'
}

export interface NavRailProps {
  route: Route
  // The extension name from a /ext/:name route (App.tsx's useExtName()) -
  // which extension entry, if any, is active. Unused outside route === 'ext'.
  activeExtension?: string
  // Storybook/test seam (same pattern as MemoryTab's initialState): seeds the
  // collapsed state directly instead of reading localStorage, so a story can
  // show expanded/collapsed deterministically regardless of browser state.
  initialCollapsed?: boolean
  // Storybook/test seam (same pattern as MemoryTab's initialState): pre-seeds
  // the extension nav entries and skips the live GET /api/v1/extensions fetch.
  initialExtensions?: ExtensionInfo[]
}

// NavRail (#746 item 1, fully collapsible per #759 item 1) is the app's
// persistent left rail: Chats and Memory as peers, text labels, collapsible
// to a slim icon strip. This is where Memory lives now - it replaces the old
// 🧠 button + ad-hoc /memory link in ChatList, and it is NOT in the per-chat
// overflow menu (that's Download Logs only, see ChatMenu). The
// collapsed/expanded choice persists across reload via localStorage - the
// same pattern App.tsx already uses for theme.
//
// Collapsed still renders a real <nav> (~40px, icon-only buttons + the
// expand chevron) - never nothing, so Chats/Memory/extensions stay reachable
// at any width instead of vanishing behind a 5px sliver (#870). Never
// auto-collapses/-expands on its own (hover, timer, etc.) - only the two
// explicit clicks below touch it.
export function NavRail({ route, activeExtension, initialCollapsed, initialExtensions }: NavRailProps) {
  const [collapsed, setCollapsed] = useState(initialCollapsed ?? readCollapsed)
  const [extensions, setExtensions] = useState<ExtensionInfo[]>(initialExtensions ?? [])

  useEffect(() => {
    localStorage.setItem(STORAGE_KEY, collapsed ? '1' : '0')
  }, [collapsed])

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
  // entry is just noise in a nav rail, so it's dropped entirely rather than
  // shown unclickable.
  const linkedExtensions = extensions.filter(ext => !!ext.href)

  if (collapsed) {
    return (
      <nav
        aria-label="Main"
        className="flex-shrink-0 w-10 h-full flex flex-col items-center border-r border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 py-2"
      >
        <div className="flex-1 w-full flex flex-col items-center gap-1 overflow-y-auto">
          <IconNavItem icon="💬" label="Chats" active={route === 'chat'} onClick={() => navigate('/chat')} />
          <IconNavItem icon="🧠" label="Memory" active={route === 'memory'} onClick={() => navigate('/memory')} />
          {linkedExtensions.length > 0 && (
            <div className="w-full pt-1 mt-1 border-t border-gray-100 dark:border-gray-700 flex flex-col items-center gap-1">
              {linkedExtensions.map(ext => (
                <IconExtensionNavItem key={ext.name} ext={ext} active={activeExtension === ext.name} />
              ))}
            </div>
          )}
        </div>
        <button
          onClick={() => setCollapsed(false)}
          aria-label="Expand navigation"
          title="Expand navigation"
          className="mt-1 flex items-center justify-center w-8 h-8 rounded-lg text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
        >
          <span aria-hidden="true">»</span>
        </button>
      </nav>
    )
  }

  return (
    <nav
      aria-label="Main"
      className="flex-shrink-0 w-40 h-full flex flex-col border-r border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800"
    >
      <div className="flex-1 py-2 px-2 space-y-1 overflow-y-auto">
        <NavItem icon="💬" label="Chats" active={route === 'chat'} onClick={() => navigate('/chat')} />
        <NavItem icon="🧠" label="Memory" active={route === 'memory'} onClick={() => navigate('/memory')} />
        {linkedExtensions.length > 0 && (
          <div className="pt-1 mt-1 border-t border-gray-100 dark:border-gray-700 space-y-1">
            {linkedExtensions.map(ext => (
              <ExtensionNavItem key={ext.name} ext={ext} active={activeExtension === ext.name} />
            ))}
          </div>
        )}
      </div>
      <div className="p-2 border-t border-gray-200 dark:border-gray-700">
        <button
          onClick={() => setCollapsed(true)}
          aria-label="Collapse navigation"
          title="Collapse navigation"
          className="w-full flex items-center justify-center rounded-lg p-2 text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
        >
          <span aria-hidden="true">«</span>
        </button>
      </div>
    </nav>
  )
}

function IconNavItem({
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
      title={label}
      className={`flex items-center justify-center w-8 h-8 rounded-lg text-base transition-colors ${
        active
          ? 'bg-blue-50 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400'
          : 'text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700'
      }`}
    >
      <span aria-hidden="true" className="leading-none">{icon}</span>
    </button>
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
      className={`w-full flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm transition-colors ${
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

// `icon` isn't in the generated ExtensionInfo type yet (a wire schema
// addition is landing separately) - read it defensively so this doesn't
// break the moment the field appears, falling back to the generic glyph.
function extensionIcon(ext: ExtensionInfo): string {
  return (ext as { icon?: string }).icon ?? '🧩'
}

// ExtensionNavItem navigates client-side to this app's own /ext/:name host
// page (#870, ExtensionHost) rather than a real <a href> - that would leave
// the SPA (and NavRail) behind entirely. The extension's own server route is
// still reachable directly; this is purely an in-app wrapper around it. Only
// href-bearing extensions ever reach this component - see linkedExtensions.
function ExtensionNavItem({ ext, active }: { ext: ExtensionInfo; active: boolean }) {
  const label = ext.title ?? ext.name
  return (
    <button
      onClick={() => navigate(`/ext/${encodeURIComponent(ext.name)}`)}
      aria-current={active ? 'page' : undefined}
      aria-label={label}
      title={label}
      className={`w-full flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm transition-colors ${
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

// Collapsed-strip counterpart of ExtensionNavItem - icon only, same
// client-side /ext/:name navigation.
function IconExtensionNavItem({ ext, active }: { ext: ExtensionInfo; active: boolean }) {
  const label = ext.title ?? ext.name
  return (
    <button
      onClick={() => navigate(`/ext/${encodeURIComponent(ext.name)}`)}
      aria-current={active ? 'page' : undefined}
      aria-label={label}
      title={label}
      className={`flex items-center justify-center w-8 h-8 rounded-lg text-base transition-colors ${
        active
          ? 'bg-blue-50 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400'
          : 'text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700'
      }`}
    >
      <span aria-hidden="true" className="leading-none">{extensionIcon(ext)}</span>
    </button>
  )
}
