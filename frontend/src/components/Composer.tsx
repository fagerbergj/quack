import { useLayoutEffect, useRef, useState } from 'react'
import { AttachmentStrip, type AttachmentItem } from './AttachmentUI'
import { useMediaQuery } from '../hooks/useMediaQuery'
import { Icon } from './Icon'
import { Button } from './ui/button'
import { Textarea } from './ui/input'
import { Tooltip } from './ui/tooltip'
import type { QueuedTurn } from '../state/chatStore'

interface AttachmentPreview {
  url: string
  mime: string
  name: string
}

function StopIcon() {
  return (
    <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor" aria-hidden="true">
      <rect x="5" y="5" width="14" height="14" rx="2" />
    </svg>
  )
}

// Below Tailwind's sm breakpoint (640px), where the app's other sm: utilities switch.
const NARROW_QUERY = '(max-width: 639px)'

// The placeholders' parenthetical is meaningless on touch and wraps into a
// second line the fixed-height input clips, so it's dropped when narrow.
function placeholderFor(streaming: boolean, narrow: boolean, archived: boolean, noChat: boolean): string {
  if (archived) return 'Archived - restore it to reply'
  if (noChat) return 'Ask a question'
  if (streaming) return narrow ? 'Type a follow-up…' : 'Type a follow-up… (queues until the current response finishes)'
  return narrow ? 'Ask something…' : 'Ask something… (Enter to send, Shift+Enter for newline)'
}

export interface ComposerProps {
  // Archived chat only. No active chat is NOT disabled: the first send creates the chat.
  disabled: boolean
  // Input stays live while streaming; Send queues instead of running a second turn.
  streaming: boolean
  // A rejected return restores the draft (input + attachments) instead of losing it.
  onSubmit: (text: string, files: File[], previews: AttachmentPreview[]) => void | Promise<void>
  onStop: () => void
  queue?: QueuedTurn[]
  onRemoveQueued?: (id: string) => void
  archived?: boolean
  // Empty /chat route: composer stays enabled and the placeholder invites the first message.
  noChat?: boolean
}

