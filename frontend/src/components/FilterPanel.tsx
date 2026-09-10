import { Icon } from './Icon'
import { useState, useRef, useEffect } from 'react'
import { Sheet } from './Sheet'

interface FacetOption {
  value: string
  label: string
  count?: number
}

export interface Facet {
  key: string
  label: string
  options: FacetOption[]
}

export interface FilterPanelProps {
  facets: Facet[]
  // selected values per facet key
  selected: Record<string, string[]>
  onToggle: (facetKey: string, value: string) => void
  onClear: () => void
}

function activeFilterCount(selected: Record<string, string[]>): number {
  return Object.values(selected).reduce((n, vs) => n + vs.length, 0)
}

// A filter-icon button that opens a popover of facet groups (origin, status,
// repo, type…), each a multi-select checklist with counts. Owns only its
// open/closed state; the active selection lives in the parent (so it can be mirrored to the URL).
export function FilterPanel({ facets, selected, onToggle, onClear }: FilterPanelProps) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  const count = activeFilterCount(selected)

  // Close on outside click / Escape.
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [open])

  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => setOpen(o => !o)}
        aria-label="Filter chats"
        aria-expanded={open}
        className={`relative flex items-center justify-center rounded-lg border min-w-[44px] min-h-[44px] transition-colors ${
          count > 0
            ? 'border-blue-500 text-blue-600 dark:text-blue-400'
            : 'border-gray-300 dark:border-gray-600 text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700'
        }`}
      >
        <Icon name="filter_alt" className="w-4 h-4" />
        {count > 0 && (
          <span className="absolute -top-1.5 -right-1.5 min-w-[15px] h-[15px] px-1 rounded-full bg-blue-600 text-white text-[11px] font-semibold flex items-center justify-center">
            {count}
          </span>
        )}
      </button>

      {open && (
        <Sheet anchored aria-label="Filter chats" onClose={() => setOpen(false)} className="medium:absolute medium:left-0 medium:mt-1 medium:w-56 medium:rounded-lg medium:border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 p-2 medium:pb-2 text-sm medium:text-xs">
          <div className="flex items-center justify-between px-1 pb-1.5 mb-1 border-b border-gray-100 dark:border-gray-700">
            <span className="font-semibold text-gray-700 dark:text-gray-200">Filters</span>
            {count > 0 && (
              <button onClick={onClear} className="min-h-[24px] inline-flex items-center text-[11px] text-blue-600 dark:text-blue-400 hover:underline">
                Clear all
              </button>
            )}
          </div>
          {facets.length === 0 && (
            <div className="px-1 py-2 text-gray-500 dark:text-gray-400">No filters available</div>
          )}
          <div className="medium:max-h-80 medium:overflow-y-auto">
            {facets.map(f => (
              <div key={f.key} className="mb-2 last:mb-0">
                <div className="px-1 py-0.5 text-[11px] font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">
                  {f.label}
                </div>
                {f.options.map(o => {
                  const on = (selected[f.key] ?? []).includes(o.value)
                  return (
                    <label
                      key={o.value}
                      className="flex items-center gap-2 min-h-[44px] medium:min-h-0 px-1 py-1 rounded cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-700"
                    >
                      <input
                        type="checkbox"
                        checked={on}
                        onChange={() => onToggle(f.key, o.value)}
                        className="accent-blue-600"
                      />
                      <span className="flex-1 text-gray-700 dark:text-gray-200 truncate">{o.label}</span>
                      {o.count != null && <span className="text-gray-500 dark:text-gray-400">{o.count}</span>}
                    </label>
                  )
                })}
              </div>
            ))}
          </div>
        </Sheet>
      )}
    </div>
  )
}
