// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { renderToStaticMarkup } from 'react-dom/server'
import { DagNode } from './DagNode'
import type { DagNodeDef } from '../state/agentStream'
import type { NodeState } from '../state/chatStore'
import type { AgentRun, Activity } from './messageParts'

// Structural assertions on the static markup - no testing-library in this repo
// (see ToolCallView.test.ts), so we render to HTML and check the load-bearing
// shape: collapsed-by-default one-line previews (the popup itself only opens
// on click, which needs a DOM - that's a Storybook play-function concern, see
// DagNode.stories.tsx's *Popup stories), and the deterministic-retry merge.

const node: DagNodeDef = { id: 'r1', agent: 'web-researcher', task: 'Research Dublin.', depends_on: [] }

function html(state: NodeState, runs: AgentRun[], answer: string): string {
  return renderToStaticMarkup(createElement(DagNode, { node, state, runs, answer, isFinal: false }))
}

const activity: Activity[] = [
  { kind: 'tool', tool: { callId: 'c1', name: 'web_search', args: { query: 'x' }, result: {}, done: true } },
]

describe('DagNode - judge verdict collapses to a one-line preview (#385/#399 ethos)', () => {
  const verdict = '**Mostly solid**, but add a source for the rainfall claim.'
  const out = html(
    { status: 'done', startedAt: 0, finishedAt: 1000 },
    [
      { runId: 'w', agent: 'web-researcher', stage: 'worker', done: true, activity },
      { runId: 'j1', agent: 'judge', stage: 'judge', round: 1, done: true, score: 0.5, passed: false, feedback: verdict, activity: [] },
    ],
    'answer text',
  )

  it('renders a clickable one-line "Verdict" preview, not standing prose', () => {
    expect(out).toMatch(/<button type="button"[^>]*><span class="italic shrink-0">Verdict<\/span>/)
    expect(out).toContain(verdict) // the raw preview text, not yet rendered as markdown
  })

  it('does not render the verdict as markdown inline (that only happens in the popup, on click)', () => {
    expect(out).not.toContain('<strong>Mostly solid</strong>')
    expect(out).not.toContain('role="dialog"')
  })
})

describe('DagNode - answer collapses to a one-line preview (#385/#399 ethos)', () => {
  const answer = '## Heading\n\nVisit in **May**.\n\n- one\n- two'
  const out = html({ status: 'done', startedAt: 0, finishedAt: 1000 }, [
    { runId: 'w', agent: 'web-researcher', stage: 'worker', done: true, activity },
  ], answer)

  it('renders a clickable one-line "answer" preview', () => {
    expect(out).toContain('<span class="shrink-0">answer</span>')
    expect(out).toContain('## Heading Visit in **May**. - one - two') // previewLine flattens whitespace
  })

  it('does not render the answer as markdown inline (that only happens in the popup, on click)', () => {
    expect(out).not.toContain('<h2>Heading</h2>')
    expect(out).not.toContain('role="dialog"')
  })
})

describe('DagNode - deterministic-retry continuation merges into one feed', () => {
  const retryActivity: Activity[] = [
    { kind: 'tool', tool: { callId: 'c9', name: 'run_command', args: { command: 'go test ./...' }, result: { exit_code: 0 }, done: true } },
  ]
  const out = html({ status: 'done', startedAt: 0, finishedAt: 1000 }, [
    { runId: 'worker-r0', agent: 'web-researcher', stage: 'worker', done: true, activity },
    { runId: 'worker-cont1', agent: 'web-researcher', stage: 'worker', done: true, activity: retryActivity },
  ], 'the answer')

  it('shows ONE merged tool-call count across both worker runs, not two separate blocks', () => {
    expect(out).toContain('2 tool calls')
    expect(out).not.toContain('1 tool call<')
  })

  it('a judge-triggered revise round still gets its own labeled block', () => {
    const withRevise = html({ status: 'done', startedAt: 0, finishedAt: 1000 }, [
      { runId: 'worker-r0', agent: 'web-researcher', stage: 'worker', done: true, activity },
      { runId: 'j1', agent: 'judge', stage: 'judge', round: 1, done: true, score: 0.4, passed: false, feedback: 'needs work', activity: [] },
      { runId: 'worker-r1', agent: 'web-researcher', stage: 'revise', round: 1, done: true, activity: retryActivity },
    ], 'the answer')
    expect(withRevise).toContain('Revised')
    expect(withRevise).toContain('1 tool call')
  })
})

