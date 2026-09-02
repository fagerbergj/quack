// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { MermaidDiagram } from './MermaidDiagram'

// jsdom doesn't implement SVG layout (getBBox); mermaid needs it during layout
// to size text labels - stub a fixed box on every SVG element (not just
// SVGGraphicsElement - jsdom's SVG class hierarchy is incomplete), we only
// care that render completes.
;(SVGElement.prototype as unknown as { getBBox: () => DOMRect }).getBBox = () =>
  ({ x: 0, y: 0, width: 100, height: 20, top: 0, right: 0, bottom: 0, left: 0, toJSON: () => '' }) as DOMRect

// Must exceed waitFor's own budget below, or vitest kills the test while
// waitFor still thinks it has time - mermaid's ~1MB import is genuinely slow
// under full-suite concurrency.
const mermaidTestTimeout = 20_000

async function waitFor(check: () => boolean, timeoutMs = 8000) {
  const start = Date.now()
  while (!check()) {
    if (Date.now() - start > timeoutMs) throw new Error('timed out waiting for condition')
    await act(async () => {
      await new Promise(r => setTimeout(r, 20))
    })
  }
}

describe('MermaidDiagram', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  it('renders a valid diagram as an SVG', async () => {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(MermaidDiagram, { code: 'flowchart TD\n  A --> B' }))
    })

    await waitFor(() => host!.querySelector('[data-testid="mermaid-diagram"] svg') != null)

    expect(host.querySelector('[data-testid="mermaid-diagram"] svg')).not.toBeNull()
    expect(host.textContent).not.toContain('Diagram failed to render')
  }, mermaidTestTimeout)

  it('falls back to the source without throwing when the diagram is invalid', async () => {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)

    expect(() => {
      act(() => {
        root!.render(createElement(MermaidDiagram, { code: 'this is not @@@ %%% a diagram' }))
      })
    }).not.toThrow()

    await waitFor(() => host!.textContent?.includes('Diagram failed to render') ?? false)

    expect(host.querySelector('[data-testid="mermaid-diagram"]')).toBeNull()
    expect(host.querySelector('pre')).not.toBeNull() // CopyablePre fallback shows the raw source
    expect(host.textContent).toContain('this is not @@@ %%% a diagram')
  }, mermaidTestTimeout)
})
