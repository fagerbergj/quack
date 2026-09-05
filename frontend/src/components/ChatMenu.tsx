import { useEffect, useRef, useState } from 'react'
import { UsageSummary, type UsageSummaryProps } from './UsageSummary'
import { useTheme, type Theme } from '../hooks/useTheme'

const THEME_OPTIONS: { value: Theme; label: string }[] = [
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
  { value: 'system', label: 'System' },
]

// ChatMenu is the chat header's ⋯ overflow menu (#746 items 2/3): per-chat
// actions that aren't worth permanent header real estate. Today that's
// Download Logs (the `⬇ recording` link, relabelled and moved here - it stays
// a plain link to the same endpoint, only label and placement change), plus -
// on compact width (#1136) - the token/model usage summary that the header
// itself hides there to give the title its width back. It does NOT hold
// Memory - Memory is a NavRail peer of Chats, not a per-chat action. Same
// disclosure pattern as DagNode's NodeMenu: a button that toggles a
// role="menu" popover, closed on outside click or Escape.
export function ChatMenu({ chatId, usage }: { chatId: string; usage?: UsageSummaryProps }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  const [theme, setTheme] = useTheme()

  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false) }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  return (
    <div ref={ref} className="relative flex-shrink-0">
      <button
        onClick={() => setOpen(o => !o)}
        aria-label="Chat actions"
        aria-haspopup="menu"
        aria-expanded={open}
        title="Chat actions"
        className="min-w-[44px] min-h-[44px] flex items-center justify-center rounded text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
      >
        ⋯
      </button>
      {open && (
        <div role="menu" className="absolute z-20 right-0 mt-1 w-44 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-lg py-1 text-xs">
          {/* Shown here always when the header itself hides the inline
              UsageSummary (compact width, `hidden medium:flex` in Chat.tsx) -
              the header stays the source of truth for whether it's shown
              inline; this is just the escape hatch when it isn't. */}
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
            className="flex items-center gap-1.5 px-3 py-1.5 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700"
          >
            <span aria-hidden="true">⬇</span> Download Logs
          </a>
          {/* #1173: Light/Dark/System - only in-app way to change theme.
              APG menuitemradio: activating changes the selection but leaves
              the menu open (unlike Download Logs above), so a user can
              change their mind without reopening. */}
          <div role="group" aria-label="Theme" className="border-t border-gray-100 dark:border-gray-700 mt-1 pt-1">
            {THEME_OPTIONS.map(opt => (
              <button
                key={opt.value}
                role="menuitemradio"
                aria-checked={theme === opt.value}
                onClick={() => setTheme(opt.value)}
                className="flex items-center gap-1.5 w-full px-3 py-1.5 text-left text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700"
              >
                <span aria-hidden="true" className="w-3">{theme === opt.value ? '✓' : ''}</span> {opt.label}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
