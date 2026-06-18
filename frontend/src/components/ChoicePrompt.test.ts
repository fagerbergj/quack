import { describe, it, expect } from 'vitest'
import {
  startRun,
  appendRunToolCall,
  fillRunToolResult,
  pendingChoice,
  type AgentRun,
} from './messageParts'

// Build the run state the store holds after a get_user_choice call streams: the
// call, then its {status:"pending"} placeholder result (which marks it done).
function choiceRuns(options: unknown, question?: unknown): AgentRun[] {
  let runs = startRun([], { runId: 'orchestrator', agent: 'orchestrator', stage: 'worker' })
  runs = appendRunToolCall(runs, 'orchestrator', 'c1', 'get_user_choice', { options, question })
  runs = fillRunToolResult(runs, 'orchestrator', 'c1', 'get_user_choice', { status: 'pending' })
  return runs
}

describe('pendingChoice', () => {
  it('returns question + options + callId for a pending get_user_choice', () => {
    expect(pendingChoice(choiceRuns(['A', 'B'], 'Which one?'))).toEqual({ callId: 'c1', question: 'Which one?', options: ['A', 'B'] })
  })

  it('defaults question to empty string when absent', () => {
    expect(pendingChoice(choiceRuns(['A', 'B']))).toEqual({ callId: 'c1', question: '', options: ['A', 'B'] })
  })

  it('filters non-string options out', () => {
    expect(pendingChoice(choiceRuns(['A', 3, null, 'B']))).toEqual({ callId: 'c1', question: '', options: ['A', 'B'] })
  })

  it('returns null when the result is not the pending placeholder (answered)', () => {
    let runs = startRun([], { runId: 'orchestrator', agent: 'orchestrator', stage: 'worker' })
    runs = appendRunToolCall(runs, 'orchestrator', 'c1', 'get_user_choice', { options: ['A', 'B'] })
    runs = fillRunToolResult(runs, 'orchestrator', 'c1', 'get_user_choice', { choice: 'A' })
    expect(pendingChoice(runs)).toBeNull()
  })

  it('returns null for an ordinary tool call', () => {
    let runs = startRun([], { runId: 'orchestrator', agent: 'orchestrator', stage: 'worker' })
    runs = appendRunToolCall(runs, 'orchestrator', 'c1', 'web_search', { query: 'x' })
    expect(pendingChoice(runs)).toBeNull()
  })

  it('returns null when options are absent', () => {
    expect(pendingChoice(choiceRuns(undefined))).toBeNull()
  })
})
