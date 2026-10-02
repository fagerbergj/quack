import { describe, it, expect } from 'vitest'
import { buildMermaid, involved, type DiagramSpec } from './diagramSource'
import { retrySpec } from './diagram.fixtures'

describe('buildMermaid', () => {
  const code = buildMermaid(retrySpec)

  it('emits one subgraph per layer holding its nodes', () => {
    expect(code.match(/^ {2}subgraph /gm)).toHaveLength(4)
    expect(code).toMatch(/subgraph l1\["Domain"\]\n {4}n1\["~ deliver\(\)"\]\n {4}n2\["\+ Retry worker"\]\n {4}n3\["- Drop on failure"\]\n {2}end/)
  })

  it('gives each edge a positional id and marks removed ones dotted', () => {
    expect(code).toContain('n1 e2@-->|"+ enqueue on 5xx"| n4')
    expect(code).toContain('n1 e5@-.-> n3')
  })

  it('styles by change with classDef, class and linkStyle', () => {
    expect(code).toContain('classDef added fill:#dcfce7')
    expect(code).toContain('class n2 added')
    expect(code).toContain('linkStyle 2 stroke:#16a34a')
    expect(code).not.toContain('click ')
  })

  it('escapes characters mermaid would read as syntax', () => {
    const spec: DiagramSpec = {
      layers: [{ id: 'a', title: 'A "x"', description: 'd' }],
      nodes: [{ id: 'n', label: 'a <b> #1 `c`', layer: 'a', detail: 'd' }],
      edges: [],
    }
    expect(buildMermaid(spec)).toContain('n0["a #lt;b#gt; #35;1 \'c\'"]')
    expect(buildMermaid(spec)).toContain('subgraph l0["A #quot;x#quot;"]')
  })

  it('skips edges whose endpoints are missing and keeps linkStyle aligned', () => {
    const spec: DiagramSpec = {
      layers: [{ id: 'a', title: 'A', description: 'd' }],
      nodes: [{ id: 'n', label: 'N', layer: 'a', detail: 'd' }, { id: 'm', label: 'M', layer: 'a', detail: 'd' }],
      edges: [{ id: 'bad', from: 'n', to: 'zz', detail: 'd' }, { id: 'ok', from: 'n', to: 'm', change: 'added', detail: 'd' }],
    }
    const out = buildMermaid(spec)
    expect(out).not.toContain('e0@')
    expect(out).toContain('linkStyle 0 stroke:#16a34a')
  })
})

describe('involved', () => {
  it('keeps a node, its edges, neighbours and their layers', () => {
    const s = involved(retrySpec, 'node', 'worker')
    expect([...s].sort()).toEqual([
      'edge:e_drain', 'edge:e_retry', 'layer:domain', 'layer:storage', 'node:deliver', 'node:queue', 'node:worker',
    ])
  })

  it('keeps an edge with its two endpoints', () => {
    const s = involved(retrySpec, 'edge', 'e_enqueue')
    expect(s.has('node:deliver') && s.has('node:queue') && s.has('edge:e_enqueue')).toBe(true)
    expect(s.has('node:worker')).toBe(false)
  })

  it('keeps a layer with its members and the edges between them', () => {
    const s = involved(retrySpec, 'layer', 'domain')
    expect(s.has('node:legacy') && s.has('edge:e_retry') && s.has('edge:e_drop')).toBe(true)
    expect(s.has('edge:e_send')).toBe(false)
  })
})
