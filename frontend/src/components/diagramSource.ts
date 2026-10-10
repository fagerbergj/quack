import type { DiagramSpec } from './A2uiSurface'

export type { DiagramSpec }
export type DiagramNode = DiagramSpec['nodes'][number]
export type DiagramEdge = DiagramSpec['edges'][number]
export type Change = NonNullable<DiagramNode['change']>

// Light fills with dark text read in both themes; the mark is the non-colour cue.
export const CHANGE_STYLE: Record<Change, { label: string; mark: string; fill: string; stroke: string; text: string }> = {
  added: { label: 'Added', mark: '+', fill: '#dcfce7', stroke: '#16a34a', text: '#14532d' },
  modified: { label: 'Modified', mark: '~', fill: '#fef3c7', stroke: '#d97706', text: '#78350f' },
  removed: { label: 'Removed', mark: '-', fill: '#fee2e2', stroke: '#dc2626', text: '#7f1d1d' },
  unchanged: { label: 'Unchanged', mark: '', fill: '#f3f4f6', stroke: '#9ca3af', text: '#1f2937' },
}

// Mermaid ids are positional (n0, e1, l2) so agent-chosen ids never reach the mermaid grammar.
const nodeKey = (i: number) => `n${i}`
const edgeKey = (i: number) => `e${i}`
const layerKey = (i: number) => `l${i}`

// `%` too: mermaid reads a `%%{init}%%` directive anywhere in the source, quoted or not.
const esc = (s: string) =>
  s.replace(/#/g, '#35;').replace(/%/g, '#37;').replace(/"/g, '#quot;').replace(/</g, '#lt;').replace(/>/g, '#gt;').replace(/`/g, "'").replace(/\s+/g, ' ').trim()

const text = (mark: string, label: string) => `"${esc(mark ? `${mark} ${label}` : label)}"`

export function buildMermaid(spec: DiagramSpec): string {
  const nodeIdx = new Map(spec.nodes.map((n, i) => [n.id, i]))
  const lines = [`flowchart ${spec.direction ?? 'TB'}`]
  spec.layers.forEach((l, li) => {
    const members = spec.nodes.flatMap((n, i) => (n.layer === l.id ? [`    ${nodeKey(i)}[${text(CHANGE_STYLE[n.change ?? 'unchanged'].mark, n.label)}]`] : []))
    if (members.length) lines.push(`  subgraph ${layerKey(li)}[${text('', l.title)}]`, ...members, '  end')
  })
  spec.edges.forEach((e, i) => {
    const from = nodeIdx.get(e.from)
    const to = nodeIdx.get(e.to)
    if (from == null || to == null) return
    const arrow = e.change === 'removed' ? '-.->' : '-->'
    const label = e.label?.trim() ? `|${text(CHANGE_STYLE[e.change ?? 'unchanged'].mark, e.label)}|` : ''
    lines.push(`  ${nodeKey(from)} ${edgeKey(i)}@${arrow}${label} ${nodeKey(to)}`)
  })
  // Invisible links between each layer's first node stack layers in declared order; appended last so the
  // real edges' positional ids and linkStyle indexes stay put.
  const anchors = spec.layers.flatMap(l => { const i = spec.nodes.findIndex(n => n.layer === l.id); return i < 0 ? [] : [nodeKey(i)] })
  anchors.slice(1).forEach((a, i) => lines.push(`  ${anchors[i]} ~~~ ${a}`))
  for (const [c, s] of Object.entries(CHANGE_STYLE)) {
    if (c === 'unchanged') continue
    const dash = c === 'removed' ? ',stroke-dasharray:5 3' : ''
    lines.push(`  classDef ${c} fill:${s.fill},stroke:${s.stroke},color:${s.text}${dash}`)
  }
  spec.nodes.forEach((n, i) => { if (n.change && n.change !== 'unchanged') lines.push(`  class ${nodeKey(i)} ${n.change}`) })
  let link = 0
  spec.edges.forEach(e => {
    if (!nodeIdx.has(e.from) || !nodeIdx.has(e.to)) return
    if (e.change && e.change !== 'unchanged') lines.push(`  linkStyle ${link} stroke:${CHANGE_STYLE[e.change].stroke},stroke-width:2px`)
    link++
  })
  return lines.join('\n')
}

export type Kind = 'node' | 'edge' | 'layer'
export const selKey = (kind: Kind, id: string) => `${kind}:${id}`

// The selected element plus what stays undimmed with it: neighbours for a node or edge, members for a layer.
export function involved(spec: DiagramSpec, kind: Kind, id: string): Set<string> {
  const out = new Set([selKey(kind, id)])
  const nodes = new Map(spec.nodes.map(n => [n.id, n]))
  const addNode = (n?: DiagramNode) => { if (n) { out.add(selKey('node', n.id)); out.add(selKey('layer', n.layer)) } }
  if (kind === 'layer') {
    const members = new Set(spec.nodes.filter(n => n.layer === id).map(n => n.id))
    members.forEach(m => addNode(nodes.get(m)))
    spec.edges.filter(e => members.has(e.from) && members.has(e.to)).forEach(e => out.add(selKey('edge', e.id)))
  } else if (kind === 'node') {
    addNode(nodes.get(id))
    for (const e of spec.edges.filter(e => e.from === id || e.to === id)) {
      out.add(selKey('edge', e.id))
      addNode(nodes.get(e.from))
      addNode(nodes.get(e.to))
    }
  } else {
    const e = spec.edges.find(e => e.id === id)
    if (e) { addNode(nodes.get(e.from)); addNode(nodes.get(e.to)) }
  }
  return out
}
