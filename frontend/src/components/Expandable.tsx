import { useLayoutEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'

// Height-locks long content: children in a capped container; fade +
// Show more / Show less toggle only on overflow (used across the DAG view so a
// long answer or big tool body stays scannable). The decision is measured (scrollHeight vs the cap) on the CONTENT box, never the clamped box - clamping changes layout, so measuring the clamped box feeds the decision back into its own input, ping-ponging into React's max-update-depth guard on a streaming turn (#185). The content box is never clamped; one ResizeObserver on it covers both re-measure triggers (content growing, width rewrap).
export function Expandable({
  children,
  maxHeight = 240,
  fade = 'from-white dark:from-gray-800',
  className = '',
}: {
  children: ReactNode
  maxHeight?: number
  // Tailwind gradient colour-stop classes for the fade, matched to the surface
  // the content sits on (e.g. 'from-gray-50 dark:from-gray-900' for code blocks).
  fade?: string
  className?: string
}) {
  const ref = useRef<HTMLDivElement>(null)
  const [expanded, setExpanded] = useState(false)
  const [overflows, setOverflows] = useState(false)

  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const measure = () => setOverflows(el.scrollHeight > maxHeight + 1)
    measure()
    if (typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [maxHeight])

  const collapsed = overflows && !expanded

  return (
    <div className={className}>
      <div
        style={collapsed ? { maxHeight } : undefined}
        className={collapsed ? 'relative overflow-hidden' : 'relative'}
      >
        <div ref={ref}>{children}</div>
        {collapsed && (
          <div className={`pointer-events-none absolute inset-x-0 bottom-0 h-8 bg-gradient-to-t to-transparent ${fade}`} />
        )}
      </div>
      {overflows && (
        <button
          type="button"
          onClick={() => setExpanded(e => !e)}
          aria-expanded={expanded}
          className="min-h-[44px] -my-2 inline-flex items-center text-[11px] font-medium text-blue-600 dark:text-blue-400 hover:underline focus:outline-none focus:underline"
        >
          {expanded ? 'Show less' : 'Show more'}
        </button>
      )}
    </div>
  )
}
