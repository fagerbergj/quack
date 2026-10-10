import { useEffect, useState, type ReactNode } from 'react'
import { navigate, type Route } from '../router'
import { api, type ExtensionInfo } from '../api'
import { closeOnBackdrop, useDrawer } from '../hooks/useDrawer'
import { serverVersion } from '../state/clientConfig'
import { Icon, ICON_NAMES, type IconName } from './Icon'

// "dev" stays "dev"; anything else gets exactly one leading "v", even if the stamped value has one.
export function displayVersion(v: string): string {
  if (v === 'dev') return v
  return v.startsWith('v') ? v : `v${v}`
}

export interface NavRailProps {
  route: Route
  // The extension name from a /ext/:name route (App.tsx's useExtName()) -
  // which extension entry, if any, is active. Unused outside route === 'ext'.
  activeExtension?: string
  // Storybook/test seam (same pattern as MemoryTab's initialState): pre-seeds
  // the extension nav entries and skips the live GET /api/v1/extensions fetch.
  initialExtensions?: ExtensionInfo[]
  // Storybook/test seam: overrides the version footer instead of reading
  // the live clientConfig singleton (which resolves async off GET /api/v1/config).
  versionOverride?: string
  // false renders nothing; true mounts the overlay at every width. App.tsx owns the state, and nothing is
  // persisted, so the drawer is always closed on load.
  open: boolean
  onClose: () => void
}

// Off-canvas drawer at every width. Closes on item selection, backdrop tap, the close button or Esc;
// a native modal <dialog> via useDrawer.
export function NavRail({ route, activeExtension, initialExtensions, versionOverride, open, onClose }: NavRailProps) {
  const [extensions, setExtensions] = useState<ExtensionInfo[]>(initialExtensions ?? [])
  const version = versionOverride ?? serverVersion()

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

  // An extension with no UI descriptor has nowhere to navigate, so it is dropped rather than shown inert.
  const linkedExtensions = extensions.filter(ext => !!ext.href)

  const dialogRef = useDrawer(open)

  if (!open) return null

  return (
    <dialog
      ref={dialogRef}
      aria-label="Main navigation"
      onClose={onClose}
      onClick={closeOnBackdrop}
      className="m-0 h-dvh max-h-none w-64 max-w-[85vw] p-0 open:flex flex-col bg-white dark:bg-gray-800 border-0 border-r border-gray-200 dark:border-gray-700 shadow-lg backdrop:bg-black/50"
    >
      <div className="flex items-center justify-between p-2 border-b border-gray-200 dark:border-gray-700">
        <span className="px-1.5 text-sm font-semibold text-gray-700 dark:text-gray-200">Navigation</span>
        <button
          onClick={onClose}
          aria-label="Close navigation"
          title="Close navigation"
          className="flex items-center justify-center w-11 h-11 rounded-lg text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
        >
          <Icon name="close" className="w-4 h-4" />
        </button>
      </div>
      <div className="flex-1 py-2 px-2 space-y-1 overflow-y-auto">
        <NavItem icon={<Icon name="chat" className="w-4 h-4" />} label="Chats" active={route === 'chat'} onClick={() => { navigate('/chat'); onClose() }} />
        <NavItem icon={<Icon name="lightbulb" className="w-4 h-4" />} label="Memory" active={route === 'memory'} onClick={() => { navigate('/memory'); onClose() }} />
        <NavItem icon={<Icon name="extension" className="w-4 h-4" />} label="Plugins" active={route === 'plugins'} onClick={() => { navigate('/plugins'); onClose() }} />
        {linkedExtensions.length > 0 && (
          <div className="pt-1 mt-1 border-t border-gray-100 dark:border-gray-700 space-y-1">
            {linkedExtensions.map(ext => (
              <ExtensionNavItem key={ext.name} ext={ext} active={activeExtension === ext.name} onNavigate={onClose} />
            ))}
          </div>
        )}
      </div>
      {version && (
        <div className="shrink-0 px-3 py-1.5">
          <span className="text-[11px] text-gray-500 dark:text-gray-400" title={version}>
            {displayVersion(version)}
          </span>
        </div>
      )}
    </dialog>
  )
}

function NavItem({
  icon, label, active, onClick,
}: {
  icon: ReactNode
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
      <span aria-hidden="true" className="shrink-0 leading-none flex items-center">{icon}</span>
      <span className="truncate">{label}</span>
    </button>
  )
}

// Inline <svg> is injected raw because the extension registry is trusted (same boundary as its href/title);
// anything not a known icon name or SVG, such as an emoji, falls back to the generic glyph.
const warnedUnknownIcons = new Set<string>()

function extensionIcon(ext: ExtensionInfo): ReactNode {
  const icon = ext.icon
  if (icon && ICON_NAMES.has(icon)) return <Icon name={icon as IconName} className="w-4 h-4" />
  if (icon && icon.trim().startsWith('<svg')) {
    return <span className="w-4 h-4 [&>svg]:w-4 [&>svg]:h-4" dangerouslySetInnerHTML={{ __html: icon }} />
  }
  // Named but unrecognized icon: warn once so a new extension icon name gets
  // noticed and added to Icon.tsx's PATHS, instead of silently staying generic.
  if (icon && !warnedUnknownIcons.has(icon)) {
    warnedUnknownIcons.add(icon)
    console.warn(`NavRail: unknown extension icon "${icon}", falling back to the generic icon`)
  }
  return <Icon name="extension" className="w-4 h-4" />
}

// Navigates client-side to the /ext/:name host page; a real <a href> would leave the SPA.
// Only href-bearing extensions reach this (see linkedExtensions).
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
