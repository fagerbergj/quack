import { useId, useState } from 'react'
import { UsageSummary, type UsageSummaryProps } from './UsageSummary'
import { useTheme, type Theme } from '../hooks/useTheme'
import { Icon } from './Icon'
import { Sheet } from './Sheet'

const THEME_OPTIONS: { value: Theme; label: string }[] = [
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
  { value: 'system', label: 'System' },
]

// Per-chat actions only; Memory is a NavRail peer of Chats, not a per-chat action, so it does not belong here.
export function ChatMenu({ chatId, usage }: { chatId: string; usage?: UsageSummaryProps }) {
  const [open, setOpen] = useState(false)
  const sheetId = useId()
  const [theme, setTheme] = useTheme()

  return (
    <div className="relative flex-shrink-0">
      <button
        onClick={() => setOpen(o => !o)}
        popoverTarget={sheetId}
        popoverTargetAction="show"
        aria-label="Chat actions"
        aria-haspopup="menu"
        aria-expanded={open}
        title="Chat actions"
        className="min-w-[44px] min-h-[44px] flex items-center justify-center rounded text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
      >
        <Icon name="more_horiz" className="w-5 h-5" />
      </button>
      {open && (
        <Sheet id={sheetId} anchored="right" role="menu" aria-label="Chat actions" onClose={() => setOpen(false)} className="medium:w-44 medium:rounded-lg medium:border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 pt-1 medium:pb-1 text-sm medium:text-xs">
          {/* Shown only where the header's `hidden medium:flex` UsageSummary (Chat.tsx) is hidden. */}
          {usage && (usage.models.length > 0 || (usage.usage?.total_tokens ?? 0) > 0) && (
            <div className="px-3 py-1.5 border-b border-gray-100 dark:border-gray-700 medium:hidden">
              <UsageSummary {...usage} />
            </div>
          )}
          <a
            role="menuitem"
            href={`/api/v1/chats/${chatId}/recording`}
            onClick={() => setOpen(false)}
            title="Download this chat's full recording (every streamed event)"
            className="flex items-center gap-1.5 min-h-[44px] medium:min-h-0 px-3 py-1.5 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700"
          >
            <Icon name="download" className="w-3.5 h-3.5" /> Download Logs
          </a>
          {/* APG menuitemradio: selecting leaves the menu open so the user can change their mind without reopening. */}
          <div role="group" aria-label="Theme" className="border-t border-gray-100 dark:border-gray-700 mt-1 pt-1">
            {THEME_OPTIONS.map(opt => (
              <button
                key={opt.value}
                role="menuitemradio"
                aria-checked={theme === opt.value}
                onClick={() => setTheme(opt.value)}
                className="flex items-center gap-1.5 w-full min-h-[44px] medium:min-h-0 px-3 py-1.5 text-left text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700"
              >
                <span aria-hidden="true" className="w-3 inline-flex">{theme === opt.value ? <Icon name="check" className="w-3 h-3" /> : ''}</span> {opt.label}
              </button>
            ))}
          </div>
        </Sheet>
      )}
    </div>
  )
}
