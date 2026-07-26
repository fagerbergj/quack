// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { AssistantText } from './AgentParts'

// jsdom doesn't implement SVG layout (getBBox); mermaid needs it during layout
// to size text labels - stub a fixed box on every SVG element (not just
// SVGGraphicsElement - jsdom's SVG class hierarchy is incomplete), we only
// care that render completes.
;(SVGElement.prototype as unknown as { getBBox: () => DOMRect }).getBBox = () =>
  ({ x: 0, y: 0, width: 100, height: 20, top: 0, right: 0, bottom: 0, left: 0, toJSON: () => '' }) as DOMRect

async function waitFor(check: () => boolean, timeoutMs = 8000) {
  const start = Date.now()
  while (!check()) {
    if (Date.now() - start > timeoutMs) throw new Error('timed out waiting for condition')
    await act(async () => {
      await new Promise(r => setTimeout(r, 20))
    })
  }
}

describe('AssistantText mermaid wiring', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  function mount(text: string) {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    act(() => {
      root!.render(createElement(AssistantText, { text }))
    })
  }

  it('renders a complete mermaid block as a diagram', async () => {
    mount('Here:\n\n```mermaid\nflowchart TD\n  A --> B\n```\n')
    await waitFor(() => host!.querySelector('[data-testid="mermaid-diagram"] svg') != null)
    expect(host!.querySelector('[data-testid="mermaid-diagram"] svg')).not.toBeNull()
  })

  it('does not attempt to render an unclosed (streaming) mermaid block', async () => {
    mount('Here:\n\n```mermaid\nflowchart TD\n  A --> B')
    // Give any (incorrect) async render attempt a chance to happen, then assert it didn't.
    await act(async () => {
      await new Promise(r => setTimeout(r, 200))
    })
    expect(host!.querySelector('[data-testid="mermaid-diagram"]')).toBeNull()
    expect(host!.textContent).not.toContain('Diagram failed to render')
    // Falls back to the plain streaming code block - the fence content is still visible as text.
    expect(host!.querySelector('pre')).not.toBeNull()
    expect(host!.textContent).toContain('flowchart TD')
  })

  // Regression: the streaming guard compared end.offset against a TRIMMED
  // text length. An open fence swallows the trailing newline, so end.offset
  // (31) != trimmed length (30) and the block was judged "not last" - the
  // guard fell open and rendered the half-written diagram. Real streams end
  // mid-line with a newline, so this is the ordinary case, not an edge one;
  // the case above (no trailing newline) passed even while this was broken.
  it('does not render an unclosed mermaid block that ends with a newline', async () => {
    mount('Here:\n\n```mermaid\nflowchart TD\n  A --> B\n')
    await act(async () => {
      await new Promise(r => setTimeout(r, 200))
    })
    expect(host!.querySelector('[data-testid="mermaid-diagram"]')).toBeNull()
    expect(host!.textContent).not.toContain('Diagram failed to render')
    expect(host!.querySelector('pre')).not.toBeNull()
  })

  // The mirror case: a CLOSED block followed by trailing blank lines must
  // still render - trailing whitespace after a complete fence is normal.
  it('renders a closed mermaid block followed by trailing newlines', async () => {
    mount('```mermaid\nflowchart TD\n  A --> B\n```\n\n')
    await act(async () => {
      await new Promise(r => setTimeout(r, 200))
    })
    expect(host!.querySelector('[data-testid="mermaid-diagram"]')).not.toBeNull()
  })

  it('renders normal markdown untouched when there is no mermaid block', () => {
    mount('**bold** text and a [link](https://example.com)')
    expect(host!.querySelector('strong')?.textContent).toBe('bold')
    expect(host!.querySelector('[data-testid="mermaid-diagram"]')).toBeNull()
  })
})
