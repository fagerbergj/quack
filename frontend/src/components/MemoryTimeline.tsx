import type { Memory } from '../api'
import { MemoryEntry } from './MemoryEntry'

const DAY_MS = 24 * 60 * 60 * 1000

// AGE_BANDS is checked in order - the first band whose cutoff the memory's
// age is under wins. Grouping is by AGE ONLY (#746 item 14): NOT by
// retrieval usage, which doesn't exist as data yet (that's #747, a separate
// PR needing a schema field + a write on the recall path).
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

// groupByAge buckets memories (assumed already sorted by the caller) into
// age bands without breaking up runs of the same band - so old memories stay
// visibly grouped at the end regardless of the list's overall sort order.
export function groupByAge(memories: Memory[], now = Date.now()): AgeGroup[] {
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
  // Test/story seam: pins "now" so age-band assignment is deterministic.
  now?: number
}

// MemoryTimeline (#746 item 14): a vertical line down the left carries each
// entry's date, with entries grouped into age bands (Today / This week / This
// month / Older) so old memories are visibly at the end instead of mixed
// through a flat list.
export function MemoryTimeline({ memories, onForget, now }: MemoryTimelineProps) {
  const groups = groupByAge(memories, now)
  return (
    <div className="py-2">
      {groups.map(g => (
        <div key={`${g.label}-${g.memories[0]?.id}`}>
          <div className="pl-[4.75rem] pr-3 py-1 text-[10px] font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
            {g.label}
          </div>
          {g.memories.map(m => (
            <div key={m.id} className="flex">
              <div className="w-14 shrink-0 pt-3 pl-3 text-right text-[11px] text-gray-400 dark:text-gray-500 tabular-nums">
                {shortDate(m.timestamp)}
              </div>
              <div className="relative shrink-0 w-4 flex justify-center">
                <div className="absolute inset-y-0 w-px bg-gray-200 dark:bg-gray-700" />
                <span className="relative mt-[1.15rem] w-1.5 h-1.5 rounded-full bg-gray-300 dark:bg-gray-600 ring-2 ring-white dark:ring-gray-900" />
              </div>
              <div className="flex-1 min-w-0 pr-3">
                <MemoryEntry memory={m} onForget={onForget} />
              </div>
            </div>
          ))}
        </div>
      ))}
    </div>
  )
}
