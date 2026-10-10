import { Icon } from './Icon'

// The single drawer trigger, in each page header. Its grid glyph must never match the chat-list toggle's
// menu icon, so the app never shows two identical hamburgers.
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
