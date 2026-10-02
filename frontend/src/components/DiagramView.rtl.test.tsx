// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { DiagramView } from './DiagramView'
import A2uiSurfaceView from './A2uiSurface'
import { retrySpec, retrySurface } from './diagram.fixtures'

// jsdom has no SVG layout, so mermaid can't run: stand in an SVG with mermaid's id scheme (checked against real output in the Storybook story).
vi.mock('./MermaidDiagram', async importOriginal => {
  const real = await importOriginal<typeof import('./MermaidDiagram')>()
  const fakeSvg = (code: string) => {
    const n = (code.match(/^ {4}n\d+\[/gm) ?? []).length
    const layers = (code.match(/subgraph l\d+/g) ?? []).length
    const edges = [...code.matchAll(/ (e\d+)@/g)].map(m => m[1])
    return '<svg id="mm">'
      + Array.from({ length: layers }, (_, i) => `<g class="cluster" id="mm-l${i}"><rect/></g>`).join('')
      + edges.map(e => `<path class="flowchart-link" id="mm-${e}" data-id="${e}" d="M0 0L9 9" marker-end="url(#m)"/><g class="label" data-id="${e}"><text>x</text></g>`).join('')
      + Array.from({ length: n }, (_, i) => `<g class="node default" id="mm-flowchart-n${i}-${i}"><rect/></g>`).join('')
      + '</svg>'
  }
  return { ...real, useMermaidSvg: (code: string) => ({ svg: fakeSvg(code), error: null }) }
})

afterEach(cleanup)

const panel = () => screen.getByRole('group', { name: 'Diagram' }).querySelector('[aria-live]') as HTMLElement
const dim = (name: string) => (screen.getByRole('button', { name }).style.opacity)

describe('DiagramView', () => {
  it('shows a legend entry per change kind', () => {
    render(<DiagramView spec={retrySpec} />)
    const items = within(screen.getByRole('list', { name: 'Legend' })).getAllByRole('listitem').map(li => li.textContent)
    expect(items).toEqual(['Added(+)', 'Modified(~)', 'Removed(-)', 'Unchanged'])
  })

  it('selects a node on click: shows its detail and dims everything but its neighbours', async () => {
    render(<DiagramView spec={retrySpec} />)
    expect(panel().textContent).toMatch(/Select a layer, node or connection/)
    await userEvent.setup().click(screen.getByRole('button', { name: 'Node: Retry worker' }))
    expect(panel().textContent).toMatch(/SKIP LOCKED/)
    expect(screen.getByRole('button', { name: 'Node: Retry worker' }).getAttribute('aria-pressed')).toBe('true')
    expect(dim('Node: retry_queue')).toBe('')
    expect(dim('Node: deliver()')).toBe('')
    expect(dim('Node: Receiver')).toBe('0.25')
    expect(dim('Node: POST /events')).toBe('0.25')
  })

  it('selects an edge through its wide hit path', async () => {
    const { container } = render(<DiagramView spec={retrySpec} />)
    await userEvent.setup().click(screen.getByRole('button', { name: 'Connection: deliver() to retry_queue' }))
    expect(panel().textContent).toMatch(/next_at = now \+ 2\^attempts/)
    expect(dim('Node: deliver()')).toBe('')
    expect(dim('Node: Retry worker')).toBe('0.25')
    const hit = container.querySelector('[data-qid="edge:e_enqueue"][role="button"]')!
    expect(hit.getAttribute('data-id')).toBeNull()
    expect(hit.getAttribute('marker-end')).toBeNull()
  })

  it('selects an edge through its label', async () => {
    const { container } = render(<DiagramView spec={retrySpec} />)
    await userEvent.setup().click(container.querySelector('g.label[data-qid="edge:e_drain"] text')!)
    expect(panel().textContent).toMatch(/Selects due rows/)
  })

  it('selects with Enter on a focused node and clears with Escape', async () => {
    const user = userEvent.setup()
    render(<DiagramView spec={retrySpec} />)
    screen.getByRole('button', { name: 'Node: deliver()' }).focus()
    await user.keyboard('{Enter}')
    expect(panel().textContent).toMatch(/typed `?RetryableError`?/)
    await user.keyboard('{Escape}')
    expect(panel().textContent).toMatch(/Select a layer/)
    expect(dim('Node: Receiver')).toBe('')
  })

  it('makes nodes reachable with Tab', async () => {
    const user = userEvent.setup()
    render(<DiagramView spec={retrySpec} />)
    await user.tab()
    expect(document.activeElement?.getAttribute('aria-label')).toMatch(/^(Layer|Node|Connection): /)
  })

  it('shows the layer description when a layer is clicked', async () => {
    render(<DiagramView spec={retrySpec} />)
    await userEvent.setup().click(screen.getByRole('button', { name: 'Layer: Storage' }))
    expect(panel().textContent).toMatch(/Postgres tables backing the retry queue/)
    expect(dim('Node: retry_queue')).toBe('')
    expect(dim('Node: Receiver')).toBe('0.25')
  })

  it('fires explain_focus with the element id and kind, and hides the button without a handler', async () => {
    const user = userEvent.setup()
    const onExplain = vi.fn()
    const { rerender } = render(<DiagramView spec={retrySpec} onExplain={onExplain} />)
    await user.click(screen.getByRole('button', { name: 'Node: Retry worker' }))
    await user.click(screen.getByRole('button', { name: 'Explain more' }))
    expect(onExplain).toHaveBeenCalledWith({ kind: 'node', id: 'worker' })
    rerender(<DiagramView spec={retrySpec} />)
    expect(screen.queryByRole('button', { name: 'Explain more' })).toBeNull()
  })
})

describe('Diagram and RiskTable in a surface', () => {
  it('dispatches explain_focus through the surface action, and Risk lists each change', async () => {
    const user = userEvent.setup()
    const onAction = vi.fn()
    render(<A2uiSurfaceView content={retrySurface} onAction={onAction} />)
    await user.click(screen.getByRole('button', { name: 'Connection: Retry worker to deliver()' }))
    await user.click(screen.getByRole('button', { name: 'Explain more' }))
    expect(onAction).toHaveBeenCalledWith(expect.objectContaining({
      name: 'explain_focus', source_component_id: 'flow', context: { element_id: 'e_retry', kind: 'edge' },
    }))
    await user.click(screen.getByRole('tab', { name: 'Risk' }))
    expect(screen.getAllByText(/ risk$/).map(e => e.textContent)).toEqual(['high risk', 'medium risk', 'low risk'])
    expect(screen.getByText(/Blast radius is diff-only/)).toBeTruthy()
    expect(screen.getByText('deliver_test.go covers 5xx and 404, not 429.')).toBeTruthy()
  })

  it('holds Explain more while a reply is in flight', async () => {
    render(<A2uiSurfaceView content={retrySurface} busy />)
    await userEvent.setup().click(screen.getByRole('button', { name: 'Node: Retry worker' }))
    expect(screen.queryByRole('button', { name: 'Explain more' })).toBeNull()
  })
})
