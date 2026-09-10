import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { AttachmentStrip, type AttachmentItem } from './AttachmentUI'
import { useMediaQuery } from '../hooks/useMediaQuery'
import type { QueuedTurn } from '../state/chatStore'

interface AttachmentPreview {
  url: string
  mime: string
  name: string
}

// Inline SVG paths, matching the icon style already used elsewhere (e.g.
// CopyButton) - no icon-library dependency for three glyphs.
function PaperclipIcon() {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48" />
    </svg>
  )
}

function StopIcon() {
  return (
    <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor" aria-hidden="true">
      <rect x="5" y="5" width="14" height="14" rx="2" />
    </svg>
  )
}

function SendIcon() {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" fill="currentColor" aria-hidden="true">
      <path d="M3 20l18-8L3 4v6l12 2-12 2z" />
    </svg>
  )
}

// Below Tailwind's sm breakpoint (640px) - matches where the app's other
// sm: utilities switch, and comfortably covers the 390px phones this was
// found on.
const NARROW_QUERY = '(max-width: 639px)'

// The parenthetical on both the idle and streaming placeholders is
// meaningless on a touch device and, at phone widths, wraps to a second line
// the fixed-height input clips (#759 item 2) - so it's dropped below the breakpoint rather than fought with layout.
function useNarrowViewport(): boolean {
  const supported = typeof window !== 'undefined' && typeof window.matchMedia === 'function'
  const [narrow, setNarrow] = useState(() => supported && window.matchMedia(NARROW_QUERY).matches)
  useEffect(() => {
    if (!supported) return
    const mql = window.matchMedia(NARROW_QUERY)
    const onChange = () => setNarrow(mql.matches)
    mql.addEventListener('change', onChange)
    return () => mql.removeEventListener('change', onChange)
  }, [supported])
  return narrow
}

function placeholderFor(disabled: boolean, streaming: boolean, narrow: boolean, archived: boolean): string {
  if (archived) return 'Archived chats are read-only - restore to continue'
  if (disabled) return 'Select or start a chat first'
  if (streaming) return narrow ? 'Type a follow-up…' : 'Type a follow-up… (queues until the current response finishes)'
  return narrow ? 'Ask something…' : 'Ask something… (Enter to send, Shift+Enter for newline)'
}

export interface ComposerProps {
  // No active chat, or the active chat is archived - input is disabled.
  disabled: boolean
  // A turn is streaming - input stays live and Send queues instead of running
  // a second turn; Stop appears alongside it to cancel the active run.
  streaming: boolean
  onSubmit: (text: string, files: File[], previews: AttachmentPreview[]) => void
  onStop: () => void
  // Follow-ups queued while streaming, in send order - rendered as pending
  // rows above the input; empty/omitted when nothing is queued.
  queue?: QueuedTurn[]
  onRemoveQueued?: (id: string) => void
  // Distinguishes an archived chat's disabled composer (a specific, expected
  // read-only state) from the generic "no chat selected" disabled placeholder.
  archived?: boolean
}

