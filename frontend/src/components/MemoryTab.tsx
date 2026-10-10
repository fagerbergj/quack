import { useState, useEffect, useCallback, useMemo, useLayoutEffect, useRef, type Ref } from 'react'
import { api, type Memory, type VoteDirection, type MemoryWeekStats, type MemoryScopeStats } from '../api'
import { MemoryTimeline } from './MemoryTimeline'
import { MemorySortFilter, type MemorySort, type MemoryTierFilter } from './MemorySortFilter'
import { MemoryStatsHeader } from './MemoryStatsHeader'

const PAGE_SIZE = 20

export interface MemoryTabProps {
  // Storybook/test seam: pre-seeds state and skips the live fetch, so a story
  // can show empty/populated/error deterministically with no backend.
  initialState?: { memories: Memory[]; total: number; error?: string }
  // Same seam for the stats header, independent of initialState so a story can mix list and header states.
  initialStats?: { weeks: MemoryWeekStats[]; scopes: MemoryScopeStats[]; error?: string }
}

// Mirrors the backend's SetHumanVote delta so the UI moves instantly; the server response then
// overwrites it with the authoritative numbers.
function applyOptimisticVote(m: Memory, vote: VoteDirection): Memory {
  const upvotes = m.upvotes ?? 0
  const downvotes = m.downvotes ?? 0
  let nextUp = upvotes, nextDown = downvotes
  if (m.own_vote === 'up') nextUp--
  if (m.own_vote === 'down') nextDown--
  if (vote === 'up') nextUp++
  if (vote === 'down') nextDown++
  return {
    ...m,
    upvotes: Math.max(0, nextUp),
    downvotes: Math.max(0, nextDown),
    vote_score: Math.max(0, nextUp) - Math.max(0, nextDown),
    tier: vote === 'up' ? 'verified' : m.tier,
    own_vote: vote === 'none' ? undefined : vote,
  }
}

