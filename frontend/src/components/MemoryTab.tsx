import { useState, useEffect, useCallback, useMemo, useLayoutEffect, useRef } from 'react'
import { api, type Memory, type VoteDirection, type MemoryWeekStats, type MemoryScopeStats } from '../api'
import { MemoryTimeline } from './MemoryTimeline'
import { MemorySortFilter, type MemorySort, type MemoryTierFilter } from './MemorySortFilter'
import { MemoryStatsHeader } from './MemoryStatsHeader'

const PAGE_SIZE = 20

export interface MemoryTabProps {
  // Storybook/test seam: pre-seeds state and skips the live fetch, so a story
  // can show empty/populated/error deterministically with no backend.
  initialState?: { memories: Memory[]; total: number; error?: string }
  // Same seam for the stats header (#1267) - independent of initialState so
  // a story can show the list and the header loading/empty/populated in any
  // combination.
  initialStats?: { weeks: MemoryWeekStats[]; scopes: MemoryScopeStats[]; error?: string }
}

// applyOptimisticVote mirrors the backend's SetHumanVote delta (up:+1/undo,
// down:+1/undo, none:remove) so the UI moves instantly; the server response
// that follows overwrites this with the authoritative numbers.
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

// The Memory tab (#727): browse what quack believes (list, never falling back
// to search on error - a browse view that quietly shows a different result
// set than it claims is worse than an error state), search "what would a run
// recall for this", and forget one entry at a time.
export function MemoryTab({ initialState, initialStats }: MemoryTabProps = {}) {
  const [bucket, setBucket] = useState('')
  const [q, setQ] = useState('')
  // Debounced 250ms behind `q` (the input's live value, used only for display)
  // so a fast typist doesn't fire one search request per keystroke (#1285).
  const [debouncedQ, setDebouncedQ] = useState('')
  const requestSeq = useRef(0)
  const [sort, setSort] = useState<MemorySort>('newest')
  const [tier, setTier] = useState<MemoryTierFilter>('')
  // Local to the tab (no persisted preference, per design doc §8 step 6) - a
  // default listing shows only what quack currently trusts.
  const [includeInvalidated, setIncludeInvalidated] = useState(false)
  // page_token is opaque (never parsed/constructed) - pageTokens[i] is the
  // token that fetched page i (pageTokens[0] is undefined: first page).
  // Going back replays a token this component already received, going
  // forward stores the next one the server just gave it.
  const [pageIndex, setPageIndex] = useState(0)
  const [pageTokens, setPageTokens] = useState<(string | undefined)[]>([undefined])
  const [nextPageToken, setNextPageToken] = useState<string | undefined>(undefined)
  const [memories, setMemories] = useState<Memory[]>(initialState?.memories ?? [])
  // Mirrors `memories` for handleVote's rollback (finding 1, PR #1300 review) -
  // a ref read there stays outside the setMemories updater, which React may
  // invoke more than once and must stay pure.
  const memoriesRef = useRef(memories)
  useEffect(() => { memoriesRef.current = memories })
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

  // Stats fetch (#1267) is independent of the memory list fetch above - it
  // doesn't page or filter, so it has its own loading/error state and only
  // runs once per mount.
  const [statsWeeks, setStatsWeeks] = useState<MemoryWeekStats[]>(initialStats?.weeks ?? [])
  const [statsScopes, setStatsScopes] = useState<MemoryScopeStats[]>(initialStats?.scopes ?? [])
  const [statsLoading, setStatsLoading] = useState(initialStats === undefined)
  const [statsError, setStatsError] = useState<string | null>(initialStats?.error ?? null)

  useEffect(() => {
    if (initialStats !== undefined) return // story/test seam
    let cancelled = false
    setStatsLoading(true)
    api.getMemoryStats()
      .then(result => {
        if (cancelled) return
        setStatsWeeks(result.weeks ?? [])
        setStatsScopes(result.scopes ?? [])
        setStatsError(null)
      })
      .catch(e => {
        if (cancelled) return
        setStatsError(e instanceof Error ? e.message : 'Failed to load stats')
      })
      .finally(() => { if (!cancelled) setStatsLoading(false) })
    return () => { cancelled = true }
  }, [initialStats])

  // Debounce: reset paging and adopt the typed query only after 250ms of
  // no further keystrokes (prototype: 9 requests -> 2 for an 8-char query).
  // Guarded on q !== debouncedQ so mount (both '') never schedules a no-op
  // resetPaging - that would still create a new pageTokens array and fire
  // a redundant, extra initial fetch.
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
      // A slower earlier request can resolve after a faster later one; only
      // the most recently issued request may write to state (#1285 race).
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
      setError(e instanceof Error ? e.message : 'Failed to load memories')
    } finally {
      if (seq === requestSeq.current) setLoading(false)
    }
  }, [bucket, debouncedQ, pageIndex, pageTokens, includeInvalidated, tier, sort])

  useEffect(() => {
    if (initialState !== undefined) return // story/test seam: static demo state, no live fetch
    void load()
  }, [load, initialState])

  // Stable identity (empty deps, via functional setState) so memo(MemoryEntry)
  // (#1286) can actually skip the other 19 rows when one row is voted/forgotten.
  const handleForget = useCallback(async (id: string) => {
    await api.forgetMemory(id)
    setMemories(prev => prev.filter(m => m.id !== id))
    setTotal(t => Math.max(0, t - 1))
  }, [])

  // handleVote is optimistic-first (frontend-design convention): the store
  // updates immediately so the arrow highlight/score never lags a click,
  // and rolls back only the voted ROW on failure (#1265 review finding 8) -
  // a page-wide snapshot would also discard any other unrelated change
  // (e.g. another vote's own response landing) that happened while this
  // request was in flight.
  const handleVote = useCallback(async (id: string, vote: VoteDirection) => {
    // Read the pre-vote row from the ref, not from inside the setMemories
    // updater - an updater must be pure, and a synchronous throw from
    // voteMemory (below) would otherwise reach the catch with prevRow still
    // unset, skipping the rollback.
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

  // Server-side filter (#1265 review finding 10) - a page reload with the
  // new tier, not a client-side re-slice of whatever page happened to be
  // loaded, so the filter spans the whole corpus, not just the current page.
  function handleTierChange(next: MemoryTierFilter) {
    setTier(next)
    resetPaging()
  }

  // Server-side sort (#1266 owner follow-up), same reasoning as tier above -
  // a page reload with the new sort, not a client-side re-slice.
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
        <label className="flex-shrink-0 flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400 cursor-pointer select-none whitespace-nowrap">
          <input
            type="checkbox"
            checked={includeInvalidated}
            onChange={e => handleIncludeInvalidatedChange(e.target.checked)}
            className="accent-blue-600"
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

      <div className="flex-1 overflow-y-auto overscroll-contain" style={showFooter ? { paddingBottom: footerHeight } : undefined}>
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
            onForget={handleForget}
            onVote={handleVote}
            grouped={sort === 'newest' || sort === 'oldest'}
          />
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