// #746 item 4: only tool calls count as "steps" - a node with pure reasoning
// (no tool calls) shows no count at all, never a misleading "0 tool calls".
describe('DagNode - tool-call count excludes thinking traces (#746 item 4)', () => {
  it('counts only tool-kind activity, not thinking traces', () => {
    const mixed: Activity[] = [
      { kind: 'thinking', text: 'reasoning one' },
      { kind: 'tool', tool: { callId: 'c1', name: 'web_search', args: { query: 'x' }, result: {}, done: true } },
      { kind: 'thinking', text: 'reasoning two' },
      { kind: 'tool', tool: { callId: 'c2', name: 'web_fetch', args: { url: 'x' }, result: {}, done: true } },
    ]
    const out = html({ status: 'done', startedAt: 0, finishedAt: 1000 }, [
      { runId: 'w', agent: 'web-researcher', stage: 'worker', done: true, activity: mixed },
    ], 'the answer')
    expect(out).toContain('2 tool calls')
  })

  it('shows no count for a node with only thinking, no tool calls', () => {
    const thinkingOnly: Activity[] = [{ kind: 'thinking', text: 'just reasoning, no tools' }]
    const out = html({ status: 'done', startedAt: 0, finishedAt: 1000 }, [
      { runId: 'w', agent: 'web-researcher', stage: 'worker', done: true, activity: thinkingOnly },
    ], 'the answer')
    expect(out).not.toContain('tool call')
    expect(out).not.toContain('0 tool')
  })
})

// #426 - the judge verdict's popup CopyButton writes the full verdict text to
// the clipboard. Needs a real DOM (click-through), unlike the static-markup
// tests above.
describe('DagNode - judge verdict popup copy button (#426)', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  it('copies the full verdict text, not something else', () => {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })

    const verdict = 'Mostly solid, but the rainfall claim needs a source.'
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(DagNode, {
        node,
        state: { status: 'done', startedAt: 0, finishedAt: 1000 },
        runs: [
          { runId: 'w', agent: 'web-researcher', stage: 'worker', done: true, activity },
          { runId: 'j1', agent: 'judge', stage: 'judge', round: 1, done: true, score: 0.5, passed: false, feedback: verdict, activity: [] },
        ],
        answer: 'answer text',
        isFinal: false,
      }))
    })

    const previewButton = Array.from(host.querySelectorAll('button'))
      .find(b => b.textContent?.includes('Mostly solid'))!
    act(() => { previewButton.dispatchEvent(new MouseEvent('click', { bubbles: true })) })

    const copyButton = Array.from(host.querySelectorAll('button'))
      .find(b => b.getAttribute('aria-label')?.startsWith('Copy judge verdict'))!
    act(() => { copyButton.dispatchEvent(new MouseEvent('click', { bubbles: true })) })

    expect(writeText).toHaveBeenCalledWith(verdict)
  })
})

// Regression (#725): a running node with a busy tool-calling agent used to
// re-render its whole accumulated activity list on every streamed SSE event,
// locking the tab. A running node must show only a compact status line.
describe('DagNode - a running node collapses activity to a compact status line (#725)', () => {
  const manyToolCalls: Activity[] = Array.from({ length: 30 }, (_, i) => ({
    kind: 'tool' as const,
    tool: { callId: `c${i}`, name: 'edit_file', args: { path: `app/src/File${i}.kt` }, result: { ok: true }, done: true },
  }))
  const runningOut = html(
    { status: 'running', startedAt: 0 },
    [{ runId: 'w', agent: 'code-implementer', stage: 'worker', done: false, activity: manyToolCalls }],
    '',
  )

  it('shows only the most recent tool call, not all 30', () => {
    expect(runningOut).toContain('editing app/src/File29.kt')
    expect(runningOut).not.toContain('File0.kt')
    expect(runningOut).not.toContain('File15.kt')
  })

  it('does not mount the full activity list (no windowed "earlier" toggle)', () => {
    expect(runningOut).not.toContain('earlier')
  })

  it('a finished node keeps the full activity list reachable (windowed, not trimmed away)', () => {
    const doneOut = html(
      { status: 'done', startedAt: 0, finishedAt: 1000 },
      [{ runId: 'w', agent: 'code-implementer', stage: 'worker', done: true, activity: manyToolCalls }],
      '',
    )
    expect(doneOut).toContain('27 earlier')
  })
})