// Composer owns the draft `input` + `attachments` locally so typing only re-renders
// this small component, not the whole chat (the turn list / DAG trees). The
// finished message goes up via onSubmit - the caller decides whether that's an immediate send or (while streaming) queuing it for after the current run.
export function Composer({ disabled, streaming, onSubmit, onStop, queue, onRemoveQueued, archived = false }: ComposerProps) {
  const [input, setInput] = useState('')
  const [attachments, setAttachments] = useState<AttachmentItem[]>([])
  const fileInputRef = useRef<HTMLInputElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const narrow = useNarrowViewport()
  // #1174: the compact (<600px) branch swaps decoration/subtree (icon buttons,
  // pill row, queued chip) rather than just resizing, so it's a JS branch -
  // the wrapper's pure resize stays CSS via the `medium:` breakpoint. Below the 600px `medium` size class (#1145); useCompact was deleted in #1189.
  const compact = useMediaQuery('(max-width: 599px)')

  // Auto-grow the textarea with its content (CSS field-sizing isn't in Firefox/
  // Safari yet). Reset to auto first so it shrinks back when the draft is
  // cleared; capped at MAX_HEIGHT_PX (#1174). #425: overflow-y is toggled in JS rather than a permanent Tailwind class - an always-on `overflow-y-auto` renders a Chromium scrollbar track even on a single empty line.
  const MAX_HEIGHT_PX = compact ? 128 : 192
  useLayoutEffect(() => {
    const ta = textareaRef.current
    if (!ta) return
    ta.style.height = 'auto'
    const overflowing = ta.scrollHeight > MAX_HEIGHT_PX
    ta.style.height = `${Math.min(ta.scrollHeight, MAX_HEIGHT_PX)}px`
    ta.style.overflowY = overflowing ? 'auto' : 'hidden'
  }, [input, compact])

  function submit() {
    const trimmed = input.trim()
    if ((!trimmed && attachments.length === 0) || disabled) return
    const items = attachments.slice()
    const previews = items.map(a => ({ url: a.url, mime: a.file.type, name: a.file.name }))
    setInput('')
    setAttachments([])
    onSubmit(trimmed, items.map(a => a.file), previews)
  }

  function handleKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      submit()
    }
  }

  return (
    // #1248 follow-up: floating pill, not a full-width bar - no bg/border here,
    // the pill surface below carries its own bg/shadow. Bottom offset is the
    // shared --composer-gap (index.css); don't add another safe-area-inset read here - that doubling caused the pre-#1249 excess.
    <div className="px-3 pt-2 pb-[var(--composer-gap)] medium:px-6 medium:pt-3">
      {queue != null && queue.length > 0 && (
        compact ? (
          // #1174: a row per queued bubble stacks on top of the 60px budget -
          // one "N queued" chip instead. <details> is DOM-handled, so the chip
          // stays open across queue additions without re-rendering the composer (same disclosure pattern as TriggerEnvelope); remove is always visible because touch has no hover.
          <details className="mb-3">
            <summary className="list-none w-fit cursor-pointer select-none px-3 py-1.5 text-xs font-medium text-gray-600 dark:text-gray-300 hover:text-gray-800 dark:hover:text-gray-100 rounded-full ring-1 ring-gray-300 dark:ring-gray-600 bg-white dark:bg-gray-700">
              {`${queue.length} queued`}
            </summary>
            <div className="flex flex-col gap-2 mt-2">
              {queue.map(item => (
                <div key={item.id} className="flex justify-end">
                  <div className="max-w-2xl ml-auto">
                    <div className="bg-gray-200 dark:bg-gray-700 text-gray-600 dark:text-gray-300 rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
                      {item.text}
                    </div>
                    <div className="flex items-center justify-end gap-2 mt-0.5 pr-1 text-[11px] uppercase tracking-wide text-gray-500 dark:text-gray-400">
                      <span>queued</span>
                      {onRemoveQueued && (
                        <button
                          type="button"
                          onClick={() => onRemoveQueued(item.id)}
                          aria-label="Remove queued message"
                          title="Remove"
                          className="min-h-[44px] -my-2 inline-flex items-center hover:text-red-500 dark:hover:text-red-400 transition-opacity normal-case"
                        >
                          remove
                        </button>
                      )}
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </details>
        ) : (
          <div className="flex flex-col gap-2 mb-3" aria-label="Queued messages">
            {queue.map(item => (
              // Looks like the user's own message bubble, just grayed out with a
              // "queued" hint - not a separate pill design.
              <div key={item.id} className="group flex justify-end">
                <div className="max-w-2xl ml-auto">
                  <div className="bg-gray-200 dark:bg-gray-700 text-gray-600 dark:text-gray-300 rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
                    {item.text}
                  </div>
                  <div className="flex items-center justify-end gap-2 mt-0.5 pr-1 text-[11px] uppercase tracking-wide text-gray-500 dark:text-gray-400">
                    <span>queued</span>
                    {onRemoveQueued && (
                      <button
                        type="button"
                        onClick={() => onRemoveQueued(item.id)}
                        aria-label="Remove queued message"
                        title="Remove"
                        className="min-h-[44px] -my-2 inline-flex items-center opacity-0 group-hover:opacity-100 hover:text-red-500 dark:hover:text-red-400 transition-opacity normal-case"
                      >
                        remove
                      </button>
                    )}
                  </div>
                </div>
              </div>
            ))}
          </div>
        )
      )}
      <form onSubmit={e => { e.preventDefault(); submit() }} className="flex flex-col gap-2">
        <AttachmentStrip
          attachments={attachments}
          onRemove={i => setAttachments(prev => {
            if (prev[i].url) URL.revokeObjectURL(prev[i].url)
            return prev.filter((_, j) => j !== i)
          })}
        />
        {/* #1248: one floating pill surface at every width - shadow +
        ring instead of a full-width bar/border, so it reads as a control
        floating over the chat rather than a docked toolbar. #1174: ring
        (box-shadow) rather than a border on the compact pill so the 44px
        row box doesn't grow 2px. */}
        <div className={compact
          ? 'flex items-center gap-2 rounded-full ring-1 ring-gray-300 dark:ring-gray-600 bg-white dark:bg-gray-800 shadow-lg'
          : 'flex gap-2 items-end rounded-3xl ring-1 ring-gray-200 dark:ring-gray-700 bg-white dark:bg-gray-800 shadow-lg p-2'}>
          <input
            ref={fileInputRef}
            type="file"
            accept="image/*,audio/*"
            multiple
            className="hidden"
            onChange={e => {
              if (e.target.files) {
                const items: AttachmentItem[] = Array.from(e.target.files).map(f => ({
                  file: f,
                  url: URL.createObjectURL(f),
                }))
                setAttachments(prev => [...prev, ...items])
              }
              e.target.value = ''
            }}
          />
          <button
            type="button"
            onClick={() => fileInputRef.current?.click()}
            disabled={streaming || disabled}
            className={compact
              ? 'h-11 w-11 flex-shrink-0 flex items-center justify-center rounded-full text-gray-500 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-gray-600 disabled:opacity-50 disabled:cursor-not-allowed transition-colors'
              : 'h-11 w-11 flex-shrink-0 flex items-center justify-center rounded-xl text-gray-500 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-gray-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors'}
            aria-label="Attach file"
            title="Attach image or audio"
          >
            <PaperclipIcon />
          </button>
          <textarea
            ref={textareaRef}
            id="composer-input"
            name="message"
            // placeholder:truncate is the layout-proof half of #759 item 2: the
            // narrow-viewport text above is the common case, but this stops any
            // placeholder from ever wrapping into a clipped second line, at any width - e.g. mid-stream, when Stop+Queue also compete for the row's space.
            className={compact
              ? 'flex-1 min-w-0 bg-transparent px-4 py-2 text-base focus:outline-none focus:ring-2 focus:ring-blue-500 resize-none max-h-32 disabled:opacity-50 dark:text-gray-100 dark:placeholder-gray-400 placeholder:truncate'
              : 'flex-1 min-w-0 bg-transparent px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 rounded-xl resize-none max-h-48 disabled:opacity-50 dark:text-gray-100 dark:placeholder-gray-400 placeholder:truncate'}
            rows={1}
            placeholder={placeholderFor(disabled, streaming, narrow, archived)}
            value={input}
            onChange={e => setInput(e.target.value)}
            onKeyDown={handleKeyDown}
            disabled={disabled}
          />
          {/* Red fill: while a run is live, stopping it is the primary action
              (audit #6) - Send/Queue is the secondary one. */}
          {streaming && (
            <button
              type="button"
              onClick={onStop}
              aria-label="Stop"
              className={compact
                ? 'h-11 w-11 flex-shrink-0 flex items-center justify-center rounded-full bg-red-600 text-white hover:bg-red-700 transition-colors'
                : 'h-11 w-11 flex-shrink-0 flex items-center justify-center rounded-xl bg-red-600 text-white hover:bg-red-700 transition-colors'}
            >
              <StopIcon />
            </button>
          )}
          <button
            type="submit"
            disabled={(!input.trim() && attachments.length === 0) || disabled}
            aria-label={streaming ? 'Queue' : 'Send'}
            className={compact
              ? 'h-11 w-11 flex-shrink-0 flex items-center justify-center rounded-full bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors'
              : 'h-11 w-11 flex-shrink-0 flex items-center justify-center rounded-xl bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors'}
          >
            <SendIcon />
          </button>
        </div>
      </form>
    </div>
  )
}
