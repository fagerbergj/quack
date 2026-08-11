import { useState, useEffect, useCallback, useMemo, useLayoutEffect, useRef } from 'react'
import { api, type Memory } from '../api'
import { MemoryTimeline } from './MemoryTimeline'
import { MemorySortFilter, type MemorySort } from './MemorySortFilter'

const PAGE_SIZE = 20

export interface MemoryTabProps {
  // Storybook/test seam: pre-seeds state and skips the live fetch, so a story
  // can show empty/populated/error deterministically with no backend.
  initialState?: { memories: Memory[]; total: number; error?: string }
}

// sortMemories orders a loaded page client-side (#746 items 11/15) - the API
// has no sort param (Forbidden: no endpoint/contract changes), so this only
// ever reorders what's already been fetched, not the underlying corpus.
function sortMemories(memories: Memory[], sort: MemorySort): Memory[] {
  const sorted = [...memories].sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime())
  return sort === 'oldest' ? sorted.reverse() : sorted
}

// The Memory tab (#727): browse what quack believes (list, never falling back
// to search on error - a browse view that quietly shows a different result
// set than it claims is worse than an error state), search "what would a run
// recall for this", and forget one entry at a time.
export function MemoryTab({ initialState }: MemoryTabProps = {}) {
  const [bucket, setBucket] = useState('')
  const [q, setQ] = useState('')
  const [sort, setSort] = useState<MemorySort>('newest')
  // page_token is opaque (never parsed/constructed) - pageTokens[i] is the
  // token that fetched page i (pageTokens[0] is undefined: first page).
  // Going back replays a token this component already received, going
  // forward stores the next one the server just gave it.
  const [pageIndex, setPageIndex] = useState(0)
  const [pageTokens, setPageTokens] = useState<(string | undefined)[]>([undefined])
  const [nextPageToken, setNextPageToken] = useState<string | undefined>(undefined)
  const [memories, setMemories] = useState<Memory[]>(initialState?.memories ?? [])
  const [total, setTotal] = useState(initialState?.total ?? 0)
  const [loading, setLoading] = useState(initialState === undefined)
  const [error, setError] = useState<string | null>(initialState?.error ?? null)
  // Every bucket value seen across any load, accumulated (never shrunk) - the
  // dropdown's option list (item 11). There's no "list distinct buckets"
  // endpoint to call instead (Forbidden: don't add one), so this is
  // best-effort against whatever pages this session has actually fetched.
  const [knownBuckets, setKnownBuckets] = useState<Set<string>>(
    () => new Set((initialState?.memories ?? []).map(m => m.bucket)),
  )
  // The pagination footer is a normal flex sibling below the scroll area, not
  // an overlay - but on short viewports (#759 item 3) the last row can land
  // flush against it with no breathing room, reading as clipped. Padding the
  // scroll area by the footer's own measured height (never a guessed pixel
  // value, so it survives a font-size change) gives the last row room to
  // clear it once scrolled fully into view.
  const footerRef = useRef<HTMLDivElement>(null)
  const [footerHeight, setFooterHeight] = useState(0)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const result = await api.listMemories({
        bucket: bucket.trim() || undefined,
        q: q.trim() || undefined,
        limit: PAGE_SIZE,
        page_token: pageTokens[pageIndex],
      })
      setMemories(result.memories)
      setTotal(result.total)
      setNextPageToken(result.next_page_token)
      setKnownBuckets(prev => {
        const next = new Set(prev)
        for (const m of result.memories) next.add(m.bucket)
        return next
      })
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load memories')
    } finally {
      setLoading(false)
    }
  }, [bucket, q, pageIndex, pageTokens])

  useEffect(() => {
    if (initialState !== undefined) return // story/test seam: static demo state, no live fetch
    void load()
  }, [load, initialState])

  async function handleForget(id: string) {
    await api.forgetMemory(id)
    setMemories(prev => prev.filter(m => m.id !== id))
    setTotal(t => Math.max(0, t - 1))
  }

  function resetPaging() {
    setPageIndex(0)
    setPageTokens([undefined])
  }

  function handleBucketChange(next: string) {
    setBucket(next)
    resetPaging()
  }

  function handlePrevPage() {
    setPageIndex(i => Math.max(0, i - 1))
  }

  function handleNextPage() {
    if (!nextPageToken) return
    setPageTokens(prev => {
      const next = [...prev]
      next[pageIndex + 1] = nextPageToken
      return next
    })
    setPageIndex(i => i + 1)
  }

  const searching = q.trim() !== ''
  const hasMore = !searching && !!nextPageToken
  const rangeStart = pageIndex * PAGE_SIZE + 1
  const rangeEnd = pageIndex * PAGE_SIZE + memories.length
  const sortedMemories = useMemo(() => sortMemories(memories, sort), [memories, sort])
  const bucketOptions = useMemo(() => Array.from(knownBuckets).sort(), [knownBuckets])
  const showFooter = !loading && !error && !searching && total > 0

  useLayoutEffect(() => {
    const el = footerRef.current
    if (!el) { setFooterHeight(0); return }
    const measure = () => setFooterHeight(el.getBoundingClientRect().height)
    measure()
    if (typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [showFooter])

  return (
    <div className="flex flex-col h-full">
      <div className="p-3 border-b border-gray-200 dark:border-gray-700 flex items-center gap-2">
        <input
          type="search"
          value={q}
          onChange={e => { setQ(e.target.value); resetPaging() }}
          placeholder="Search — what would a run recall for this?"
          aria-label="Search memories"
          className="flex-1 min-w-0 rounded-lg border border-gray-300 dark:border-gray-600 px-3 py-1.5 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 dark:bg-gray-700 dark:text-gray-100 dark:placeholder-gray-400"
        />
        <MemorySortFilter
          sort={sort}
          onSortChange={setSort}
          bucket={bucket}
          buckets={bucketOptions}
          onBucketChange={handleBucketChange}
        />
      </div>

      <div className="flex-1 overflow-y-auto overscroll-contain" style={showFooter ? { paddingBottom: footerHeight } : undefined}>
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
        {!loading && !error && memories.length > 0 && (
          <MemoryTimeline memories={sortedMemories} onForget={handleForget} />
        )}
      </div>

      {showFooter && (
        <div ref={footerRef} className="p-3 border-t border-gray-200 dark:border-gray-700 flex items-center justify-between text-xs text-gray-500 dark:text-gray-400">
          <span>{rangeStart}–{rangeEnd} of {total}</span>
          <div className="flex gap-2">
            <button
              onClick={handlePrevPage}
              disabled={pageIndex === 0}
              className="px-2 py-1 rounded border border-gray-300 dark:border-gray-600 disabled:opacity-40 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors"
            >
              Prev
            </button>
            <button
              onClick={handleNextPage}
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
