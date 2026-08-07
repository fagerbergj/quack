import { useEffect, useState } from 'react'
import { navigate, type Route } from '../router'

const STORAGE_KEY = 'navRailCollapsed'

function readCollapsed(): boolean {
  return localStorage.getItem(STORAGE_KEY) === '1'
}

export interface NavRailProps {
  route: Route
  // Storybook/test seam (same pattern as MemoryTab's initialState): seeds the
  // collapsed state directly instead of reading localStorage, so a story can
  // show expanded/collapsed deterministically regardless of browser state.
  initialCollapsed?: boolean
}

// NavRail (#746 item 1, fully collapsible per #759 item 1) is the app's
// persistent left rail: Chats and Memory as peers, text labels, collapsible
// to nothing. This is where Memory lives now - it replaces the old 🧠 button
// + ad-hoc /memory link in ChatList, and it is NOT in the per-chat overflow
// menu (that's Download Logs only, see ChatMenu). The collapsed/expanded
// choice persists across reload via localStorage - the same pattern App.tsx
// already uses for theme.
//
// Collapsed renders no rail at all - not even an icon strip - so it takes no
// width in the flex layout; a `fixed` restore button (real <button>, focusable,
// named) is the only thing left on screen. Never auto-collapses/-expands on
// its own (hover, timer, etc.) - only the two explicit clicks below touch it.
export function NavRail({ route, initialCollapsed }: NavRailProps) {
  const [collapsed, setCollapsed] = useState(initialCollapsed ?? readCollapsed)

  useEffect(() => {
    localStorage.setItem(STORAGE_KEY, collapsed ? '1' : '0')
  }, [collapsed])

  if (collapsed) {
    return (
      <button
        onClick={() => setCollapsed(false)}
        aria-label="Expand navigation"
        title="Expand navigation"
        className="fixed left-0 top-1/2 -translate-y-1/2 z-10 flex items-center justify-center w-5 h-14 rounded-r-lg border border-l-0 border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 shadow-sm transition-colors"
      >
        <span aria-hidden="true">»</span>
      </button>
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
