// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render as rtlRender, screen } from '@testing-library/react'
import type { ReactElement } from 'react'
import { DagNode } from './DagNode'
import type { DagNodeDef } from '../state/agentStream'
import { ChatStoreProvider } from '../state/ChatStoreProvider'

function render(ui: ReactElement) {
  return rtlRender(<ChatStoreProvider>{ui}</ChatStoreProvider>)
}

afterEach(cleanup)

describe('DagNode continue: line', () => {
  it('shows "continues <node>" when the node declares one', () => {
    const node: DagNodeDef = { id: 'n2', agent: 'code-implementer', task: 'extend it', depends_on: [], continue: 'n1' }
    render(<DagNode node={node} state={{ status: 'running' }} runs={[]} answer="" isFinal={false} />)
    expect(screen.getByText(/continues n1/)).toBeTruthy()
  })

  it('shows nothing for a fresh node (no continue field)', () => {
    const node: DagNodeDef = { id: 'n1', agent: 'code-implementer', task: 'do it', depends_on: [] }
    render(<DagNode node={node} state={{ status: 'running' }} runs={[]} answer="" isFinal={false} />)
    expect(screen.queryByText(/continues/)).toBeNull()
  })
})
