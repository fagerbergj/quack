import { useState, useEffect, useCallback } from 'react'
import { api, type Memory } from '../api'
import { MemoryEntry } from './MemoryEntry'

const PAGE_SIZE = 20

export interface MemoryTabProps {
  // Storybook/test seam: pre-seeds state and skips the live fetch, so a story
  // can show empty/populated/error deterministically with no backend.
  initialState?: { memories: Memory[]; total: number; error?: string }
}

// The Memory tab (#727): browse what quack believes (list, never falling back
// to search on error - a browse view that quietly shows a different result
// set than it claims is worse than an error state), search "what would a run
// recall for this", and forget one entry at a time.
export function MemoryTab({ initialState }: MemoryTabProps = {}) {
  const [bucket, setBucket] = useState('')
  const [q, setQ] = useState('')
  const [offset, setOffset] = useState(0)
  const [memories, setMemories] = useState<Memory[]>(initialState?.memories ?? [])
  const [total, setTotal] = useState(initialState?.total ?? 0)
  const [loading, setLoading] = useState(initialState === undefined)
  const [error, setError] = useState<string | null>(initialState?.error ?? null)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const result = await api.listMemories({
        bucket: bucket.trim() || undefined,
        q: q.trim() || undefined,
        limit: PAGE_SIZE,
        offset,
      })
      setMemories(result.memories)
      setTotal(result.total)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load memories')
    } finally {
      setLoading(false)
    }
  }, [bucket, q, offset])

  useEffect(() => {
    if (initialState !== undefined) return // story/test seam: static demo state, no live fetch
    void load()
  }, [load, initialState])

  async function handleForget(id: string) {
    await api.forgetMemory(id)
    setMemories(prev => prev.filter(m => m.id !== id))
    setTotal(t => Math.max(0, t - 1))
  }

  const searching = q.trim() !== ''
  const hasMore = !searching && offset + memories.length < total

  return (
    <div className="flex flex-col h-full">
      <div className="p-3 border-b border-gray-200 dark:border-gray-700 flex flex-col sm:flex-row gap-2">
        <input
          type="text"
          value={bucket}
          onChange={e => { setBucket(e.target.value); setOffset(0) }}
          placeholder="Bucket (e.g. repo:NightsOut, role:coding) — blank for all"
          aria-label="Bucket filter"
          className="flex-1 min-w-0 rounded-lg border border-gray-300 dark:border-gray-600 px-3 py-1.5 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 dark:bg-gray-700 dark:text-gray-100 dark:placeholder-gray-400"
        />
        <input
          type="search"
          value={q}
          onChange={e => { setQ(e.target.value); setOffset(0) }}
          placeholder="Search — what would a run recall for this?"
          aria-label="Search memories"
          className="flex-1 min-w-0 rounded-lg border border-gray-300 dark:border-gray-600 px-3 py-1.5 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 dark:bg-gray-700 dark:text-gray-100 dark:placeholder-gray-400"
        />
      </div>

      <div className="flex-1 overflow-y-auto overscroll-contain">
        {loading && (
          <div className="text-center text-gray-400 dark:text-gray-500 text-sm py-10">Loading…</div>
        )}
        {!loading && error && (
          <div className="m-3 rounded-md bg-red-50 dark:bg-red-950/30 border border-red-200 dark:border-red-800 px-4 py-3 text-sm text-red-700 dark:text-red-400">
            {error}
          </div>
        )}
        {!loading && !error && memories.length === 0 && (
          <div className="text-center text-gray-400 dark:text-gray-500 text-sm py-10">
            {searching ? 'No memories match that search' : bucket ? 'No memories in this bucket yet' : 'No memories yet'}
          </div>
        )}
        {!loading && !error && memories.map(m => (
          <MemoryEntry key={m.id} memory={m} onForget={handleForget} />
        ))}
      </div>

      {!loading && !error && !searching && total > 0 && (
        <div className="p-3 border-t border-gray-200 dark:border-gray-700 flex items-center justify-between text-xs text-gray-500 dark:text-gray-400">
          <span>{offset + 1}–{offset + memories.length} of {total}</span>
          <div className="flex gap-2">
            <button
              onClick={() => setOffset(o => Math.max(0, o - PAGE_SIZE))}
              disabled={offset === 0}
              className="px-2 py-1 rounded border border-gray-300 dark:border-gray-600 disabled:opacity-40 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors"
            >
              Prev
            </button>
            <button
              onClick={() => setOffset(o => o + PAGE_SIZE)}
              disabled={!hasMore}
              className="px-2 py-1 rounded border border-gray-300 dark:border-gray-600 disabled:opacity-40 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors"
            >
              Next
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
