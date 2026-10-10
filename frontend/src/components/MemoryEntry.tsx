import { memo, useEffect, useRef, useState } from 'react'
import type { Memory, VoteDirection } from '../api'
import { paletteClasses, TONE } from '../lib/colorHash'
import { relativeTime } from '../lib/relativeTime'
import { Icon } from './Icon'
import { VoteControl } from './VoteControl'

export interface MemoryEntryProps {
  memory: Memory
  onForget: (id: string) => Promise<void>
  onVote: (id: string, vote: VoteDirection) => Promise<void>
}

export type MemoryTier = 'unverified' | 'reinforced' | 'invalidated' | 'unknown'

// Missing status is a pre-lifecycle memory and reads as unverified by design. The switch is exhaustive, so
// a new enum status without a case fails to compile instead of silently showing "unverified".
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

// Unverified and unrecognized tiers stay neutral: never a colour that claims an unverified tier.
export function memoryTierBadgeClass(tier: MemoryTier): string {
  return tier === 'reinforced' ? TONE.green : tier === 'invalidated' ? TONE.red : TONE.gray
}

// Title mirrors the label, or the invalidation reason when present, so hover reveals it without the extra line.
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

// Vote-based tier, rendered only for 'verified': 'unverified' would duplicate TierBadge's default label.
function VoteTierBadge({ tier }: { tier: 'unverified' | 'verified' }) {
  return (
    <span className="inline-flex items-center px-1.5 py-0.5 rounded-full text-[11px] font-medium bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-400">
      {tier}
    </span>
  )
}

// Colour is hashed from the label and never the only signal. `neutral` opts out because a provenance node id
// can hash to red, which reads as an error.
function Pill({ label, seed, neutral }: { label: string; seed: string; neutral?: boolean }) {
  const cls = neutral ? TONE.gray : paletteClasses(seed)
  return (
    <span className={`inline-flex items-center px-1.5 py-0.5 rounded-full text-[11px] font-medium ${cls}`}>
      {label}
    </span>
  )
}

// Rows carry no full-text action buttons. Forget has its own confirm step so a misclick in the menu
// can't silently delete a fact.
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
                onClick={() => { void handleConfirm() }}
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

function MetaRow({ memory }: { memory: Memory }) {
  const voteTier = memory.tier ?? 'unverified'
  const lastUpvoted = relativeTime(memory.last_upvoted_at)
  const lastRecalled = relativeTime(memory.last_recalled_at)

  // Deliberately not memoized on memory.timestamp, which would freeze the relative label; memo(MemoryEntry)
  // already skips rows whose memory is unchanged.
  const mintedTime = new Date(memory.timestamp)
  const mintedTimeText = Number.isNaN(mintedTime.getTime()) ? memory.timestamp : mintedTime.toLocaleString()
  const mintedTimeRelative = relativeTime(memory.timestamp) ?? mintedTimeText

  return (
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
          className={`inline-flex items-center px-1.5 py-0.5 rounded-full text-[11px] font-medium ${TONE.purple}`}
        >
          merged ×{memory.absorbed_ids!.length}
        </span>
      )}
    </div>
  )
}

// memo: a vote changes one row of 20, so without it every row re-renders on any sibling's vote.
// Test-only render counter: the memo test asserts on counts, never on timings.
export const memoryEntryRenderProbe = { count: 0 }

export const MemoryEntry = memo(function MemoryEntry({ memory, onForget, onVote }: MemoryEntryProps) {
  memoryEntryRenderProbe.count++

  return (
    <div className="px-3 py-2.5 border-b border-gray-100 dark:border-gray-700 flex items-start gap-2">
      <div className="flex-1 min-w-0">
        <p className="text-sm text-gray-800 dark:text-gray-100 whitespace-pre-wrap break-words">{memory.content}</p>
        <MetaRow memory={memory} />
        {memory.status === 'invalidated' && memory.invalidation_reason && (
          <p
            title={memory.invalidation_reason}
            className="text-[11px] text-gray-500 dark:text-gray-400 mt-1 truncate"
          >
            {memory.invalidation_reason}
          </p>
        )}
      </div>
      {/* End of row on every viewport, not a gutter or the meta row, so the text column keeps the full width. */}
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
