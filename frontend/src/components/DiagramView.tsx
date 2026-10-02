import { useLayoutEffect, useMemo, useRef, useState, type FocusEvent, type KeyboardEvent, type ReactNode } from 'react'
import { AssistantText } from './AgentParts'
import { MermaidError, MermaidPending, useMermaidSvg } from './MermaidDiagram'
import {
  CHANGE_STYLE, buildMermaid, involved, selKey,
  type Change, type DiagramEdge, type DiagramNode, type DiagramSpec, type Kind,
} from './diagramSource'

export interface Sel { kind: Kind; id: string }

interface Target { roots: SVGElement[]; mark: SVGElement; orig: string | null }

const HIGHLIGHT = '#3b82f6'
const DIM = '0.25'
const MIN_WIDTH = 560

function target(registry: Map<string, Target>, key: string, el: SVGElement, roots: SVGElement[] = [el]) {
  registry.set(key, { roots, mark: el, orig: el.getAttribute('style') })
}

function interactive(el: Element, key: string, label: string) {
  el.setAttribute('data-qid', key)
  el.setAttribute('role', 'button')
  el.setAttribute('tabindex', '0')
  el.setAttribute('aria-label', label)
  el.setAttribute('aria-pressed', 'false')
  el.setAttribute('style', `${el.getAttribute('style') ?? ''};cursor:pointer`)
}

// Mermaid's flowchart ids: node g "<svg>-flowchart-<id>-<n>", cluster g "<svg>-<id>", edge path and label g data-id="<edge id>" (set by the `e0@-->` syntax).
// Edges get an invisible wide twin so a 1.5px line is clickable; the registry maps spec ids to the elements to dim and mark.
function decorate(host: HTMLElement, spec: DiagramSpec): Map<string, Target> {
  const reg = new Map<string, Target>()
  // Mermaid scales the svg to the column; below ~560px labels get unreadably small, so scroll instead.
  const svg = host.querySelector('svg')
  const natural = parseFloat(svg?.style.maxWidth ?? '')
  if (svg && natural) svg.style.minWidth = `${Math.min(natural, MIN_WIDTH)}px`
  host.querySelectorAll<SVGGElement>('g.node').forEach(g => {
    const n = spec.nodes[Number(/-flowchart-n(\d+)-\d+$/.exec(g.id)?.[1])]
    if (!n) return
    const key = selKey('node', n.id)
    const shape = g.querySelector<SVGElement>('rect, polygon, circle, ellipse, path') ?? g
    target(reg, key, shape, [g])
    interactive(g, key, `Node: ${n.label}`)
  })
  host.querySelectorAll<SVGGElement>('g.cluster').forEach(g => {
    const l = spec.layers[Number(/-l(\d+)$/.exec(g.id)?.[1])]
    if (!l) return
    const key = selKey('layer', l.id)
    target(reg, key, g.querySelector<SVGElement>('rect') ?? g, [g])
    interactive(g, key, `Layer: ${l.title}`)
  })
  host.querySelectorAll<SVGElement>('[data-id]').forEach(el => {
    const e = spec.edges[Number(/^e(\d+)$/.exec(el.getAttribute('data-id') ?? '')?.[1])]
    if (!e) return
    const key = selKey('edge', e.id)
    const from = spec.nodes.find(n => n.id === e.from)?.label
    const to = spec.nodes.find(n => n.id === e.to)?.label
    if (el.tagName.toLowerCase() === 'path') {
      const hit = el.cloneNode(false) as SVGElement
      for (const a of ['id', 'data-id', 'class', 'marker-end', 'marker-start']) hit.removeAttribute(a)
      hit.setAttribute('style', 'fill:none;stroke:transparent;stroke-width:16px;pointer-events:stroke;outline:none')
      el.after(hit)
      interactive(hit, key, `Connection: ${from} to ${to}`)
      const prev = reg.get(key)
      target(reg, key, el, [el, hit, ...(prev?.roots ?? [])])
    } else {
      el.setAttribute('data-qid', key)
      el.setAttribute('style', `${el.getAttribute('style') ?? ''};cursor:pointer`)
      const prev = reg.get(key)
      if (prev) prev.roots.push(el)
      else reg.set(key, { roots: [el], mark: el, orig: null })
    }
  })
  return reg
}

function paint(reg: Map<string, Target>, host: HTMLElement, sel: Sel | null, spec: DiagramSpec) {
  const keep = sel ? involved(spec, sel.kind, sel.id) : null
  const chosen = sel ? selKey(sel.kind, sel.id) : null
  for (const [key, t] of reg) {
    if (t.orig == null) t.mark.removeAttribute('style')
    else t.mark.setAttribute('style', t.orig)
    for (const r of t.roots) r.style.opacity = keep && !keep.has(key) ? DIM : ''
    if (key === chosen) {
      t.mark.style.setProperty('stroke', HIGHLIGHT, 'important')
      t.mark.style.setProperty('stroke-width', '3px', 'important')
      t.mark.scrollIntoView?.({ block: 'nearest', inline: 'center' })
    }
  }
  host.querySelectorAll('[data-qid][role="button"]').forEach(el => el.setAttribute('aria-pressed', String(el.getAttribute('data-qid') === chosen)))
}

function ChangeBadge({ change }: { change: Change }) {
  const s = CHANGE_STYLE[change]
  return <span className="rounded px-1.5 py-0.5 text-[11px] font-medium" style={{ background: s.fill, color: s.text, border: `1px solid ${s.stroke}` }}>{s.label}</span>
}

