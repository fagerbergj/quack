import { useState, useRef, useEffect } from 'react'

export interface FacetOption {
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

export function activeFilterCount(selected: Record<string, string[]>): number {
  return Object.values(selected).reduce((n, vs) => n + vs.length, 0)
}

// FilterPanel is a filter-icon button that opens a popover of facet groups
// (origin, status, repo, type…), each a multi-select checklist with counts -
// eBay-style faceted filtering. It owns only its open/closed state; the active
// selection lives in the parent (so it can be mirrored to the URL).
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
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && setOpen(false)
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => setOpen(o => !o)}
        aria-label="Filter chats"
        aria-expanded={open}
        className={`relative flex items-center justify-center rounded-lg border px-2 py-1.5 transition-colors ${
          count > 0
            ? 'border-blue-500 text-blue-600 dark:text-blue-400'
            : 'border-gray-300 dark:border-gray-600 text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700'
        }`}
      >
        {/* funnel icon */}
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true">
          <path d="M1.5 2.5h13l-5 6v4l-3 1.5V8.5l-5-6z" fill="currentColor" />
        </svg>
        {count > 0 && (
          <span className="absolute -top-1.5 -right-1.5 min-w-[15px] h-[15px] px-1 rounded-full bg-blue-600 text-white text-[9px] font-semibold flex items-center justify-center">
            {count}
          </span>
        )}
      </button>

      {open && (
        <div className="absolute z-50 mt-1 left-0 w-56 rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-lg p-2 text-xs">
          <div className="flex items-center justify-between px-1 pb-1.5 mb-1 border-b border-gray-100 dark:border-gray-700">
            <span className="font-semibold text-gray-700 dark:text-gray-200">Filters</span>
            {count > 0 && (
              <button onClick={onClear} className="text-[11px] text-blue-600 dark:text-blue-400 hover:underline">
                Clear all
              </button>
            )}
          </div>
          {facets.length === 0 && (
            <div className="px-1 py-2 text-gray-400 dark:text-gray-500">No filters available</div>
          )}
          <div className="max-h-80 overflow-y-auto">
            {facets.map(f => (
              <div key={f.key} className="mb-2 last:mb-0">
                <div className="px-1 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
                  {f.label}
                </div>
                {f.options.map(o => {
                  const on = (selected[f.key] ?? []).includes(o.value)
                  return (
                    <label
                      key={o.value}
                      className="flex items-center gap-2 px-1 py-1 rounded cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-700"
                    >
                      <input
                        type="checkbox"
                        checked={on}
                        onChange={() => onToggle(f.key, o.value)}
                        className="accent-blue-600"
                      />
                      <span className="flex-1 text-gray-700 dark:text-gray-200 truncate">{o.label}</span>
                      {o.count != null && <span className="text-gray-400 dark:text-gray-500">{o.count}</span>}
                    </label>
                  )
                })}
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
