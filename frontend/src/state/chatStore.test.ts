import { describe, it, expect } from 'vitest'
import { activityFromTurn } from './chatStore'
import { pendingChoice } from '../components/messageParts'
import type { Turn } from '../generated'

function turnWith(...output: Turn['output']): Turn {
  return { id: 't1', created_at: '', input: { role: 'user', content: 'hi' }, output }
}

describe('activityFromTurn', () => {
  it('reconstructs a synthetic orchestrator run from a quack:activity item', () => {
    const turn = turnWith({
      type: 'quack:activity', id: 't1:activity', status: 'completed',
      tool_calls: [
        { call_id: 'c1', name: 'web_search', args: { query: 'x' }, result: { hits: 2 } },
        { call_id: 'c2', name: 'get_user_choice', args: { options: ['A', 'B'] }, result: { status: 'pending' } },
      ],
    })
    const runs = activityFromTurn(turn)
    expect(runs).toHaveLength(1)
    expect(runs[0].runId).toBe('orchestrator')
    expect(runs[0].activity).toHaveLength(2)
    expect(runs[0].activity[0]).toEqual({
      kind: 'tool',
      tool: { callId: 'c1', name: 'web_search', args: { query: 'x' }, result: { hits: 2 }, done: true },
    })
    // The reconstructed run feeds pendingChoice the same way the live path does.
    expect(pendingChoice(runs)).toEqual({ callId: 'c2', options: ['A', 'B'] })
  })

  it('returns [] when the turn has no activity item', () => {
    expect(activityFromTurn(turnWith({ type: 'message', id: 'm', status: 'completed', content: [] }))).toEqual([])
  })
})