function Legend() {
  return (
    <ul aria-label="Legend" className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-gray-600 dark:text-gray-300">
      {(Object.keys(CHANGE_STYLE) as Change[]).map(c => (
        <li key={c} className="flex items-center gap-1.5">
          <span aria-hidden className={`inline-block h-3 w-4 rounded-sm border ${c === 'removed' ? 'border-dashed' : ''}`} style={{ background: CHANGE_STYLE[c].fill, borderColor: CHANGE_STYLE[c].stroke }} />
          {CHANGE_STYLE[c].label}{CHANGE_STYLE[c].mark && <span className="font-mono text-gray-400">({CHANGE_STYLE[c].mark})</span>}
        </li>
      ))}
    </ul>
  )
}

interface Info { title: string; detail: string; badges: ReactNode }

const Pill = ({ children }: { children: ReactNode }) => <span className="text-xs text-gray-500 dark:text-gray-400">{children}</span>

const TYPE_PILL = 'rounded bg-gray-100 px-1.5 py-0.5 text-[11px] dark:bg-gray-700'

function describeNode(spec: DiagramSpec, n: DiagramNode): Info {
  const badges = <>
    <Pill>{spec.layers.find(l => l.id === n.layer)?.title}</Pill>
    {n.change && <ChangeBadge change={n.change} />}
    {n.type && <span className={TYPE_PILL}>{n.type}</span>}
  </>
  return { title: n.label, detail: n.detail, badges }
}

function describeEdge(spec: DiagramSpec, e: DiagramEdge): Info {
  const label = (id: string) => spec.nodes.find(n => n.id === id)?.label
  const badges = <>{e.label && <Pill>{e.label}</Pill>}{e.change && <ChangeBadge change={e.change} />}</>
  return { title: `${label(e.from)} -> ${label(e.to)}`, detail: e.detail, badges }
}

function describe(spec: DiagramSpec, sel: Sel): Info | undefined {
  if (sel.kind === 'layer') {
    const l = spec.layers.find(l => l.id === sel.id)
    return l && { title: `Layer: ${l.title}`, detail: l.description, badges: null }
  }
  if (sel.kind === 'node') {
    const n = spec.nodes.find(n => n.id === sel.id)
    return n && describeNode(spec, n)
  }
  const e = spec.edges.find(e => e.id === sel.id)
  return e && describeEdge(spec, e)
}

function Panel({ spec, sel, onClear, onExplain }: { spec: DiagramSpec; sel: Sel | null; onClear: () => void; onExplain?: (s: Sel) => void }) {
  if (!sel) return <p className="text-xs text-gray-500 dark:text-gray-400">Select a layer, node or connection for details. Esc clears.</p>
  const info = describe(spec, sel)
  if (!info) return null
  const { title, detail, badges } = info
  return (
    <div className="space-y-2">
      <h4 className="text-sm font-semibold text-gray-900 dark:text-gray-100">{title}</h4>
      {badges && <div className="flex flex-wrap items-center gap-2">{badges}</div>}
      <AssistantText text={detail} />
      <div className="flex gap-2">
        {onExplain && <button type="button" onClick={() => onExplain(sel)} className="min-h-[44px] rounded-lg border border-gray-300 px-3 text-sm hover:bg-gray-50 dark:border-gray-600 dark:hover:bg-gray-700">Explain more</button>}
        <button type="button" onClick={onClear} className="min-h-[44px] rounded-lg border border-transparent px-3 text-sm text-blue-600 hover:underline dark:text-blue-400">Clear</button>
      </div>
    </div>
  )
}

export function DiagramView({ spec, onExplain }: { spec: DiagramSpec; onExplain?: (s: Sel) => void }) {
  const code = useMemo(() => buildMermaid(spec), [spec])
  const { svg, error } = useMermaidSvg(code)
  const host = useRef<HTMLDivElement>(null)
  const reg = useRef(new Map<string, Target>())
  const [sel, setSel] = useState<Sel | null>(null)

  useLayoutEffect(() => {
    if (host.current && svg) reg.current = decorate(host.current, spec)
    else reg.current = new Map()
    setSel(null)
  }, [svg, spec])
  useLayoutEffect(() => { if (host.current) paint(reg.current, host.current, sel, spec) }, [sel, svg, spec])

  const pick = (el: Element | null) => {
    const [kind, ...id] = (el?.closest('[data-qid]')?.getAttribute('data-qid') ?? '').split(':')
    if (id.length) setSel({ kind: kind as Kind, id: id.join(':') })
  }
  // The wide hit path is transparent, so keyboard focus is shown on the visible line it stands for.
  const onFocus = (e: FocusEvent<HTMLDivElement>) => {
    const el = e.target as Element
    const t = reg.current.get(el.getAttribute('data-qid') ?? '')
    if (t && el !== t.mark) t.mark.style.setProperty('stroke-width', '3px', 'important')
  }
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Escape') setSel(null)
    else if ((e.key === 'Enter' || e.key === ' ') && (e.target as Element).closest('[data-qid]')) { e.preventDefault(); pick(e.target as Element) }
  }

  if (error) return <MermaidError code={code} error={error} />
  if (!svg) return <MermaidPending />
  return (
    <div role="group" aria-label="Diagram" onKeyDown={onKeyDown} onFocus={onFocus} onBlur={() => host.current && paint(reg.current, host.current, sel, spec)} className="not-prose min-w-0 space-y-3">
      <div className="min-w-0 space-y-2">
        <div ref={host} onClick={e => pick(e.target as Element)} data-testid="diagram-svg" className="overflow-x-auto" dangerouslySetInnerHTML={{ __html: svg }} />
        <Legend />
      </div>
      <div aria-live="polite" className="min-w-0 rounded-lg border border-gray-200 p-3 dark:border-gray-700">
        <Panel spec={spec} sel={sel} onClear={() => setSel(null)} onExplain={onExplain} />
      </div>
    </div>
  )
}
