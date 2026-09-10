import { memo, useEffect, useRef, useState } from 'react'
import type { Memory, VoteDirection } from '../api'
import { paletteClasses } from '../lib/colorHash'
import { relativeTime } from '../lib/relativeTime'
import { Icon } from './Icon'
import { VoteControl } from './VoteControl'

export interface MemoryEntryProps {
  memory: Memory
  onForget: (id: string) => Promise<void>
  onVote: (id: string, vote: VoteDirection) => Promise<void>
}

export type MemoryTier = 'unverified' | 'reinforced' | 'invalidated' | 'unknown'

// Maps a memory's status (Memory['status'], openapi.yaml) to a display tier.
// Missing/empty status is a pre-lifecycle memory (design doc §3) and reads as
// unverified, by design - not "unknown". Every other value is switched on exhaustively: adding a status to the generated enum without a case here is a compile error (the `never` assignment below fails to type-check), not a badge silently relabeling an unrecognized status "unverified".
export function memoryTier(memory: Pick<Memory, 'status'>): MemoryTier {
  const status = memory.status
  if (!status) return 'unverified'
  switch (status) {
    case 'unverified': return 'unverified'
    case 'reinforced': return 'reinforced'
    case 'invalidated': return 'invalidated'
    default: {
      const _exhaustive: never = status
      void _exhaustive
      return 'unknown'
    }
  }
}

// memoryTierLabel renders an unknown status as itself (the raw string the
// backend sent) rather than masquerading as unverified.
export function memoryTierLabel(memory: Pick<Memory, 'status' | 'reinforcement_count'>): string {
  const tier = memoryTier(memory)
  if (tier === 'reinforced') return `reinforced ×${memory.reinforcement_count ?? 0}`
  if (tier === 'unknown') return String(memory.status)
  return tier
}

// Mirrors originBadgeClass's palette (ChatList.tsx) for the memory lifecycle
// tiers (memory-lifecycle.md §8 step 6): green for reinforced, red for
// invalidated, neutral gray for unverified AND any unrecognized status - never a color the reader would read as a claim about a tier we didn't actually verify.
export function memoryTierBadgeClass(tier: MemoryTier): string {
  switch (tier) {
    case 'reinforced': return 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400'
    case 'invalidated': return 'bg-red-100 text-red-600 dark:bg-red-900/30 dark:text-red-400'
    default: return 'bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-300'
  }
}

// TierBadge - the compact lifecycle indicator every memory row carries.
// Title mirrors the visible label, or the invalidation reason when present,
// so a hover reveals the same secondary text without requiring the extra line.
function TierBadge({ memory }: { memory: Memory }) {
  const tier = memoryTier(memory)
  return (
    <span
      title={memory.invalidation_reason ?? memoryTierLabel(memory)}
      className={`inline-flex items-center px-1.5 py-0.5 rounded-full text-[11px] font-medium ${memoryTierBadgeClass(tier)}`}
    >
      {memoryTierLabel(memory)}
    </span>
  )
}

// VoteTierBadge - the vote-based tier (epic #1255 P4, memory.tier). Rendered
// only for 'verified' (#1266): 'unverified' duplicates the lifecycle
// TierBadge's default label, so the caller skips it rather than showing two chips that both say "unverified".
function VoteTierBadge({ tier }: { tier: 'unverified' | 'verified' }) {
  return (
    <span className="inline-flex items-center px-1.5 py-0.5 rounded-full text-[11px] font-medium bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-400">
      {tier}
    </span>
  )
}

// A category-coloured pill (#746 item 13): the colour is deterministic
// (hashed from the label itself, not assignment order) and always rendered
// WITH the label text - colour is never the only signal. `neutral` opts out of the hash (#1266): a node id used as provenance can hash to red, which reads as an error rather than "this node wrote it".
function Pill({ label, seed, neutral }: { label: string; seed: string; neutral?: boolean }) {
  const cls = neutral ? 'bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-300' : paletteClasses(seed)
  return (
    <span className={`inline-flex items-center px-1.5 py-0.5 rounded-full text-[11px] font-medium ${cls}`}>
      {label}
    </span>
  )
}

