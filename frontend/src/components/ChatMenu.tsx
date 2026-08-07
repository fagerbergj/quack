import { useEffect, useRef, useState } from 'react'

// ChatMenu is the chat header's ⋯ overflow menu (#746 items 2/3): per-chat
// actions that aren't worth permanent header real estate. Today that's just
// Download Logs (the `⬇ recording` link, relabelled and moved here - it stays
// a plain link to the same endpoint, only label and placement change). It
// does NOT hold Memory - Memory is a NavRail peer of Chats, not a per-chat
// action. Same disclosure pattern as DagNode's NodeMenu: a button that toggles
// a role="menu" popover, closed on outside click or Escape.
export function ChatMenu({ chatId }: { chatId: string }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

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
        className="w-7 h-7 flex items-center justify-center rounded text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
      >
        ⋯
      </button>
      {open && (
        <div role="menu" className="absolute z-20 right-0 mt-1 w-44 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-lg py-1 text-xs">
          <a
            role="menuitem"
            href={`/api/v1/chats/${chatId}/recording`}
            onClick={() => setOpen(false)}
            title="Download this chat's full recording (every streamed event)"
            className="flex items-center gap-1.5 px-3 py-1.5 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700"
          >
            <span aria-hidden="true">⬇</span> Download Logs
          </a>
        </div>
      )}
    </div>
  )
}
