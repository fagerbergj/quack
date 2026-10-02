import { memo, useEffect, useId, useState } from 'react'
import type { MermaidConfig } from 'mermaid'
import { CopyablePre } from './CopyablePre'
import { Icon } from './Icon'

// The mermaid package is large (~1MB+ minified) and most chat messages never
// contain a diagram - load it only when a ```mermaid block actually renders,
// and only once per page (module-level cache), not once per diagram.
let mermaidPromise: Promise<typeof import('mermaid')> | undefined
function loadMermaid() {
  mermaidPromise ??= import('mermaid')
  return mermaidPromise
}

// Mermaid renders SVG generated from model-authored (untrusted) diagram
// text. 'strict' runs mermaid's own sanitize pass over the generated SVG -
// script tags, foreignObject, click/href bindings stripped - the same trust boundary rehype-sanitize enforces for markdown HTML, applied to mermaid's own output.
const BASE_CONFIG: Partial<MermaidConfig> = { startOnLoad: false, securityLevel: 'strict' }

function useIsDarkMode(): boolean {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains('dark'))
  useEffect(() => {
    const el = document.documentElement
    const observe = () => setDark(el.classList.contains('dark'))
    const observer = new MutationObserver(observe)
    observer.observe(el, { attributes: true, attributeFilter: ['class'] })
    return () => observer.disconnect()
  }, [])
  return dark
}

// Renders on `code` or theme change only. A parse/render failure never throws: callers get `error` and show the source.
export function useMermaidSvg(code: string): { svg: string | null; error: string | null } {
  const reactId = useId().replace(/[^a-zA-Z0-9]/g, '')
  const dark = useIsDarkMode()
  const [svg, setSvg] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setSvg(null)
    setError(null)
    loadMermaid()
      .then(async ({ default: mermaid }) => {
        // A directive that fails (e.g. a bad themeVariables colour) stays in mermaid's global list and breaks every later render.
        mermaid.mermaidAPI.reset()
        mermaid.initialize({ ...BASE_CONFIG, theme: dark ? 'dark' : 'default' })
        const { svg } = await mermaid.render(`mermaid-${reactId}`, code)
        if (!cancelled) setSvg(svg)
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      })
    return () => { cancelled = true }
  }, [code, dark, reactId])

  return { svg, error }
}

export function MermaidError({ code, error }: { code: string; error: string }) {
  return (
    <div className="not-prose">
      <div className="mb-1 flex items-center gap-1 text-[11px] text-amber-600 dark:text-amber-400" title={error}>
        <Icon name="warning" className="w-3.5 h-3.5" /> Diagram failed to render - showing source
      </div>
      <CopyablePre><code className="language-mermaid">{code}</code></CopyablePre>
    </div>
  )
}

export function MermaidPending() {
  return (
    <div className="not-prose py-2 text-[11px] text-gray-500 dark:text-gray-400" role="status">
      Rendering diagram…
    </div>
  )
}

// Memoized so an unrelated token arriving elsewhere in a streaming message doesn't re-render or re-parse every diagram already on screen.
export const MermaidDiagram = memo(function MermaidDiagram({ code }: { code: string }) {
  const { svg, error } = useMermaidSvg(code)
  if (error) return <MermaidError code={code} error={error} />
  if (!svg) return <MermaidPending />
  // mermaid's own 'strict' securityLevel already sanitized this SVG string
  // (see BASE_CONFIG above) - safe to inject directly.
  return <div className="not-prose my-2 overflow-x-auto" data-testid="mermaid-diagram" dangerouslySetInnerHTML={{ __html: svg }} />
})
