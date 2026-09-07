import { Icon } from './Icon'

// NavToggle (#1171) is the single trigger for the navigation drawer,
// sitting in each page's header leading slot (Chat, Memory, ExtensionHost).
// 44x44 (w-11 h-11) tap target, and its own glyph - a grid icon that is
// never the chat-list toggle's menu icon (#1175: the app had two identical
// hamburgers; there is now exactly one). The drawer's open state is owned by
// App.tsx, which hands it down as props; aria-expanded mirrors it.
export interface NavToggleProps {
  open: boolean
  onToggle: () => void
}

export function NavToggle({ open, onToggle }: NavToggleProps) {
  return (
    <button
      onClick={onToggle}
      aria-label="Toggle navigation"
      aria-haspopup="dialog"
      aria-expanded={open}
      title="Toggle navigation"
      className="flex-shrink-0 w-11 h-11 flex items-center justify-center rounded-lg text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
    >
      <Icon name="menu_grid" className="w-5 h-5" />
    </button>
  )
}