// Composer owns the draft locally so typing re-renders only this component, not the whole chat.
function QueuedMessages({ queue, compact, onRemoveQueued }: {
  queue: QueuedTurn[]
  compact: boolean
  onRemoveQueued?: (id: string) => void
}) {
  if (queue.length === 0) return null
  return compact ? (
    // A row per queued bubble would blow the compact height budget. <details> stays open across queue
    // additions without re-rendering; remove is always visible because touch has no hover.
    <details className="mb-3">
      <summary className="list-none w-fit cursor-pointer select-none px-3 py-2 text-xs font-medium text-gray-600 dark:text-gray-300 hover:text-gray-800 dark:hover:text-gray-100 rounded-full ring-1 ring-gray-300 dark:ring-gray-600 bg-white dark:bg-gray-700">
        {`${queue.length} queued`}
      </summary>
      <div className="flex flex-col gap-2 mt-2">
        {queue.map(item => (
          <div key={item.id} className="flex justify-end">
            <div className="max-w-2xl ml-auto">
              <div className="bg-gray-200 dark:bg-gray-700 text-gray-600 dark:text-gray-300 rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
                {item.text}
              </div>
              <div className="flex items-center justify-end gap-2 mt-0.5 pr-1 text-2xs text-gray-500 dark:text-gray-400">
                <span>Queued</span>
                {onRemoveQueued && (
                  <Button
                    variant="ghost"
                    size="inline"
                    onClick={() => onRemoveQueued(item.id)}
                    aria-label="Remove queued message"
                    title="Remove"
                    className="-my-2 text-2xs normal-case hover:bg-transparent hover:text-destructive"
                  >
                    remove
                  </Button>
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
        <div key={item.id} className="group flex justify-end">
          <div className="max-w-2xl ml-auto">
            <div className="bg-gray-200 dark:bg-gray-700 text-gray-600 dark:text-gray-300 rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
              {item.text}
            </div>
            <div className="flex items-center justify-end gap-2 mt-0.5 pr-1 text-2xs text-gray-500 dark:text-gray-400">
              <span>Queued</span>
              {onRemoveQueued && (
                <Button
                  variant="ghost"
                  size="inline"
                  onClick={() => onRemoveQueued(item.id)}
                  aria-label="Remove queued message"
                  title="Remove"
                  className="-my-2 text-2xs opacity-0 group-hover:opacity-100 focus-visible:opacity-100 normal-case hover:bg-transparent hover:text-destructive transition-opacity"
                >
                  remove
                </Button>
              )}
            </div>
          </div>
        </div>
      ))}
    </div>
  )
}

export function Composer({ disabled, streaming, onSubmit, onStop, queue, onRemoveQueued, archived = false, noChat = false }: ComposerProps) {
  const [input, setInput] = useState('')
  const [attachments, setAttachments] = useState<AttachmentItem[]>([])
  const fileInputRef = useRef<HTMLInputElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const narrow = useMediaQuery(NARROW_QUERY)
  // Compact swaps subtrees (icon buttons, queued chip), not just sizes, so it is a JS branch; pure resizes
  // stay CSS via `medium:`.
  const compact = useMediaQuery('(max-width: 599px)')

  // CSS field-sizing isn't in Firefox/Safari yet. overflow-y is toggled here because an always-on
  // `overflow-y-auto` shows a Chromium scrollbar track even on one empty line.
  const MAX_HEIGHT_PX = compact ? 128 : 192
  useLayoutEffect(() => {
    const ta = textareaRef.current
    if (!ta) return
    ta.style.height = 'auto'
    const overflowing = ta.scrollHeight > MAX_HEIGHT_PX
    ta.style.height = `${Math.min(ta.scrollHeight, MAX_HEIGHT_PX)}px`
    ta.style.overflowY = overflowing ? 'auto' : 'hidden'
  }, [input, compact])

  // Clears the draft immediately but restores it if onSubmit rejects (e.g. chat-create fails).
  function submit() {
    const trimmed = input.trim()
    if ((!trimmed && attachments.length === 0) || disabled) return
    const items = attachments.slice()
    const previews = items.map(a => ({ url: a.url, mime: a.file.type, name: a.file.name }))
    setInput('')
    setAttachments([])
    const result = onSubmit(trimmed, items.map(a => a.file), previews)
    result?.catch(() => {
      // Only restore into a still-empty draft: a slow/failing submit gives
      // the user seconds to type again, and a blind restore would clobber it.
      setInput(cur => (cur === '' ? trimmed : cur))
      setAttachments(cur => (cur.length === 0 ? items : cur))
    })
  }

  function handleKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      submit()
    }
  }

  return (
    // --composer-gap (index.css) already includes the safe-area inset; reading it again here doubles
    // the bottom gap.
    <div className="px-3 pt-2 pb-[var(--composer-gap)] medium:px-6 medium:pt-3">
      {queue != null && (
        <QueuedMessages queue={queue} compact={compact} onRemoveQueued={onRemoveQueued} />
      )}
      <form onSubmit={e => { e.preventDefault(); submit() }} className="flex flex-col gap-2">
        <AttachmentStrip
          attachments={attachments}
          onRemove={i => setAttachments(prev => {
            if (prev[i].url) URL.revokeObjectURL(prev[i].url)
            return prev.filter((_, j) => j !== i)
          })}
        />
        {/* Ring (box-shadow), not border, so the compact 44px row doesn't grow 2px. */}
        <div className={compact
          ? 'flex items-center gap-2 rounded-full ring-1 ring-border bg-card shadow-overlay'
          : 'flex gap-2 items-end rounded-3xl ring-1 ring-border bg-card shadow-overlay p-2'}>
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
          <Tooltip content="Attach image or audio">
            <Button
              variant="ghost"
              size="icon"
              onClick={() => fileInputRef.current?.click()}
              disabled={streaming || disabled}
              className={compact ? 'size-11 medium:size-11 rounded-full' : 'size-11 medium:size-11 rounded-xl'}
              aria-label="Attach file"
            >
              <Icon name="attach" className="w-5 h-5" />
            </Button>
          </Tooltip>
          <Textarea
            ref={textareaRef}
            id="composer-input"
            name="message"
            // placeholder:truncate stops any placeholder wrapping into a clipped second line, e.g. mid-stream
            // when Stop and Queue also compete for the row.
            className={compact
              ? 'flex-1 min-h-0 border-0 bg-transparent px-4 py-2 resize-none max-h-32 placeholder:truncate'
              : 'flex-1 min-h-0 border-0 bg-transparent px-4 py-3 resize-none max-h-48 placeholder:truncate'}
            rows={1}
            placeholder={placeholderFor(streaming, narrow, archived, noChat)}
            value={input}
            onChange={e => setInput(e.target.value)}
            onKeyDown={handleKeyDown}
            disabled={disabled}
          />
          {/* Red fill: while a run is live, stopping it is the primary action. */}
          {streaming && (
            <Button
              variant="destructive"
              size="icon"
              onClick={onStop}
              aria-label="Stop"
              className={compact ? 'size-11 medium:size-11 rounded-full' : 'size-11 medium:size-11 rounded-xl'}
            >
              <StopIcon />
            </Button>
          )}
          <Button
            type="submit"
            size="icon"
            disabled={(!input.trim() && attachments.length === 0) || disabled}
            aria-label={streaming ? 'Queue' : 'Send'}
            className={compact ? 'size-11 medium:size-11 rounded-full' : 'size-11 medium:size-11 rounded-xl'}
          >
            <Icon name="send" className="w-5 h-5" />
          </Button>
        </div>
      </form>
    </div>
  )
}
