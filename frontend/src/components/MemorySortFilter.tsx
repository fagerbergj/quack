import { useEffect, useRef, useState } from 'react'
import type { MemoryListSort } from '../api'

export type MemorySort = MemoryListSort

// SORT_OPTIONS labels every server-side sort (#1266 owner follow-up) - the
// order they list in here is the order they appear in the popover.
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
}

// MemorySortFilter (#746 items 11/15) combines sort and the bucket filter in
// one dialog, matching the disclosure pattern the chat sidebar's FilterPanel
// already uses (an icon button that opens a popover, closed on outside click
// or Escape) rather than inventing a second idiom. The bucket filter is a
// dropdown here (item 11), not the free-text input it used to be - it takes
// no horizontal space in the toolbar until opened.
export function MemorySortFilter({ sort, onSortChange, bucket, buckets, onBucketChange, tier, onTierChange }: MemorySortFilterProps) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  const active = sort !== 'newest' || bucket !== '' || tier !== ''

  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false) }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  return (
    <div ref={ref} className="relative flex-shrink-0">
      <button
        onClick={() => setOpen(o => !o)}
        aria-label="Sort and filter memories"
        aria-haspopup="dialog"
        aria-expanded={open}
        title="Sort and filter"
        className={`flex items-center justify-center min-w-[44px] min-h-[44px] rounded-lg border transition-colors ${
          active
            ? 'border-blue-500 text-blue-600 dark:text-blue-400'
            : 'border-gray-300 dark:border-gray-600 text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700'
        }`}
      >
        {/* sliders icon */}
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true">
          <path d="M2 4h12M4 4a1.5 1.5 0 1 0 3 0 1.5 1.5 0 0 0-3 0ZM2 8h7M9 8a1.5 1.5 0 1 0 3 0 1.5 1.5 0 0 0-3 0ZM2 12h9M11 12a1.5 1.5 0 1 0 3 0 1.5 1.5 0 0 0-3 0Z" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" />
        </svg>
      </button>

      {open && (
        <div role="dialog" aria-label="Sort and filter memories" className="absolute z-50 mt-1 right-0 w-56 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-lg p-2 text-xs">
          <div className="px-1 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">Sort</div>
          {SORT_OPTIONS.map(({ value, label }) => (
            <label key={value} className="flex items-center gap-2 px-1 py-1 rounded cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-700">
              <input
                type="radio"
                name="memory-sort"
                checked={sort === value}
                onChange={() => onSortChange(value)}
                className="accent-blue-600"
              />
              <span className="text-gray-700 dark:text-gray-200">{label}</span>
            </label>
          ))}
          <div className="mt-2 mb-1 border-t border-gray-100 dark:border-gray-700" />
          <label className="block px-1 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
            Bucket
          </label>
          <select
            value={bucket}
            onChange={e => onBucketChange(e.target.value)}
            aria-label="Bucket filter"
            className="w-full mt-0.5 rounded border border-gray-300 dark:border-gray-600 px-2 py-1 text-xs bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 focus:outline-none focus:ring-2 focus:ring-blue-500"
          >
            <option value="">All buckets</option>
            {buckets.map(b => <option key={b} value={b}>{b}</option>)}
          </select>
          <label className="block px-1 py-0.5 mt-2 text-[10px] font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
            Tier
          </label>
          <select
            value={tier}
            onChange={e => onTierChange(e.target.value as MemoryTierFilter)}
            aria-label="Tier filter"
            className="w-full mt-0.5 rounded border border-gray-300 dark:border-gray-600 px-2 py-1 text-xs bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 focus:outline-none focus:ring-2 focus:ring-blue-500"
          >
            <option value="">All tiers</option>
            <option value="unverified">Unverified</option>
            <option value="verified">Verified</option>
          </select>
        </div>
      )}
    </div>
  )
}
