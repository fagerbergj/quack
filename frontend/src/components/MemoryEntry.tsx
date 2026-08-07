import { useState } from 'react'
import type { Memory } from '../api'
import { paletteClasses } from '../lib/colorHash'

export interface MemoryEntryProps {
  memory: Memory
  onForget: (id: string) => Promise<void>
}

// Pill - a category-coloured pill (#746 item 13): the colour is deterministic
// (hashed from the label itself, not assignment order) and always rendered
// WITH the label text - colour is never the only signal.
function Pill({ label, seed }: { label: string; seed: string }) {
  return (
    <span className={`inline-flex items-center px-1.5 py-0.5 rounded-full text-[10px] font-medium ${paletteClasses(seed)}`}>
      {label}
    </span>
  )
}

// One memory row: content + bucket/author/kind pills, and a two-step forget
// control - a click reveals Confirm/Cancel, only Confirm actually deletes -
// so a misclick can never silently forget a fact (#727). Forget is icon-only
// (#746 item 12): the accessible name (aria-label) and a tooltip (title)
// stand in for the label text, and the confirmation step is unchanged - an
// icon-only DELETE with no confirmation is how people lose things they wanted.
export function MemoryEntry({ memory, onForget }: MemoryEntryProps) {
  const [confirming, setConfirming] = useState(false)
  const [forgetting, setForgetting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function handleConfirm() {
    setForgetting(true)
    setError(null)
    try {
      await onForget(memory.id)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to forget')
      setForgetting(false)
      setConfirming(false)
    }
  }

  return (
    <div className="px-3 py-2.5 border-b border-gray-100 dark:border-gray-700 flex items-start gap-3">
      <div className="flex-1 min-w-0">
        <p className="text-sm text-gray-800 dark:text-gray-100 whitespace-pre-wrap">{memory.content}</p>
        <div className="flex flex-wrap items-center gap-1.5 mt-1.5">
          <Pill label={memory.bucket} seed={memory.bucket} />
          <Pill label={memory.author} seed={memory.author} />
          {memory.kind && <Pill label={memory.kind} seed={memory.kind} />}
          <span className="text-[11px] text-gray-400 dark:text-gray-500">{new Date(memory.timestamp).toLocaleString()}</span>
          {memory.score != null && (
            <span className="text-[11px] text-gray-400 dark:text-gray-500">score {memory.score.toFixed(2)}</span>
          )}
        </div>
        {error && <p className="text-xs text-red-500 dark:text-red-400 mt-1">{error}</p>}
      </div>
      <div className="flex-shrink-0">
        {confirming ? (
          <div className="flex items-center gap-1.5">
            <button
              onClick={handleConfirm}
              disabled={forgetting}
              className="text-xs px-2 py-1 rounded bg-red-600 text-white hover:bg-red-700 disabled:opacity-50 transition-colors"
            >
              {forgetting ? 'Forgetting…' : 'Confirm'}
            </button>
            <button
              onClick={() => setConfirming(false)}
              disabled={forgetting}
              className="text-xs px-2 py-1 rounded border border-gray-300 dark:border-gray-600 text-gray-500 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors"
            >
              Cancel
            </button>
          </div>
        ) : (
          <button
            onClick={() => setConfirming(true)}
            aria-label={`Forget: ${memory.content.slice(0, 40)}`}
            title="Forget this memory"
            className="w-7 h-7 flex items-center justify-center rounded text-gray-400 hover:text-red-500 dark:hover:text-red-400 hover:bg-red-50 dark:hover:bg-red-900/20 transition-colors"
          >
            <span aria-hidden="true">🗑</span>
          </button>
        )}
      </div>
    </div>
  )
}
