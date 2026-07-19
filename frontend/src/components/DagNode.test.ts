import { describe, it, expect } from 'vitest'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { DagNode } from './DagNode'
import type { DagNodeDef } from '../state/agentStream'
import type { NodeState } from '../state/chatStore'
import type { AgentRun, Activity } from './messageParts'

// Structural assertions on the static markup — no testing-library in this repo
// (see ToolCallView.test.ts), so we render to HTML and check the load-bearing
// shape: collapsed-by-default one-line previews (the popup itself only opens
// on click, which needs a DOM — that's a Storybook play-function concern, see
// DagNode.stories.tsx's *Popup stories), and the deterministic-retry merge.

const node: DagNodeDef = { id: 'r1', agent: 'web-researcher', task: 'Research Dublin.', depends_on: [] }

function html(state: NodeState, runs: AgentRun[], answer: string): string {
  return renderToStaticMarkup(createElement(DagNode, { node, state, runs, answer, isFinal: false }))
}

const activity: Activity[] = [
  { kind: 'tool', tool: { callId: 'c1', name: 'web_search', args: { query: 'x' }, result: {}, done: true } },
]

describe('DagNode — judge verdict collapses to a one-line preview (#385/#399 ethos)', () => {
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

describe('DagNode — answer collapses to a one-line preview (#385/#399 ethos)', () => {
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

describe('DagNode — deterministic-retry continuation merges into one feed', () => {
  const retryActivity: Activity[] = [
    { kind: 'tool', tool: { callId: 'c9', name: 'run_command', args: { command: 'go test ./...' }, result: { exit_code: 0 }, done: true } },
  ]
  const out = html({ status: 'done', startedAt: 0, finishedAt: 1000 }, [
    { runId: 'worker-r0', agent: 'web-researcher', stage: 'worker', done: true, activity },
    { runId: 'worker-cont1', agent: 'web-researcher', stage: 'worker', done: true, activity: retryActivity },
  ], 'the answer')

  it('shows ONE merged step count across both worker runs, not two separate blocks', () => {
    expect(out).toContain('2 steps')
    expect(out).not.toContain('1 step<')
  })

  it('a judge-triggered revise round still gets its own labeled block', () => {
    const withRevise = html({ status: 'done', startedAt: 0, finishedAt: 1000 }, [
      { runId: 'worker-r0', agent: 'web-researcher', stage: 'worker', done: true, activity },
      { runId: 'j1', agent: 'judge', stage: 'judge', round: 1, done: true, score: 0.4, passed: false, feedback: 'needs work', activity: [] },
      { runId: 'worker-r1', agent: 'web-researcher', stage: 'revise', round: 1, done: true, activity: retryActivity },
    ], 'the answer')
    expect(withRevise).toContain('Revised')
    expect(withRevise).toContain('1 step')
  })
})
