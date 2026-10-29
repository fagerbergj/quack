import { memo, useState } from 'react'
import type { Memory, VoteDirection } from '../api'
import { paletteTone } from '../lib/colorHash'
import { Badge, type BadgeTone } from './ui/badge'
import { Button } from './ui/button'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from './ui/dropdown-menu'
import { Tooltip } from './ui/tooltip'
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
export function memoryTierTone(tier: MemoryTier): BadgeTone {
  return tier === 'reinforced' ? 'green' : tier === 'invalidated' ? 'red' : 'gray'
}

// Title mirrors the label, or the invalidation reason when present, so hover reveals it without the extra line.
function TierBadge({ memory }: { memory: Memory }) {
  const tier = memoryTier(memory)
  return (
    <Badge tone={memoryTierTone(tier)} title={memory.invalidation_reason ?? memoryTierLabel(memory)}>
      {memoryTierLabel(memory)}
    </Badge>
  )
}

// Vote-based tier, rendered only for 'verified': 'unverified' would duplicate TierBadge's default label.
function VoteTierBadge({ tier }: { tier: 'unverified' | 'verified' }) {
  return (
    <Badge tone="blue">{tier}</Badge>
  )
}

// Colour is hashed from the label and never the only signal. `neutral` opts out because a provenance node id
// can hash to red, which reads as an error.
function Pill({ label, seed, neutral }: { label: string; seed: string; neutral?: boolean }) {
  return <Badge tone={neutral ? 'gray' : paletteTone(seed)}>{label}</Badge>
}

// Rows carry no full-text action buttons. Forget has its own confirm step so a misclick in the menu
// can't silently delete a fact.
function KebabMenu({ memory, onForget }: { memory: Memory; onForget: (id: string) => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [forgetting, setForgetting] = useState(false)
  const [error, setError] = useState<string | null>(null)

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
    <DropdownMenu open={open} onOpenChange={o => { setOpen(o); if (!o) setConfirming(false) }}>
      <Tooltip content="More actions">
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon" aria-label="Memory actions">
            <Icon name="more_vert" className="w-4 h-4" />
          </Button>
        </DropdownMenuTrigger>
      </Tooltip>
      <DropdownMenuContent className="w-40">
        {error && <p className="px-3 py-1 text-red-700 dark:text-red-400">{error}</p>}
        {confirming ? (
          <div className="flex items-center gap-2 px-2 py-1">
            <Button variant="destructive" size="sm" onClick={() => { void handleConfirm() }} disabled={forgetting}>
              {forgetting ? 'Forgetting…' : 'Confirm'}
            </Button>
            <Button variant="outline" size="sm" onClick={() => setConfirming(false)} disabled={forgetting}>
              Cancel
            </Button>
          </div>
        ) : (
          // Stays open: Forget arms an inline Confirm rather than acting.
          <DropdownMenuItem onSelect={e => { e.preventDefault(); setConfirming(true) }}>
            <Icon name="delete" className="w-3.5 h-3.5" /> Forget
          </DropdownMenuItem>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
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
    <div className="flex flex-wrap items-center gap-2 mt-2">
      <Pill label={memory.bucket} seed={memory.bucket} />
      <Pill label={memory.author} seed={memory.author} neutral />
      {memory.kind && <Pill label={memory.kind} seed={memory.kind} />}
      <TierBadge memory={memory} />
      {voteTier === 'verified' && <VoteTierBadge tier={voteTier} />}
      <span title={mintedTimeText} className="text-2xs text-gray-500 dark:text-gray-400">{mintedTimeRelative}</span>
      {memory.score != null && (
        <span className="text-2xs text-gray-500 dark:text-gray-400">score {memory.score.toFixed(2)}</span>
      )}
      {lastUpvoted && (
        <span className="text-2xs text-gray-500 dark:text-gray-400">last upvoted {lastUpvoted}</span>
      )}
      {(memory.recalls ?? 0) > 0 && (
        <span className="text-2xs text-gray-500 dark:text-gray-400">
          recalled {memory.recalls}× {lastRecalled ? `(last ${lastRecalled})` : ''}
        </span>
      )}
      {(memory.absorbed_ids?.length ?? 0) > 0 && (
        <Badge tone="purple" title={`Absorbed: ${memory.absorbed_ids!.join(', ')}`}>
          merged ×{memory.absorbed_ids!.length}
        </Badge>
      )}
    </div>
  )
}

// memo: a vote changes one row of 20, so without it every row re-renders on any sibling's vote.
export const MemoryEntry = memo(function MemoryEntry({ memory, onForget, onVote }: MemoryEntryProps) {
  return (
    <div className="px-3 py-3 border-b border-gray-100 dark:border-gray-700 flex items-start gap-2">
      <div className="flex-1 min-w-0">
        <p className="text-sm text-gray-800 dark:text-gray-100 whitespace-pre-wrap break-words">{memory.content}</p>
        <MetaRow memory={memory} />
        {memory.status === 'invalidated' && memory.invalidation_reason && (
          <p
            title={memory.invalidation_reason}
            className="text-2xs text-gray-500 dark:text-gray-400 mt-1 truncate"
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
