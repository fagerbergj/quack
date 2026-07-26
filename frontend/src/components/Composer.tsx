import { useLayoutEffect, useRef, useState } from 'react'
import { AttachmentStrip, type AttachmentItem } from './AttachmentUI'
import type { QueuedTurn } from '../state/chatStore'

export interface AttachmentPreview {
  url: string
  mime: string
  name: string
}

export interface ComposerProps {
  // No active chat - input is disabled.
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
}

// Composer owns the draft `input` + `attachments` locally so typing only re-renders
// this small component, not the whole chat (the turn list / DAG trees). It hands the
// finished message up via onSubmit - the caller decides whether that's an immediate
// send or (while streaming) queuing it for after the current run.
export function Composer({ disabled, streaming, onSubmit, onStop, queue, onRemoveQueued }: ComposerProps) {
  const [input, setInput] = useState('')
  const [attachments, setAttachments] = useState<AttachmentItem[]>([])
  const fileInputRef = useRef<HTMLInputElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  // Auto-grow the textarea with its content (CSS field-sizing isn't in Firefox/
  // Safari yet). Reset to auto first so it shrinks back when the draft is cleared;
  // capped at MAX_HEIGHT_PX (matches max-h-48). #425: overflow-y is toggled in JS
  // rather than left as a permanent Tailwind class - an always-on `overflow-y-auto`
  // renders a vertical scrollbar even on a single empty line in Chromium, since the
  // scrollbar reserves its track regardless of whether content actually overflows.
  const MAX_HEIGHT_PX = 192
  useLayoutEffect(() => {
    const ta = textareaRef.current
    if (!ta) return
    ta.style.height = 'auto'
    const overflowing = ta.scrollHeight > MAX_HEIGHT_PX
    ta.style.height = `${Math.min(ta.scrollHeight, MAX_HEIGHT_PX)}px`
    ta.style.overflowY = overflowing ? 'auto' : 'hidden'
  }, [input])

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
    // pb adds env(safe-area-inset-bottom) on top of the normal py-4 so the
    // composer clears the home indicator on notched phones instead of
    // sitting flush under it.
    <div className="border-t border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 px-6 pt-4 pb-[calc(1rem+env(safe-area-inset-bottom))]">
      {queue != null && queue.length > 0 && (
        <div className="flex flex-col gap-2 mb-3" aria-label="Queued messages">
          {queue.map(item => (
            // Looks like the user's own message bubble, just grayed out with a
            // "queued" hint - not a separate pill design.
            <div key={item.id} className="group flex justify-end">
              <div className="max-w-2xl ml-auto">
                <div className="bg-gray-200 dark:bg-gray-700 text-gray-500 dark:text-gray-400 rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
                  {item.text}
                </div>
                <div className="flex items-center justify-end gap-2 mt-0.5 pr-1 text-[10px] uppercase tracking-wide text-gray-400 dark:text-gray-500">
                  <span>queued</span>
                  {onRemoveQueued && (
                    <button
                      type="button"
                      onClick={() => onRemoveQueued(item.id)}
                      aria-label="Remove queued message"
                      title="Remove"
                      className="opacity-0 group-hover:opacity-100 hover:text-red-500 dark:hover:text-red-400 transition-opacity normal-case"
                    >
                      remove
                    </button>
                  )}
                </div>
              </div>
            </div>
          ))}
        </div>
      )}
      <form onSubmit={e => { e.preventDefault(); submit() }} className="flex flex-col gap-2">
        <AttachmentStrip
          attachments={attachments}
          onRemove={i => setAttachments(prev => {
            if (prev[i].url) URL.revokeObjectURL(prev[i].url)
            return prev.filter((_, j) => j !== i)
          })}
        />
        <div className="flex gap-2 items-end">
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
            className="p-3 rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-500 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-gray-600 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
            aria-label="Attach file"
            title="Attach image or audio"
          >
            📎
          </button>
          <textarea
            ref={textareaRef}
            id="composer-input"
            name="message"
            className="flex-1 rounded-xl border border-gray-300 dark:border-gray-600 px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 resize-none max-h-48 disabled:opacity-50 dark:bg-gray-700 dark:text-gray-100 dark:placeholder-gray-400"
            rows={1}
            placeholder={disabled ? 'Select or start a chat first' : streaming ? 'Type a follow-up… (queues until the current response finishes)' : 'Ask something… (Enter to send, Shift+Enter for newline)'}
            value={input}
            onChange={e => setInput(e.target.value)}
            onKeyDown={handleKeyDown}
            disabled={disabled}
          />
          {streaming && (
            <button
              type="button"
              onClick={onStop}
              className="px-4 py-3 rounded-xl bg-gray-200 dark:bg-gray-700 text-gray-700 dark:text-gray-200 text-sm font-medium hover:bg-gray-300 dark:hover:bg-gray-600 transition-colors whitespace-nowrap"
            >
              Stop
            </button>
          )}
          <button
            type="submit"
            disabled={(!input.trim() && attachments.length === 0) || disabled}
            className="px-4 py-3 rounded-xl bg-blue-600 text-white text-sm font-medium hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors whitespace-nowrap"
          >
            {streaming ? 'Queue' : 'Send'}
          </button>
        </div>
      </form>
    </div>
  )
}
