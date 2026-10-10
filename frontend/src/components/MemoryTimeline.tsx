import { useMemo } from 'react'
import type { Memory, VoteDirection } from '../api'
import { MemoryEntry } from './MemoryEntry'

const DAY_MS = 24 * 60 * 60 * 1000

// Checked in order; the first band whose cutoff the age is under wins. Grouping is by age only, since
// retrieval usage isn't recorded as data.
const AGE_BANDS: { label: string; underMs: number }[] = [
  { label: 'Today', underMs: DAY_MS },
  { label: 'This week', underMs: 7 * DAY_MS },
  { label: 'This month', underMs: 30 * DAY_MS },
  { label: 'Older', underMs: Infinity },
]

function bandLabel(timestamp: string, now: number): string {
  const age = now - new Date(timestamp).getTime()
  return (AGE_BANDS.find(b => age < b.underMs) ?? AGE_BANDS[AGE_BANDS.length - 1]).label
}

function shortDate(timestamp: string): string {
  const d = new Date(timestamp)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}

export interface AgeGroup {
  label: string
  memories: Memory[]
}

// Test-only call counter for the useMemo in MemoryTimeline.
export const groupByAgeProbe = { count: 0 }

// Assumes the caller already sorted by time; consecutive same-band memories share one group.
export function groupByAge(memories: Memory[], now = Date.now()): AgeGroup[] {
  groupByAgeProbe.count++
  const groups: AgeGroup[] = []
  for (const m of memories) {
    const label = bandLabel(m.timestamp, now)
    const last = groups[groups.length - 1]
    if (last && last.label === label) last.memories.push(m)
    else groups.push({ label, memories: [m] })
  }
  return groups
}

export interface MemoryTimelineProps {
  memories: Memory[]
  onForget: (id: string) => Promise<void>
  onVote: (id: string, vote: VoteDirection) => Promise<void>
  // Test/story seam: pins "now" so age-band assignment is deterministic.
  now?: number
  // false renders a flat list. Only the newest/oldest sorts are time-ordered; any other sort would give
  // groupByAge interleaved "Today...Older...Today" headers.
  grouped?: boolean
}

export function MemoryTimeline({ memories, onForget, onVote, now, grouped = true }: MemoryTimelineProps) {
  // Memoized so unrelated state changes (e.g. a vote) don't regroup the page.
  const groups = useMemo(
    () => (grouped ? groupByAge(memories, now) : [{ label: '', memories }]),
    [grouped, memories, now],
  )
  return (
    <div className="py-2">
      {groups.map((g, i) => (
        <div key={g.label ? `${g.label}-${g.memories[0]?.id}` : `flat-${i}`}>
          {g.label && (
            <div className="pl-3 medium:pl-[4.75rem] pr-3 py-1 text-[11px] font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">
              {g.label}
            </div>
          )}
          {g.memories.map(m => (
            <div key={m.id} className="flex">
              {/* Gutter collapses below `medium`: the row's relative-time chip already carries the date. */}
              <div className="hidden medium:block w-14 shrink-0 pt-3 pl-3 text-right text-[11px] text-gray-500 dark:text-gray-400 tabular-nums">
                {shortDate(m.timestamp)}
              </div>
              <div className="hidden medium:flex relative shrink-0 w-4 justify-center">
                <div className="absolute inset-y-0 w-px bg-gray-200 dark:bg-gray-700" />
                <span className="relative mt-[1.15rem] w-1.5 h-1.5 rounded-full bg-gray-300 dark:bg-gray-600 ring-2 ring-white dark:ring-gray-900" />
              </div>
              <div className="flex-1 min-w-0 pr-3">
                <MemoryEntry memory={m} onForget={onForget} onVote={onVote} />
              </div>
            </div>
          ))}
        </div>
      ))}
    </div>
  )
}