// Stats don't page or filter, so they get their own loading/error state and fetch once per mount.
function useMemoryStats(initialStats?: MemoryTabProps['initialStats']) {
  const [weeks, setWeeks] = useState<MemoryWeekStats[]>(initialStats?.weeks ?? [])
  const [scopes, setScopes] = useState<MemoryScopeStats[]>(initialStats?.scopes ?? [])
  const [loading, setLoading] = useState(initialStats === undefined)
  const [error, setError] = useState<string | null>(initialStats?.error ?? null)

  useEffect(() => {
    if (initialStats !== undefined) return // story/test seam
    let cancelled = false
    setLoading(true)
    api.getMemoryStats()
      .then(result => {
        if (cancelled) return
        setWeeks(result.weeks ?? [])
        setScopes(result.scopes ?? [])
        setError(null)
      })
      .catch(e => {
        if (cancelled) return
        setError(e instanceof Error ? e.message : 'Failed to load stats')
      })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [initialStats])

  return { weeks, scopes, loading, error }
}

// The scrollable list area: loading / error / empty / populated, each an
// exclusive branch of (loading, error, memories.length, searching).
function MemoryListBody({ loading, error, searching, bucket, memories, grouped, onForget, onVote, style }:
  { loading: boolean; error: string | null; searching: boolean; bucket: string; memories: Memory[]; grouped: boolean; onForget: (id: string) => Promise<void>; onVote: (id: string, vote: VoteDirection) => Promise<void>; style?: React.CSSProperties }) {
  return (
    <div className="flex-1 overflow-y-auto overscroll-contain" style={style}>
      {loading && (
        <div className="text-center text-gray-500 dark:text-gray-400 text-sm py-10">Loading…</div>
      )}
      {!loading && error && (
        <div className="m-3 rounded-md bg-red-50 dark:bg-red-950/30 border border-red-200 dark:border-red-800 px-4 py-3 text-sm text-red-700 dark:text-red-400">
          {error}
        </div>
      )}
      {!loading && !error && memories.length === 0 && (
        <div className="text-center text-gray-500 dark:text-gray-400 text-sm py-10">
          {searching ? 'No memories match that search' : bucket ? 'No memories in this bucket yet' : 'No memories yet'}
        </div>
      )}
      {!loading && !error && memories.length > 0 && (
        <MemoryTimeline
          memories={memories}
          onForget={onForget}
          onVote={onVote}
          grouped={grouped}
        />
      )}
    </div>
  )
}

function MemoryTabFooter({ footerRef, rangeStart, rangeEnd, total, canGoPrev, canGoNext, onPrevPage, onNextPage }:
  { footerRef: Ref<HTMLDivElement>; rangeStart: number; rangeEnd: number; total: number; canGoPrev: boolean; canGoNext: boolean; onPrevPage: () => void; onNextPage: () => void }) {
  return (
    <div ref={footerRef} className="p-3 border-t border-gray-200 dark:border-gray-700 flex items-center justify-between text-xs text-gray-500 dark:text-gray-400">
      <span>{rangeStart}–{rangeEnd} of {total}</span>
      <div className="flex gap-2">
        <button
          onClick={onPrevPage}
          disabled={!canGoPrev}
          className="px-2 py-1 rounded border border-gray-300 dark:border-gray-600 disabled:opacity-40 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors"
        >
          Prev
        </button>
        <button
          onClick={onNextPage}
          disabled={!canGoNext}
          className="px-2 py-1 rounded border border-gray-300 dark:border-gray-600 disabled:opacity-40 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors"
        >
          Next
        </button>
      </div>
    </div>
  )
}

export function MemoryTab({ initialState, initialStats }: MemoryTabProps = {}) {
  const [bucket, setBucket] = useState('')
  const [q, setQ] = useState('')
  // Debounced 250ms behind `q` (the live input value) so typing doesn't fire a search per keystroke.
  const [debouncedQ, setDebouncedQ] = useState('')
  const requestSeq = useRef(0)
  const [sort, setSort] = useState<MemorySort>('newest')
  const [tier, setTier] = useState<MemoryTierFilter>('')
  // Deliberately not persisted: a default listing shows only what quack currently trusts.
  const [includeInvalidated, setIncludeInvalidated] = useState(false)
  // page_token is opaque: pageTokens[i] fetched page i (undefined for the first), so going back replays
  // a received token and going forward stores the next one.
  const [pageIndex, setPageIndex] = useState(0)
  const [pageTokens, setPageTokens] = useState<(string | undefined)[]>([undefined])
  const [nextPageToken, setNextPageToken] = useState<string | undefined>(undefined)
  const [memories, setMemories] = useState<Memory[]>(initialState?.memories ?? [])
  // Mirrors `memories` for handleVote's rollback, so the read stays outside the setMemories updater,
  // which React may invoke more than once and must stay pure.
  const memoriesRef = useRef(memories)
  useEffect(() => { memoriesRef.current = memories })
  const [total, setTotal] = useState(initialState?.total ?? 0)
  const [loading, setLoading] = useState(initialState === undefined)
  const [error, setError] = useState<string | null>(initialState?.error ?? null)
  // Every bucket seen across loads, never shrunk. No distinct-buckets endpoint exists, so the dropdown is
  // best-effort over the pages this session has fetched.
  const [knownBuckets, setKnownBuckets] = useState<Set<string>>(
    () => new Set((initialState?.memories ?? []).map(m => m.bucket)),
  )
  // On short viewports the last row lands flush against the footer and reads as clipped; pad the scroll
  // area by the footer's measured height (not a guessed px value) so it survives font-size changes.
  const footerRef = useRef<HTMLDivElement>(null)
  const [footerHeight, setFooterHeight] = useState(0)

  const { weeks: statsWeeks, scopes: statsScopes, loading: statsLoading, error: statsError } = useMemoryStats(initialStats)

  // Guarded on q !== debouncedQ so mount never schedules a no-op resetPaging, which would still create a
  // new pageTokens array and fire a redundant initial fetch.
  useEffect(() => {
    if (q === debouncedQ) return
    const t = setTimeout(() => {
      setDebouncedQ(q)
      resetPaging()
    }, 250)
    return () => clearTimeout(t)
  }, [q, debouncedQ])

  const load = useCallback(async () => {
    const seq = ++requestSeq.current
    setLoading(true)
    setError(null)
    try {
      const result = await api.listMemories({
        bucket: bucket.trim() || undefined,
        q: debouncedQ.trim() || undefined,
        limit: PAGE_SIZE,
        page_token: pageTokens[pageIndex],
        include_invalidated: includeInvalidated || undefined,
        tier: tier || undefined,
        sort,
      })
      // A slower earlier request can resolve after a faster later one; only the latest may write state.
      if (seq !== requestSeq.current) return
      setMemories(result.memories)
      setTotal(result.total)
      setNextPageToken(result.next_page_token)
      setKnownBuckets(prev => {
        const next = new Set(prev)
        for (const m of result.memories) next.add(m.bucket)
        return next
      })
    } catch (e) {
      if (seq !== requestSeq.current) return
      // Never fall back to search: a browse view quietly showing a different result set is worse than an error.
      setError(e instanceof Error ? e.message : 'Failed to load memories')
    } finally {
      if (seq === requestSeq.current) setLoading(false)
    }
  }, [bucket, debouncedQ, pageIndex, pageTokens, includeInvalidated, tier, sort])

  useEffect(() => {
    if (initialState !== undefined) return // story/test seam: static demo state, no live fetch
    void load()
  }, [load, initialState])

  // Stable identity (empty deps, functional setState) so memo(MemoryEntry) can skip the other rows.
  const handleForget = useCallback(async (id: string) => {
    await api.forgetMemory(id)
    setMemories(prev => prev.filter(m => m.id !== id))
    setTotal(t => Math.max(0, t - 1))
  }, [])

  // Optimistic-first; on failure roll back only the voted row, since a page-wide snapshot would also
  // discard unrelated changes (e.g. another vote's response) that landed while this request was in flight.
  const handleVote = useCallback(async (id: string, vote: VoteDirection) => {
    // Read from the ref, not inside the updater: a synchronous throw from voteMemory would otherwise reach
    // the catch with prevRow unset and skip the rollback.
    const prevRow = memoriesRef.current.find(m => m.id === id)
    setMemories(cur => cur.map(m => (m.id === id ? applyOptimisticVote(m, vote) : m)))
    try {
      const updated = await api.voteMemory(id, vote)
      setMemories(cur => cur.map(m => (m.id === id ? updated : m)))
    } catch (e) {
      if (prevRow) setMemories(cur => cur.map(m => (m.id === id ? prevRow : m)))
      throw e
    }
  }, [])

  function resetPaging() {
    setPageIndex(0)
    setPageTokens([undefined])
  }

  function handleBucketChange(next: string) {
    setBucket(next)
    resetPaging()
  }

  // Server-side filter (a page reload, not a client re-slice) so it spans the whole corpus.
  function handleTierChange(next: MemoryTierFilter) {
    setTier(next)
    resetPaging()
  }

  // Server-side sort, for the same reason as the tier filter.
  function handleSortChange(next: MemorySort) {
    setSort(next)
    resetPaging()
  }

  function handleIncludeInvalidatedChange(next: boolean) {
    setIncludeInvalidated(next)
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
      <MemoryStatsHeader weeks={statsWeeks} loading={statsLoading} error={statsError} />
      <div className="p-3 border-b border-gray-200 dark:border-gray-700 flex flex-wrap items-center gap-2">
        {/* Own line below `medium` so the placeholder (the only explanation
            of memory search) isn't clipped at 390px. */}
        <input
          type="search"
          value={q}
          onChange={e => setQ(e.target.value)}
          placeholder="Search — what would a run recall for this?"
          aria-label="Search memories"
          className="grow basis-full medium:basis-0 min-w-0 rounded-lg border border-gray-300 dark:border-gray-600 px-3 py-1.5 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 dark:bg-gray-700 dark:text-gray-100 dark:placeholder-gray-400"
        />
        <label className="flex-shrink-0 min-h-[44px] -my-2 flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400 cursor-pointer select-none whitespace-nowrap">
          <input
            type="checkbox"
            checked={includeInvalidated}
            onChange={e => handleIncludeInvalidatedChange(e.target.checked)}
            className="w-5 h-5 accent-blue-600"
          />
          Show invalidated
        </label>
        <MemorySortFilter
          sort={sort}
          onSortChange={handleSortChange}
          bucket={bucket}
          buckets={bucketOptions}
          onBucketChange={handleBucketChange}
          tier={tier}
          onTierChange={handleTierChange}
          scopes={statsScopes}
        />
      </div>

      <MemoryListBody
        loading={loading}
        error={error}
        searching={searching}
        bucket={bucket}
        memories={memories}
        grouped={sort === 'newest' || sort === 'oldest'}
        onForget={handleForget}
        onVote={handleVote}
        style={showFooter ? { paddingBottom: footerHeight } : undefined}
      />

      {showFooter && (
        <MemoryTabFooter
          footerRef={footerRef}
          rangeStart={rangeStart}
          rangeEnd={rangeEnd}
          total={total}
          canGoPrev={pageIndex > 0}
          canGoNext={hasMore}
          onPrevPage={handlePrevPage}
          onNextPage={handleNextPage}
        />
      )}
    </div>
  )
}
