import { Icon } from './Icon'
import { cn } from '../lib/utils'
import { Button } from './ui/button'
import { Popover, PopoverContent, PopoverTrigger } from './ui/popover'
import { Separator } from './ui/separator'

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

// The popover owns its open state; the selection lives in the parent so it can be mirrored to the URL.
export function FilterPanel({ facets, selected, onToggle, onClear }: FilterPanelProps) {
  const count = activeFilterCount(selected)

  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button
          variant="outline"
          size="icon"
          aria-label="Filter chats"
          className={cn('relative size-11 medium:size-11', count > 0 && 'border-primary text-primary')}
        >
          <Icon name="filter_alt" className="w-4 h-4" />
          {count > 0 && (
            <span className="absolute -top-1.5 -right-1.5 min-w-[15px] h-[15px] px-1 rounded-full bg-primary text-primary-foreground text-2xs font-semibold flex items-center justify-center">
              {count}
            </span>
          )}
        </Button>
      </PopoverTrigger>
      <PopoverContent aria-label="Filter chats" className="medium:w-56">
        <div className="flex items-center justify-between px-1 pb-2">
          <span className="font-semibold text-foreground">Filters</span>
          {count > 0 && (
            <Button variant="link" size="inline" onClick={onClear}>Clear all</Button>
          )}
        </div>
        <Separator className="mb-1" />
          {facets.length === 0 && (
            <div className="px-1 py-2 text-muted-foreground">No filters available</div>
          )}
          <div className="medium:max-h-80 medium:overflow-y-auto">
            {facets.map(f => (
              <div key={f.key} className="mb-2 last:mb-0">
                <div className="px-1 py-0.5 text-2xs font-semibold text-muted-foreground">
                  {f.label}
                </div>
                {f.options.map(o => {
                  const on = (selected[f.key] ?? []).includes(o.value)
                  return (
                    <label
                      key={o.value}
                      className="flex items-center gap-2 min-h-[44px] medium:min-h-0 px-1 py-1 rounded cursor-pointer hover:bg-accent"
                    >
                      <input
                        type="checkbox"
                        checked={on}
                        onChange={() => onToggle(f.key, o.value)}
                        className="accent-blue-600"
                      />
                      <span className="flex-1 text-foreground truncate">{o.label}</span>
                      {o.count != null && <span className="text-muted-foreground">{o.count}</span>}
                    </label>
                  )
                })}
              </div>
            ))}
          </div>
      </PopoverContent>
    </Popover>
  )
}
