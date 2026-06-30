import { useLayoutEffect, useRef, useState } from 'react'
import { AttachmentStrip, type AttachmentItem } from './AttachmentUI'

export interface AttachmentPreview {
  url: string
  mime: string
  name: string
}

export interface ComposerProps {
  // No active chat — input is disabled.
  disabled: boolean
  // A turn is streaming — show Stop instead of Send and lock input.
  streaming: boolean
  onSubmit: (text: string, files: File[], previews: AttachmentPreview[]) => void
  onStop: () => void
}

// Composer owns the draft `input` + `attachments` locally so typing only re-renders
// this small component, not the whole chat (the turn list / DAG trees). It hands the
// finished message up via onSubmit.
export function Composer({ disabled, streaming, onSubmit, onStop }: ComposerProps) {
  const [input, setInput] = useState('')
  const [attachments, setAttachments] = useState<AttachmentItem[]>([])
  const fileInputRef = useRef<HTMLInputElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  // Auto-grow the textarea with its content (CSS field-sizing isn't in Firefox/
  // Safari yet). Reset to auto first so it shrinks back when the draft is cleared;
  // max-h-48 + overflow-y-auto cap it and start scrolling past ~8 lines.
  useLayoutEffect(() => {
    const ta = textareaRef.current
    if (!ta) return
    ta.style.height = 'auto'
    ta.style.height = `${ta.scrollHeight}px`
  }, [input])

  function submit() {
    const trimmed = input.trim()
    if ((!trimmed && attachments.length === 0) || streaming || disabled) return
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
    <div className="border-t border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 px-6 py-4">
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
            className="flex-1 rounded-xl border border-gray-300 dark:border-gray-600 px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 resize-none max-h-48 overflow-y-auto disabled:opacity-50 dark:bg-gray-700 dark:text-gray-100 dark:placeholder-gray-400"
            rows={1}
            placeholder={disabled ? 'Select or start a chat first' : 'Ask something… (Enter to send, Shift+Enter for newline)'}
            value={input}
            onChange={e => setInput(e.target.value)}
            onKeyDown={handleKeyDown}
            disabled={streaming || disabled}
          />
          {streaming ? (
            <button
              type="button"
              onClick={onStop}
              className="px-4 py-3 rounded-xl bg-gray-200 dark:bg-gray-700 text-gray-700 dark:text-gray-200 text-sm font-medium hover:bg-gray-300 dark:hover:bg-gray-600 transition-colors whitespace-nowrap"
            >
              Stop
            </button>
          ) : (
            <button
              type="submit"
              disabled={(!input.trim() && attachments.length === 0) || disabled}
              className="px-4 py-3 rounded-xl bg-blue-600 text-white text-sm font-medium hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors whitespace-nowrap"
            >
              Send
            </button>
          )}
        </div>
      </form>
    </div>
  )
}
