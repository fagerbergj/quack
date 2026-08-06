import { useState } from 'react'
import type { Memory } from '../api'

export interface MemoryEntryProps {
  memory: Memory
  onForget: (id: string) => Promise<void>
}

// One memory row: content + metadata, and a two-step forget control - a click
// reveals Confirm/Cancel, only Confirm actually deletes - so a misclick can
// never silently forget a fact (#727).
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
        <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 mt-1 text-xs text-gray-400 dark:text-gray-500">
          <span className="font-mono">{memory.bucket}</span>
          <span aria-hidden="true">·</span>
          <span>{memory.author}</span>
          <span aria-hidden="true">·</span>
          <span>{new Date(memory.timestamp).toLocaleString()}</span>
          {memory.kind && (
            <>
              <span aria-hidden="true">·</span>
              <span>{memory.kind}</span>
            </>
          )}
          {memory.score != null && (
            <>
              <span aria-hidden="true">·</span>
              <span>score {memory.score.toFixed(2)}</span>
            </>
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
            className="text-xs text-gray-400 hover:text-red-500 dark:hover:text-red-400 transition-colors px-2 py-1 rounded"
          >
            Forget
          </button>
        )}
      </div>
    </div>
  )
}
