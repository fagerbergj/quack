import { describe, it, expect } from 'vitest'
import {
  startRun,
  appendRunThinking,
  appendRunToolCall,
  fillRunToolResult,
  completeRun,
  freezeOpenRuns,
  type AgentRun,
} from './AgentParts'

function run(runs: AgentRun[], runId: string): AgentRun {
  const r = runs.find(x => x.runId === runId)
  if (!r) throw new Error(`run ${runId} not found`)
  return r
}

describe('run-model reducers', () => {
  it('startRun appends a run and is idempotent on run_id', () => {
    let runs: AgentRun[] = []
    runs = startRun(runs, { runId: 'r1', agent: 'web-researcher', stage: 'worker' })
    runs = startRun(runs, { runId: 'r1', agent: 'web-researcher', stage: 'worker' }) // dup ignored
    expect(runs).toHaveLength(1)
    expect(run(runs, 'r1').stage).toBe('worker')
    expect(run(runs, 'r1').done).toBe(false)
  })

  it('attributes activity to the named run, coalescing thinking', () => {
    let runs: AgentRun[] = []
    runs = startRun(runs, { runId: 'r1', agent: 'w', stage: 'worker' })
    runs = appendRunThinking(runs, 'r1', 'chunk 1 ')
    runs = appendRunThinking(runs, 'r1', 'chunk 2')
    expect(run(runs, 'r1').activity).toEqual([{ kind: 'thinking', text: 'chunk 1 chunk 2' }])
  })

  it('pairs a tool result to its call by call_id', () => {
    let runs: AgentRun[] = []
    runs = startRun(runs, { runId: 'r1', agent: 'w', stage: 'worker' })
    runs = appendRunToolCall(runs, 'r1', 'c1', 'web_search', { query: 'x' })
    runs = fillRunToolResult(runs, 'r1', 'c1', 'web_search', { results: [] })
    const a = run(runs, 'r1').activity[0]
    expect(a.kind).toBe('tool')
    if (a.kind === 'tool') {
      expect(a.tool.done).toBe(true)
      expect(a.tool.result).toEqual({ results: [] })
    }
  })

  it('completeRun records stage-specific verdict fields', () => {
    let runs: AgentRun[] = []
    runs = startRun(runs, { runId: 'r2', agent: 'judge', stage: 'judge', round: 1 })
    runs = completeRun(runs, 'r2', { score: 0.4, passed: false, feedback: 'add sources' })
    const r = run(runs, 'r2')
    expect(r.done).toBe(true)
    expect(r.passed).toBe(false)
    expect(r.score).toBe(0.4)
    expect(r.feedback).toBe('add sources')
  })

  it('freezeOpenRuns marks a still-open run done so its timer stops at node completion', () => {
    let runs: AgentRun[] = []
    runs = startRun(runs, { runId: 'r1', agent: 'w', stage: 'worker', startedAt: 0 })
    runs = completeRun(runs, 'r1', {}, 5_000) // worker finished cleanly
    runs = startRun(runs, { runId: 'r2', agent: 'w', stage: 'revise', round: 1, startedAt: 6_000 })
    // r2's agent_complete never arrives (dropped/missing) — it's still counting.
    expect(run(runs, 'r2').done).toBe(false)

    runs = freezeOpenRuns(runs, 20_000) // node finishes

    expect(run(runs, 'r2').done).toBe(true)
    expect(run(runs, 'r2').durationMs).toBe(14_000) // 20_000 - 6_000
    // The already-complete run is untouched.
    expect(run(runs, 'r1').durationMs).toBe(5_000)
  })

  it('freezeOpenRuns returns the same array when every run is already done (no needless re-render)', () => {
    let runs: AgentRun[] = []
    runs = startRun(runs, { runId: 'r1', agent: 'w', stage: 'worker', startedAt: 0 })
    runs = completeRun(runs, 'r1', {}, 5_000)
    expect(freezeOpenRuns(runs, 9_000)).toBe(runs)
  })

  it('keeps runs independent and ordered (worker → judge → revise → judge)', () => {
    let runs: AgentRun[] = []
    runs = startRun(runs, { runId: 'r1', agent: 'w', stage: 'worker' })
    runs = completeRun(runs, 'r1', { finishReason: 'STOP' })
    runs = startRun(runs, { runId: 'r2', agent: 'judge', stage: 'judge', round: 1 })
    runs = completeRun(runs, 'r2', { score: 0.4, passed: false })
    runs = startRun(runs, { runId: 'r3', agent: 'w', stage: 'revise', round: 1 })
    runs = completeRun(runs, 'r3', {})
    runs = startRun(runs, { runId: 'r4', agent: 'judge', stage: 'judge', round: 2 })
    runs = completeRun(runs, 'r4', { score: 0.8, passed: true })

    expect(runs.map(r => r.stage)).toEqual(['worker', 'judge', 'revise', 'judge'])
    expect(run(runs, 'r3').stage).toBe('revise') // revise is its own run, not under judge
    expect(run(runs, 'r4').passed).toBe(true)
  })

  it('ignores activity for an unknown run', () => {
    let runs: AgentRun[] = []
    runs = startRun(runs, { runId: 'r1', agent: 'w', stage: 'worker' })
    const before = runs
    runs = appendRunThinking(runs, 'nope', 'x')
    expect(runs).toBe(before)
  })
})
