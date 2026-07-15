import { describe, it, expect } from 'vitest'
import { liveDagFinalText } from './Chat'
import type { DagTurnState } from '../state/chatStore'

// dag builds a minimal single-node DagTurnState (that node is the terminal node).
function dag(nodeAnswer: Record<string, string>): DagTurnState {
  return {
    planId: 'p',
    nodes: [{ id: 'a', agent: 'researcher', task: 't', depends_on: [] }],
    edges: [],
    nodeStates: {},
    nodeRuns: {},
    nodeAnswer,
  }
}

// Regression: the answer bubble briefly showed the orchestrator's mid-processing
// narration (top-level `live.text`) before flipping to the terminal node's real
// answer once it arrived. The fix is that a DAG turn's answer text is ALWAYS the
// terminal node's nodeAnswer — orchestrator narration must never occupy it, even
// as a fallback while the node answer is still empty.
describe('liveDagFinalText — no mid-stream flip to orchestrator narration', () => {
  it("is empty while the terminal node's answer hasn't arrived yet, even with orchestrator narration present", () => {
    const d = dag({})
    expect(liveDagFinalText(d)).toBe('')
  })

  it("renders the terminal node's answer once set", () => {
    const d = dag({ a: 'the real answer' })
    expect(liveDagFinalText(d)).toBe('the real answer')
  })
})

// This mirrors the `liveText` selection in Chat.tsx: for a DAG turn it must be
// exactly liveDagFinalText(dag) — never `|| liveTopText`, which is what caused
// the flicker (narration shown, then replaced by the real answer).
describe('liveText selection (Chat.tsx local formula)', () => {
  function liveText(liveDag: DagTurnState | undefined, liveTopText: string): string {
    return liveDag ? liveDagFinalText(liveDag) : liveTopText
  }

  it('never shows orchestrator narration for a DAG turn, before or after the node answer arrives', () => {
    const narration = 'orchestrator is planning the request...'
    const midStream = dag({}) // terminal node answer still empty
    expect(liveText(midStream, narration)).toBe('')

    const settled = dag({ a: 'final answer' })
    expect(liveText(settled, narration)).toBe('final answer')
  })

  it('falls back to top-level text when there is no DAG at all (plain orchestrator reply)', () => {
    expect(liveText(undefined, 'a direct reply')).toBe('a direct reply')
  })
})
