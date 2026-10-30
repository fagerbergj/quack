import { Icon } from './Icon'
import type { MemoryListSort, MemoryScopeStats } from '../api'
import { cn } from '../lib/utils'
import { Button } from './ui/button'
import { Popover, PopoverContent, PopoverTrigger } from './ui/popover'
import { Separator } from './ui/separator'
import { Tooltip } from './ui/tooltip'

export type MemorySort = MemoryListSort

// Listed in popover order.
const SORT_OPTIONS: { value: MemorySort; label: string }[] = [
  { value: 'newest', label: 'Newest first' },
  { value: 'oldest', label: 'Oldest first' },
  { value: 'score', label: 'Highest score' },
  { value: 'upvotes', label: 'Most upvoted' },
  { value: 'downvotes', label: 'Most downvoted' },
  { value: 'recalls', label: 'Most recalled' },
  { value: 'last_recalled', label: 'Recently recalled' },
]

export type MemoryTierFilter = '' | 'unverified' | 'verified'

export interface MemorySortFilterProps {
  sort: MemorySort
  onSortChange: (sort: MemorySort) => void
  bucket: string
  buckets: string[]
  onBucketChange: (bucket: string) => void
  tier: MemoryTierFilter
  onTierChange: (tier: MemoryTierFilter) => void
  // Read-only live/invalidated counts per bucket, shown below the filters; not another filter.
  scopes?: MemoryScopeStats[]
}

// Same disclosure idiom as the chat sidebar's FilterPanel: icon button to popover, closed on backdrop click
// or Escape, so the filters take no toolbar width until opened.
export function MemorySortFilter({ sort, onSortChange, bucket, buckets, onBucketChange, tier, onTierChange, scopes }: MemorySortFilterProps) {
  const active = sort !== 'newest' || bucket !== '' || tier !== ''

  return (
    <div className="flex-shrink-0">
      <Popover>
        <Tooltip content="Sort and filter">
          <PopoverTrigger asChild>
            <Button
              variant="outline"
              size="icon"
              aria-haspopup="dialog"
              aria-label="Sort and filter memories"
              className={cn('size-11 medium:size-11', active && 'border-primary text-primary')}
            >
              <Icon name="tune" className="w-4 h-4" />
            </Button>
          </PopoverTrigger>
        </Tooltip>
        <PopoverContent align="end" aria-label="Sort and filter memories" className="medium:w-56">
          <div className="px-1 py-0.5 text-2xs font-semibold text-muted-foreground">Sort</div>
          {SORT_OPTIONS.map(({ value, label }) => (
            <label key={value} className="flex items-center gap-2 min-h-[44px] medium:min-h-0 px-1 py-1 rounded cursor-pointer hover:bg-accent">
              <input
                type="radio"
                name="memory-sort"
                checked={sort === value}
                onChange={() => onSortChange(value)}
                className="accent-blue-600"
              />
              <span className="text-foreground">{label}</span>
            </label>
          ))}
          <Separator className="mt-2 mb-1" />
          <label className="block px-1 py-0.5 text-2xs font-semibold text-muted-foreground">
            Bucket
          </label>
          <select
            value={bucket}
            onChange={e => onBucketChange(e.target.value)}
            aria-label="Bucket filter"
            className="w-full mt-0.5 min-h-[44px] medium:min-h-0 rounded border border-gray-300 dark:border-gray-600 px-2 py-1 text-xs bg-white dark:bg-gray-700 text-foreground focus:outline-none focus:ring-2 focus:ring-blue-500"
          >
            <option value="">All buckets</option>
            {buckets.map(b => <option key={b} value={b}>{b}</option>)}
          </select>
          <label className="block px-1 py-0.5 mt-2 text-2xs font-semibold text-muted-foreground">
            Tier
          </label>
          <select
            value={tier}
            onChange={e => onTierChange(e.target.value as MemoryTierFilter)}
            aria-label="Tier filter"
            className="w-full mt-0.5 min-h-[44px] medium:min-h-0 rounded border border-gray-300 dark:border-gray-600 px-2 py-1 text-xs bg-white dark:bg-gray-700 text-foreground focus:outline-none focus:ring-2 focus:ring-blue-500"
          >
            <option value="">All tiers</option>
            <option value="unverified">Unverified</option>
            <option value="verified">Verified</option>
          </select>
          {scopes && scopes.length > 0 && (
            <>
              <Separator className="mt-2 mb-1" />
              <div className="px-1 py-0.5 text-2xs font-semibold text-muted-foreground">
                Live / invalidated
              </div>
              <ul className="max-h-40 overflow-y-auto">
                {scopes.map(s => (
                  <li key={s.scope} className="px-1 py-0.5 text-foreground">
                    <div className="flex items-center justify-between gap-2">
                      <span className="truncate">{s.scope}</span>
                      <span className="flex-shrink-0 tabular-nums text-muted-foreground">{s.live} / {s.invalidated}</span>
                    </div>
                    <div
                      className="flex flex-wrap gap-x-2 gap-y-1 text-2xs tabular-nums text-muted-foreground"
                      title="Of the live points: never recalled, no up/down votes cast, verified with no real judge/human support"
                    >
                      <span>{s.never_recalled} never recalled</span>
                      <span>{s.no_votes} no up/down votes</span>
                      <span>{s.unsupported_verified} unsupported verified</span>
                    </div>
                  </li>
                ))}
              </ul>
            </>
          )}
        </PopoverContent>
      </Popover>
    </div>
  )
}
