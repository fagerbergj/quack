import { useLayoutEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'

// Expandable height-locks long content: it renders its children inside a capped
// container and, only when they overflow that cap, adds a fade + a Show more /
// Show less toggle. Content that fits shows no toggle. Used across the DAG view so
// a long answer, a many-round node, or a big tool body stays scannable instead of
// walling off the screen.
//
// The overflow decision is measured (scrollHeight vs the cap) on the CONTENT box —
// never on the clamped box. Clamping changes that box's own layout (it becomes a
// block formatting context and its full height leaves the scrolling ancestor), so
// measuring the box we clamp feeds the decision back into its own input: measure
// tall → clamp → measure short → unclamp → … Re-measured on every commit that
// ping-pong is an unbounded chain of nested updates, and a streaming turn (hundreds
// of token renders) hits React's guard — "Maximum update depth exceeded" (#185) —
// which blanks the whole chat tree. The content box is never clamped, so its height
// is independent of the decision, and one ResizeObserver on it covers both re-measure
// triggers: streamed content growing, and width changes that rewrap long lines.
//
// The `fade` prop matches the underlying background so the gradient reads as a
// fade-to-nothing rather than a grey bar; default is the card background.
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
          className="mt-1 text-[11px] font-medium text-blue-600 dark:text-blue-400 hover:underline focus:outline-none focus:underline"
        >
          {expanded ? 'Show less' : 'Show more'}
        </button>
      )}
    </div>
  )
}