describe('DagNode - token badge shows cached tokens alongside the total', () => {
  it('shows the cached count when tokens were served from cache', () => {
    const out = html(
      { status: 'done', startedAt: 0, finishedAt: 1000, totalTokens: 1500, cachedTokens: 400 },
      [{ runId: 'w', agent: 'web-researcher', stage: 'worker', done: true, activity: [] }],
      'answer text',
    )
    expect(out).toContain('1,500 tok')
    expect(out).toContain('(400 cached)')
  })

  it('omits the cached annotation when nothing was cached', () => {
    const out = html(
      { status: 'done', startedAt: 0, finishedAt: 1000, totalTokens: 1500, cachedTokens: 0 },
      [{ runId: 'w', agent: 'web-researcher', stage: 'worker', done: true, activity: [] }],
      'answer text',
    )
    expect(out).toContain('1,500 tok')
    expect(out).not.toContain('cached')
  })
})

// #962: the ⋮ menu's Start/Pause/Stop verb enablement per node status - the
// wire's legal-transition table (dag.CanTransition), read off the opened
// menu's actual button labels.
describe('DagNode - verb enablement per status (#962)', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  function openMenuLabels(status: NodeState['status']): string[] {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(DagNode, {
        node, state: { status }, runs: [], answer: '', isFinal: false,
        onCancel: () => {}, onPause: () => {}, onResume: () => {}, onQueueMessage: () => {},
      }))
    })
    const toggle = Array.from(host.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Node actions')
    if (!toggle) return [] // no menu at all - terminal status
    act(() => { toggle.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    return Array.from(host.querySelectorAll('[role="menuitem"]')).map(b => b.textContent ?? '')
  }

  it('queued: Start + Stop, no Pause', () => {
    const labels = openMenuLabels('queued')
    expect(labels.some(l => l.includes('Start'))).toBe(true)
    expect(labels.some(l => l.includes('Stop'))).toBe(true)
    expect(labels.some(l => l.includes('Pause'))).toBe(false)
  })

  it('running: Pause + Stop, no Start', () => {
    const labels = openMenuLabels('running')
    expect(labels.some(l => l.includes('Pause'))).toBe(true)
    expect(labels.some(l => l.includes('Stop'))).toBe(true)
    expect(labels.some(l => l.includes('Start'))).toBe(false)
  })

  it('paused: Start + Stop, no Pause', () => {
    const labels = openMenuLabels('paused')
    expect(labels.some(l => l.includes('Start'))).toBe(true)
    expect(labels.some(l => l.includes('Stop'))).toBe(true)
    expect(labels.some(l => l.includes('Pause'))).toBe(false)
  })

  it('done/failed/cancelled: no ⋮ menu at all - nothing legal', () => {
    for (const status of ['done', 'failed', 'cancelled'] as const) {
      expect(openMenuLabels(status)).toEqual([])
    }
  })
})

describe('DagNode - context meter', () => {
  const meterNode: DagNodeDef = { ...node, context_window: 262_144 }

  function meterHtml(contextTokens: number, def = meterNode): string {
    return renderToStaticMarkup(createElement(DagNode, {
      node: def,
      state: { status: 'running', startedAt: 0, contextTokens },
      runs: [],
      answer: '',
      isFinal: false,
    }))
  }

  it('shows a compact used/limit reading once a round reports context tokens', () => {
    expect(meterHtml(156_000)).toContain('156K/262K')
  })

  it('colors the bar amber past 80% and red past 95% of the limit', () => {
    expect(meterHtml(220_000)).toContain('bg-amber-500') // 84%
    expect(meterHtml(255_000)).toContain('bg-red-500') // 97%
    expect(meterHtml(50_000)).not.toContain('bg-amber-500') // 19%, plain
    expect(meterHtml(50_000)).not.toContain('bg-red-500')
  })

  it('renders nothing for an agent with no configured context_window', () => {
    expect(meterHtml(156_000, node)).not.toContain('tokens of context used')
  })

  it('renders nothing before any round has reported context tokens', () => {
    const out = renderToStaticMarkup(createElement(DagNode, {
      node: meterNode, state: { status: 'running', startedAt: 0 }, runs: [], answer: '', isFinal: false,
    }))
    expect(out).not.toContain('tokens of context used')
  })
})