// KebabMenu - secondary row actions (UI rule: no full-text action buttons in
// the row itself, #746). Forget lives here, with its own confirm step so a
// misclick inside the menu still can't silently delete a fact.
function KebabMenu({ memory, onForget }: { memory: Memory; onForget: (id: string) => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [forgetting, setForgetting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) { setOpen(false); setConfirming(false) } }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') { setOpen(false); setConfirming(false) } }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  async function handleConfirm() {
    setForgetting(true)
    setError(null)
    try {
      await onForget(memory.id)
      setOpen(false)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to forget')
    } finally {
      setForgetting(false)
      setConfirming(false)
    }
  }

  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => setOpen(o => !o)}
        aria-label="Memory actions"
        aria-haspopup="menu"
        aria-expanded={open}
        title="More actions"
        className="min-w-[44px] min-h-[44px] flex items-center justify-center rounded text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors"
      >
        <Icon name="more_vert" className="w-4 h-4" />
      </button>
      {open && (
        <div role="menu" className="absolute z-50 right-0 mt-1 w-40 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-lg py-1 text-xs">
          {error && <p className="px-3 py-1 text-red-500 dark:text-red-400">{error}</p>}
          {confirming ? (
            <div className="flex items-center gap-1.5 px-2 py-1">
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
              role="menuitem"
              onClick={() => setConfirming(true)}
              className="w-full flex items-center gap-1.5 text-left px-3 py-1.5 min-h-[44px] medium:min-h-0 text-gray-700 dark:text-gray-200 hover:bg-gray-50 dark:hover:bg-gray-700"
            >
              <Icon name="delete" className="w-3.5 h-3.5" /> Forget
            </button>
          )}
        </div>
      )}
    </div>
  )
}

// One memory row. memo: a page is 20 rows and a vote only changes one, so
// without this every row re-renders (and re-formats its dates, #1286) on any
// sibling's vote. Test-only render counter: the memo test asserts on counts, never on timings.
export const memoryEntryRenderProbe = { count: 0 }

export const MemoryEntry = memo(function MemoryEntry({ memory, onForget, onVote }: MemoryEntryProps) {
  memoryEntryRenderProbe.count++
  const voteTier = memory.tier ?? 'unverified'
  const lastUpvoted = relativeTime(memory.last_upvoted_at)
  const lastRecalled = relativeTime(memory.last_recalled_at)

  // Not memoized on memory.timestamp: that value never changes for a given
  // memory, so caching on it froze the label at first computation even across
  // a legitimate re-render (#1300 review). memo(MemoryEntry) already skips the row when memory is unchanged - this recompute only runs when the row actually re-renders.
  const mintedTime = new Date(memory.timestamp)
  const mintedTimeText = Number.isNaN(mintedTime.getTime()) ? memory.timestamp : mintedTime.toLocaleString()
  const mintedTimeRelative = relativeTime(memory.timestamp) ?? mintedTimeText

  return (
    <div className="px-3 py-2.5 border-b border-gray-100 dark:border-gray-700 flex items-start gap-2">
      <div className="flex-1 min-w-0">
        <p className="text-sm text-gray-800 dark:text-gray-100 whitespace-pre-wrap break-words">{memory.content}</p>
        <div className="flex flex-wrap items-center gap-1.5 mt-1.5">
          <Pill label={memory.bucket} seed={memory.bucket} />
          <Pill label={memory.author} seed={memory.author} neutral />
          {memory.kind && <Pill label={memory.kind} seed={memory.kind} />}
          <TierBadge memory={memory} />
          {voteTier === 'verified' && <VoteTierBadge tier={voteTier} />}
          <span title={mintedTimeText} className="text-[11px] text-gray-500 dark:text-gray-400">{mintedTimeRelative}</span>
          {memory.score != null && (
            <span className="text-[11px] text-gray-500 dark:text-gray-400">score {memory.score.toFixed(2)}</span>
          )}
          {lastUpvoted && (
            <span className="text-[11px] text-gray-500 dark:text-gray-400">last upvoted {lastUpvoted}</span>
          )}
          {(memory.recalls ?? 0) > 0 && (
            <span className="text-[11px] text-gray-500 dark:text-gray-400">
              recalled {memory.recalls}× {lastRecalled ? `(last ${lastRecalled})` : ''}
            </span>
          )}
          {(memory.absorbed_ids?.length ?? 0) > 0 && (
            <span
              title={`Absorbed: ${memory.absorbed_ids!.join(', ')}`}
              className="inline-flex items-center px-1.5 py-0.5 rounded-full text-[11px] font-medium bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-400"
            >
              merged ×{memory.absorbed_ids!.length}
            </span>
          )}
        </div>
        {memory.status === 'invalidated' && memory.invalidation_reason && (
          <p
            title={memory.invalidation_reason}
            className="text-[11px] text-gray-500 dark:text-gray-400 mt-1 truncate"
          >
            {memory.invalidation_reason}
          </p>
        )}
      </div>
      {/* End of row, every viewport (#1266 owner follow-up) - not a left
          gutter, not folded into the metadata row - so the text column
          above always gets the row's full remaining width. */}
      <div className="flex-shrink-0 flex items-start gap-1">
        <VoteControl
          score={memory.vote_score ?? 0}
          ownVote={memory.own_vote}
          onVote={v => onVote(memory.id, v)}
        />
        <KebabMenu memory={memory} onForget={onForget} />
      </div>
    </div>
  )
})
