// @vitest-environment jsdom
import { describe, it, expect } from 'vitest'
import { buildMermaid, type DiagramSpec } from './diagramSource'
import { retrySpec } from './diagram.fixtures'

describe('generated source', () => {
  it('parses under the real mermaid grammar, including hostile labels', async () => {
    const { default: mermaid } = await import('mermaid')
    const nasty: DiagramSpec = {
      ...retrySpec,
      nodes: retrySpec.nodes.map((n, i) => (i === 0 ? { ...n, label: 'a "q" <b> #1 `c` [x] {y}' } : n)),
      layers: retrySpec.layers.map((l, i) => (i === 0 ? { ...l, title: "%%{init: {'themeVariables':{'primaryColor':'#f00'}}}%%" } : l)),
      edges: retrySpec.edges.map((e, i) => (i === 0 ? { ...e, label: 'p|q "r"' } : i === 1 ? { ...e, label: '  ' } : e)),
    }
    for (const spec of [retrySpec, nasty]) await expect(mermaid.parse(buildMermaid(spec))).resolves.toBeTruthy()
  }, 20_000)
})
