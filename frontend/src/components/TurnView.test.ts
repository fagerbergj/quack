import { describe, it, expect } from 'vitest'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { TurnView } from './TurnView'
import type { Turn } from '../generated'

// Structural assertions on the static markup - same approach as DagNode.test.ts
// (no testing-library in this repo).

const baseProps = {
  idx: 0,
  isChoiceAnswer: false,
  submittingChoice: false,
  isCopied: false,
  onChoice: () => {},
  onCopy: () => {},
  onDownload: () => {},
}

function turn(content: string, ...output: Turn['output']): Turn {
  return { id: 't1', created_at: '', input: { role: 'user', content }, output }
}

describe('TurnView - user bubble', () => {
  it('renders the user bubble when the turn has text', () => {
    const t = turn('Plan a 3-day Dublin trip')
    const out = renderToStaticMarkup(createElement(TurnView, { ...baseProps, turn: t }))
    expect(out).toContain('Plan a 3-day Dublin trip')
    expect(out).toContain('bg-blue-600')
  })

  // #434 - a label/webhook-triggered plan turn has no typed user text; the
  // empty blue pill that used to render for it is now skipped, while the
  // DAG bubble (the real synthesized-task content) still renders.
  it('skips the empty user bubble for a label-triggered plan turn, but keeps the DAG bubble', () => {
    const t = turn(
      '',
      {
        type: 'quack:dag', id: 'd1', status: 'completed', plan_id: 'p1',
        nodes: [{ id: 'n1', agent: 'web-researcher', task: 'Research Dublin', depends_on: [] }],
        edges: [],
        node_states: { n1: { status: 'done' } },
      },
    )
    const out = renderToStaticMarkup(createElement(TurnView, { ...baseProps, turn: t }))
    expect(out).not.toContain('bg-blue-600')
    expect(out).toContain('Plan')
  })
})
