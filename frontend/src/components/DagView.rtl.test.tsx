// @vitest-environment jsdom
// Incremental planning (#slice3): execute() re-sends dag_plan with the same
// plan_id as the plan grows, and chatStore's onDagPlan merge (not reset)
// keeps earlier steps' node states - this proves the DAG view itself renders
// that growth correctly: node "a" stays visibly done while newly-added node
// "b" appears alongside it.
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render as rtlRender, screen } from '@testing-library/react'
import type { ReactElement } from 'react'
import { DagView } from './DagView'
import type { DagTurnState } from '../state/chatStore'
import { ChatStoreProvider } from '../state/ChatStoreProvider'

function render(ui: ReactElement) {
  return rtlRender(<ChatStoreProvider>{ui}</ChatStoreProvider>)
}

afterEach(cleanup)

const stepOne: DagTurnState = {
  planId: 'p',
  nodes: [{ id: 'a', agent: 'web-researcher', task: 'find the file', depends_on: [] }],
  edges: [],
  nodeStates: { a: { status: 'done', outputPreview: 'FOUND: file.go' } },
  nodeRuns: {},
  nodeAnswer: {},
}

// The SAME plan grown by a second execute() step: "a" unchanged, "b" added
// depending on it - exactly what a second dag_plan event carries.
const stepTwo: DagTurnState = {
  ...stepOne,
  nodes: [
    ...stepOne.nodes,
    { id: 'b', agent: 'code-implementer', task: 'add the comment', depends_on: ['a'] },
  ],
  edges: [{ from: 'a', to: 'b' }],
  nodeStates: { ...stepOne.nodeStates, b: { status: 'queued' } },
}

describe('DagView renders a growing plan', () => {
  it('keeps the done node visible and adds the new one on re-render with a grown dag', () => {
    const { rerender } = render(<DagView dag={stepOne} />)
    expect(screen.getByText('Web researcher')).toBeTruthy()
    expect(screen.queryByText('code-implementer')).toBeNull()

    rerender(<ChatStoreProvider><DagView dag={stepTwo} /></ChatStoreProvider>)

    // "a" is still there and its StatusDot still reads "done" - its card
    // was not wiped back to queued (the merge bug this pins).
    expect(screen.getByText('Web researcher')).toBeTruthy()
    expect(screen.getByText('done')).toBeTruthy()
    // "b" is the newly-added node, freshly queued.
    expect(screen.getByText('code-implementer')).toBeTruthy()
    expect(screen.getByText('queued')).toBeTruthy()
  })
})
