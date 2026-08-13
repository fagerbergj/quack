import { describe, it, expect } from 'vitest'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import {
  startRun,
  appendRunThinking,
  appendRunToolCall,
  fillRunToolResult,
  completeRun,
  freezeOpenRuns,
  ToolBlock,
  ActivityList,
  type AgentRun,
} from './AgentParts'
import type { Activity, ToolCall } from './messageParts'

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
    // r2's agent_complete never arrives (dropped/missing) - it's still counting.
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

  // #379: appendRunToolCall/fillRunToolResult/appendRunThinking used to spread
  // `run.activity` on every single event - O(run length) work per event, O(N²)
  // over a run of N events. They now mutate the array in place, so the same
  // array instance is reused across every append/fill rather than a fresh copy
  // being allocated each time. Pin that directly: the activity array reference
  // never changes across N events, which is only possible if events are O(1)
  // amortized (a per-event copy would produce a new array reference each time).
  it('appends/fills activity in place - no per-event copy of prior entries', () => {
    let runs: AgentRun[] = startRun([], { runId: 'r1', agent: 'w', stage: 'worker' })
    const activityRef = run(runs, 'r1').activity
    const N = 500
    for (let i = 0; i < N; i++) {
      runs = appendRunToolCall(runs, 'r1', `c${i}`, 'tool', { i })
      runs = fillRunToolResult(runs, 'r1', `c${i}`, 'tool', { ok: true })
      runs = appendRunThinking(runs, 'r1', `.`)
    }
    const finalRun = run(runs, 'r1')
    expect(finalRun.activity).toBe(activityRef) // same array throughout - never re-copied
    // Each thinking append is preceded by a tool call, so nothing coalesces:
    // N tool-call entries + N separate thinking entries.
    expect(finalRun.activity).toHaveLength(N * 2)
  })
})

// #746 item 6 - tool rows dropped their copy button as noise (the row still
// expands to ToolCallView's full detail on click).
describe('ToolBlock - no copy button (#746 item 6)', () => {
  const tool: ToolCall = { callId: 'c1', name: 'run_command', args: { command: 'go test ./...' }, result: { exit_code: 0 }, done: true }

  it('renders no button at all', () => {
    const out = renderToStaticMarkup(createElement(ToolBlock, { tool }))
    expect(out).not.toContain('<button')
  })
})

describe('ActivityList - compaction row', () => {
  it('renders a compacted before/after summary', () => {
    const activity: Activity[] = [{ kind: 'compaction', tokensBefore: 210_000, tokensAfter: 96_000 }]
    const out = renderToStaticMarkup(createElement(ActivityList, { activity }))
    expect(out).toContain('compacted')
    expect(out).toContain('210K')
    expect(out).toContain('96K')
  })

  it('carries a plain-English aria-label for screen readers', () => {
    const activity: Activity[] = [{ kind: 'compaction', tokensBefore: 210_000, tokensAfter: 96_000 }]
    const out = renderToStaticMarkup(createElement(ActivityList, { activity }))
    expect(out).toContain('aria-label="Context compacted from 210,000 to 96,000 tokens"')
  })
})
