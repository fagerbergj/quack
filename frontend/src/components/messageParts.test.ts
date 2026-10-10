import { describe, it, expect } from 'vitest'
import { showLiveSpinner, startRun, appendRunToolCall, fillRunToolResult, appendRunCompaction, type AgentRun } from './messageParts'

describe('showLiveSpinner', () => {
  it('shows dots while streaming before anything visible arrives', () => {
    expect(showLiveSpinner({ streaming: true, hasDag: false, answerText: '', visibleActivityCount: 0 })).toBe(true)
  })

  it('keeps dots when an empty orchestrator run exists but has no visible activity yet', () => {
    // The top-level run exists (empty) from the first event, so the spinner must key on visible activity
    // to stay up during the pre-plan gap.
    const runs: AgentRun[] = startRun([], { runId: 'orchestrator', agent: 'orchestrator', stage: 'worker' })
    expect(runs).toHaveLength(1)
    expect(runs[0].activity).toHaveLength(0)
    expect(showLiveSpinner({ streaming: true, hasDag: false, answerText: '', visibleActivityCount: runs[0].activity.length })).toBe(true)
  })

  it('hides dots once visible activity arrives', () => {
    expect(showLiveSpinner({ streaming: true, hasDag: false, answerText: '', visibleActivityCount: 1 })).toBe(false)
  })

  it('hides dots once the DAG or answer text arrives', () => {
    expect(showLiveSpinner({ streaming: true, hasDag: true, answerText: '', visibleActivityCount: 0 })).toBe(false)
    expect(showLiveSpinner({ streaming: true, hasDag: false, answerText: 'hi', visibleActivityCount: 0 })).toBe(false)
  })

  it('never shows dots when not streaming', () => {
    expect(showLiveSpinner({ streaming: false, hasDag: false, answerText: '', visibleActivityCount: 0 })).toBe(false)
  })
})

// Recorded ACP sequences where a call announces pending, then re-announces resolved under the same
// call_id; a second pushed row would leave the first permanently unresolved.
describe('appendRunToolCall / fillRunToolResult (tool-call orphaning, #746)', () => {
  it('a call whose name resolves between announce and pairing produces exactly one row', () => {
    // An execute call with no `command` yet is titled "bash"; the same call_id re-announces with the real command.
    let runs: AgentRun[] = startRun([], { runId: 'r1', agent: 'code-reviewer', stage: 'worker' })
    runs = appendRunToolCall(runs, 'r1', 'WSYgHgzEqOOLqF6e0oTpPqxqCJ7jRIS0', 'run_command', { command: 'bash' })
    runs = appendRunToolCall(runs, 'r1', 'WSYgHgzEqOOLqF6e0oTpPqxqCJ7jRIS0', 'run_command', { command: 'git diff main...HEAD --stat' })
    runs = fillRunToolResult(runs, 'r1', 'WSYgHgzEqOOLqF6e0oTpPqxqCJ7jRIS0', 'run_command', { exit_code: 0, output: '9 files changed, 229 insertions(+), 23 deletions(-)\n' })

    const tools = runs[0].activity.filter(a => a.kind === 'tool')
    expect(tools).toHaveLength(1)
    const tool = tools[0] as { kind: 'tool'; tool: { done: boolean; args: Record<string, unknown> } }
    expect(tool.tool.done).toBe(true)
    expect(tool.tool.args.command).toBe('git diff main...HEAD --stat')
  })

  it('an MCP call whose kind stays "other" throughout still resolves to one row', () => {
    // Identical name and args on both announcements: the orphaning comes from the duplicate push alone.
    let runs: AgentRun[] = startRun([], { runId: 'r1', agent: 'code-reviewer', stage: 'worker' })
    runs = appendRunToolCall(runs, 'r1', 'lINMRTME3tBIRdPWpW4bQTt3cyUks0nl', 'other', { title: 'quackmcp_load_memory' })
    runs = appendRunToolCall(runs, 'r1', 'lINMRTME3tBIRdPWpW4bQTt3cyUks0nl', 'other', { title: 'quackmcp_load_memory' })
    runs = fillRunToolResult(runs, 'r1', 'lINMRTME3tBIRdPWpW4bQTt3cyUks0nl', 'other', { output: 'The following notes were remembered...' })

    const tools = runs[0].activity.filter(a => a.kind === 'tool')
    expect(tools).toHaveLength(1)
    const tool = tools[0] as { kind: 'tool'; tool: { done: boolean } }
    expect(tool.tool.done).toBe(true)
  })
})

describe('appendRunCompaction', () => {
  it('appends a compaction row to the run matching its run_id', () => {
    let runs: AgentRun[] = startRun([], { runId: 'worker-r0', agent: 'web-researcher', stage: 'worker' })
    runs = appendRunCompaction(runs, 'worker-r0', { kind: 'compaction', summaryInputTokens: 210_000, summaryOutputTokens: 1_800 })
    expect(runs[0].activity).toEqual([{ kind: 'compaction', summaryInputTokens: 210_000, summaryOutputTokens: 1_800 }])
  })

  it('is a no-op when no run matches the run_id', () => {
    const runs: AgentRun[] = startRun([], { runId: 'worker-r0', agent: 'web-researcher', stage: 'worker' })
    expect(appendRunCompaction(runs, 'worker-r1', { kind: 'compaction' })).toBe(runs)
  })
})
