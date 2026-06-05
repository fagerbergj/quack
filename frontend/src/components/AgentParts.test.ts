import { describe, it, expect } from 'vitest'
import {
  appendTextPart,
  appendThinkingPart,
  appendToolCall,
  fillToolResult,
  openAgent,
  closeAgent,
  appendSelfRefine,
  appendJudgeVerdict,
  partsToText,
  type MessagePart,
  type AgentPart,
} from './AgentParts'

// Helper: assert a part is an agent group and return it (narrows the type).
function agent(part: MessagePart): AgentPart {
  if (part.kind !== 'agent') throw new Error(`expected agent, got ${part.kind}`)
  return part
}

describe('messageParts tree reducers', () => {
  it('nests activity under the open actor group', () => {
    let parts: MessagePart[] = []
    parts = openAgent(parts, 'orchestrator')
    parts = appendThinkingPart(parts, 'deciding')
    parts = appendToolCall(parts, 'noop', {})

    expect(parts).toHaveLength(1)
    const orch = agent(parts[0])
    expect(orch.agent).toBe('orchestrator')
    expect(orch.done).toBe(false)
    expect(orch.items.map(i => i.kind)).toEqual(['thinking', 'tool_call'])
  })

  it('nests a dispatched agent inside its dispatcher', () => {
    let parts: MessagePart[] = []
    parts = openAgent(parts, 'orchestrator')
    parts = appendThinkingPart(parts, 'plan')
    parts = openAgent(parts, 'web-researcher')   // dispatch
    parts = appendToolCall(parts, 'web_search', { query: 'x' })
    parts = fillToolResult(parts, 'web_search', { results: [] })

    const orch = agent(parts[0])
    expect(orch.items.map(i => i.kind)).toEqual(['thinking', 'agent'])
    const wr = agent(orch.items[1])
    expect(wr.agent).toBe('web-researcher')
    expect(wr.items.map(i => i.kind)).toEqual(['tool_call'])
    const call = wr.items[0]
    if (call.kind === 'tool_call') expect(call.result).toEqual({ results: [] })
  })

  it('closeAgent closes the innermost open group, resuming the parent', () => {
    let parts: MessagePart[] = []
    parts = openAgent(parts, 'orchestrator')
    parts = openAgent(parts, 'web-researcher')
    parts = appendToolCall(parts, 'web_search', { query: 'x' })
    parts = closeAgent(parts, 'web-researcher')
    parts = appendToolCall(parts, 'after', {})   // back in the orchestrator

    const orch = agent(parts[0])
    const wr = agent(orch.items[0])
    expect(wr.done).toBe(true)
    // The post-dispatch tool lands in the orchestrator, not the closed researcher.
    expect(orch.items.map(i => i.kind)).toEqual(['agent', 'tool_call'])
  })

  it('closeAgent by name closes orphaned descendants when a nested agent_end is dropped', () => {
    let parts: MessagePart[] = []
    parts = openAgent(parts, 'orchestrator')
    parts = openAgent(parts, 'web-researcher')
    parts = appendToolCall(parts, 'web_search', { query: 'x' })
    // The researcher's agent_end never arrives; only the orchestrator's does.
    parts = closeAgent(parts, 'orchestrator')

    const orch = agent(parts[0])
    expect(orch.done).toBe(true)
    // Closing the named parent also closes the still-open child, so nothing spins.
    expect(agent(orch.items[0]).done).toBe(true)
  })

  it('nests self_refine and judge_verdict under the active agent and keeps them out of text', () => {
    let parts: MessagePart[] = []
    parts = openAgent(parts, 'orchestrator')
    parts = openAgent(parts, 'web-researcher')
    parts = appendSelfRefine(parts, true)
    parts = appendJudgeVerdict(parts, { round: 1, score: 0.4, passed: false, feedback: 'add sources' })
    parts = appendJudgeVerdict(parts, { round: 2, score: 0.8, passed: true, feedback: '' })
    parts = appendTextPart(parts, 'The vetted answer.')

    const wr = agent(agent(parts[0]).items[0])
    expect(wr.items.map(i => i.kind)).toEqual(['self_refine', 'judge_verdict', 'judge_verdict'])
    const last = wr.items[2]
    if (last.kind === 'judge_verdict') expect(last.passed).toBe(true)
    // Verdicts/refine never leak into the copyable answer text.
    expect(partsToText(parts)).toBe('The vetted answer.')
  })

  it('closeAgent for an unknown/already-closed agent is a no-op', () => {
    let parts: MessagePart[] = []
    parts = openAgent(parts, 'orchestrator')
    const before = parts
    parts = closeAgent(parts, 'web-researcher') // never opened
    expect(parts).toBe(before)
    expect(agent(parts[0]).done).toBe(false)
  })

  it('keeps answer/narrative text at the top level', () => {
    let parts: MessagePart[] = []
    parts = openAgent(parts, 'orchestrator')
    parts = openAgent(parts, 'web-researcher')
    parts = appendThinkingPart(parts, 'researching')
    parts = appendTextPart(parts, 'Here is the answer.')

    expect(parts.map(p => p.kind)).toEqual(['agent', 'text'])
    expect(partsToText(parts)).toBe('Here is the answer.')
  })

  it('coalesces streamed thinking within a group', () => {
    let parts: MessagePart[] = []
    parts = openAgent(parts, 'orchestrator')
    parts = appendThinkingPart(parts, 'Hello ')
    parts = appendThinkingPart(parts, 'world')
    const orch = agent(parts[0])
    expect(orch.items).toEqual([{ kind: 'thinking', text: 'Hello world' }])
  })
})
