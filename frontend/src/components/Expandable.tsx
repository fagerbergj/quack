import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'

// Expandable height-locks long content: it renders its children inside a capped
// container and, only when they overflow that cap, adds a fade + a Show more /
// Show less toggle. Content that fits shows no toggle. Used across the DAG view so
// a long answer, a many-round node, or a big tool body stays scannable instead of
// walling off the screen.
//
// The overflow decision is measured (scrollHeight vs the cap), re-checked after
// every render (so streamed content that grows past the cap reveals the toggle)
// and on container resize. The `fade` prop matches the underlying background so
// the gradient reads as a fade-to-nothing rather than a grey bar; default is the
// card background (white / gray-800).
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

  // Measure after every render: streamed content grows token-by-token, and each
  // growth re-renders, so this keeps `overflows` honest without an observer on the
  // (height-clamped, so size-frozen) inner box.
  useLayoutEffect(() => {
    const el = ref.current
    if (el) setOverflows(el.scrollHeight > maxHeight + 1)
  })

  // Re-measure on width changes (panel resize rewraps long lines).
  useEffect(() => {
    const el = ref.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(() => setOverflows(el.scrollHeight > maxHeight + 1))
    ro.observe(el)
    return () => ro.disconnect()
  }, [maxHeight])

  const collapsed = overflows && !expanded

  return (
    <div className={className}>
      <div
        ref={ref}
        style={collapsed ? { maxHeight } : undefined}
        className={collapsed ? 'relative overflow-hidden' : 'relative'}
      >
        {children}
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
