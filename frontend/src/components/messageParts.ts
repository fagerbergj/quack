// Each node runs a sequence of agent "runs" delimited by agent_start/agent_complete. Everything keys on
// run_id and tools pair by call_id, never open-container heuristics. No JSX, so it stays trivially testable.

export type Stage = 'worker' | 'judge' | 'revise'

// Shared by every header that names an agent so it reads the same everywhere.
export function agentLabel(name: string): string {
  if (name === 'web-researcher') return 'Web researcher'
  if (name === 'synthesizer') return 'Synthesizer'
  if (name === 'orchestrator') return 'Orchestrator'
  return name
}

export interface ToolCall {
  callId: string
  name: string
  args: Record<string, unknown>
  result?: unknown
  done: boolean
}

// Compaction fields mirror stream.CompactionData: adk reports no before/after context size, only a time
// range and the summarizer's spend, both optional.
export type Activity =
  | { kind: 'thinking'; text: string }
  | { kind: 'tool'; tool: ToolCall }
  | {
      kind: 'compaction'
      startTimestamp?: string
      endTimestamp?: string
      summaryInputTokens?: number
      summaryOutputTokens?: number
    }

// Result fields are populated on agent_complete and vary by stage.
export interface AgentRun {
  runId: string
  agent: string
  stage: Stage
  round?: number
  activity: Activity[]
  // Lets appendRunThinking fold a delta into the last thinking item in O(1), even across tool calls.
  lastThinkIdx?: number
  done: boolean
  startedAt?: number    // ms timestamp when the run opened
  durationMs?: number   // set on complete
  // results (set on complete)
  score?: number       // judge
  passed?: boolean     // judge
  threshold?: number   // judge: the score a round must reach to pass
  feedback?: string    // judge
  status?: string      // '' ok | 'unavailable' (judge unreachable) | 'no_verdict' (judge ran, never committed one)
  reason?: string
  finishReason?: string // worker
  model?: string
  totalTokens?: number
}

export interface PendingChoice {
  callId: string
  question: string
  options: string[]
}

// get_user_choice completes with a `{status:"pending"}` placeholder; the answer arrives as a later turn and
// never overwrites it, so a call still holding the placeholder is awaiting input.
export function pendingChoice(runs: AgentRun[]): PendingChoice | null {
  for (const r of runs) {
    for (const a of r.activity) {
      if (a.kind !== 'tool' || a.tool.name !== 'get_user_choice') continue
      const status = (a.tool.result as { status?: string } | undefined)?.status
      const options = a.tool.args.options
      if (status === 'pending' && Array.isArray(options)) {
        const question = typeof a.tool.args.question === 'string' ? a.tool.args.question : ''
        return { callId: a.tool.callId, question, options: options.filter((o): o is string => typeof o === 'string') }
      }
    }
  }
  return null
}

// Idempotent on run_id: a duplicate start is ignored.
export function startRun(runs: AgentRun[], r: { runId: string; agent: string; stage: Stage; round?: number; startedAt?: number }): AgentRun[] {
  if (runs.some(x => x.runId === r.runId)) return runs
  return [...runs, { runId: r.runId, agent: r.agent, stage: r.stage, round: r.round, activity: [], done: false, startedAt: r.startedAt }]
}

// Mutates run.activity in place so a run of N events costs O(N), not O(N²); the run itself still gets a
// new identity so callers see a fresh reference.
export function appendRunThinking(runs: AgentRun[], runId: string, text: string): AgentRun[] {
  return mapRun(runs, runId, run => {
    const idx = run.lastThinkIdx
    const existing = idx != null ? run.activity[idx] : undefined
    if (existing && existing.kind === 'thinking') {
      run.activity[idx as number] = { kind: 'thinking', text: existing.text + text }
    } else {
      run.lastThinkIdx = run.activity.length
      run.activity.push({ kind: 'thinking', text })
    }
    return { ...run }
  })
}

// An ACP call re-announces under the same call_id once its args resolve; update that row rather than push
// a second, since the result fills only the latest match and would orphan the first.
export function appendRunToolCall(runs: AgentRun[], runId: string, callId: string, name: string, args: Record<string, unknown>): AgentRun[] {
  return mapRun(runs, runId, run => {
    const idx = callId === '' ? -1 : run.activity.findIndex(a => a.kind === 'tool' && !a.tool.done && a.tool.callId === callId)
    if (idx >= 0) {
      run.activity[idx] = { kind: 'tool', tool: { callId, name, args, done: false } }
      return { ...run }
    }
    run.activity.push({ kind: 'tool', tool: { callId, name, args, done: false } })
    return { ...run }
  })
}

// With no call_id, falls back to the most recent pending call of the same name.
export function fillRunToolResult(runs: AgentRun[], runId: string, callId: string, name: string, result: unknown): AgentRun[] {
  return mapRun(runs, runId, run => {
    let idx = -1
    for (let i = run.activity.length - 1; i >= 0; i--) {
      const a = run.activity[i]
      if (a.kind === 'tool' && !a.tool.done && (a.tool.callId === callId || (callId === '' && a.tool.name === name))) {
        idx = i
        break
      }
    }
    if (idx < 0) return run
    const a = run.activity[idx] as { kind: 'tool'; tool: ToolCall }
    run.activity[idx] = { kind: 'tool', tool: { ...a.tool, result, done: true } }
    return { ...run }
  })
}

export function completeRun(runs: AgentRun[], runId: string, data: Partial<AgentRun>, nowMs?: number): AgentRun[] {
  return mapRun(runs, runId, run => {
    const durationMs = nowMs != null && run.startedAt != null ? nowMs - run.startedAt : run.durationMs
    return { ...run, ...data, done: true, durationMs }
  })
}

// Backstop for a node that finishes while a run's agent_complete was dropped, reordered or never sent.
export function freezeOpenRuns(runs: AgentRun[], nowMs?: number): AgentRun[] {
  if (!runs.some(r => !r.done)) return runs
  return runs.map(run => {
    if (run.done) return run
    const durationMs = nowMs != null && run.startedAt != null ? nowMs - run.startedAt : run.durationMs
    return { ...run, done: true, durationMs }
  })
}

// The backend sends quack's run_id (stream.RunIDFromBranch), the one agent_start carries, not adk's invocation id.
export function appendRunCompaction(runs: AgentRun[], runId: string, data: Extract<Activity, { kind: 'compaction' }>): AgentRun[] {
  return mapRun(runs, runId, run => ({ ...run, activity: [...run.activity, data] }))
}

function mapRun(runs: AgentRun[], runId: string, fn: (run: AgentRun) => AgentRun): AgentRun[] {
  let found = false
  const next = runs.map(run => {
    if (run.runId !== runId) return run
    found = true
    return fn(run)
  })
  return found ? next : runs
}

// `tool` is the latest tool call even when the tail has gone back to thinking.
export interface LiveStatus {
  thinking: boolean
  tool?: ToolCall
  compacted: boolean
}

// Rendering the full list per streamed token locks the tab, so a running run shows only this summary.
export function liveStatusLine(activity: Activity[]): LiveStatus {
  let tool: ToolCall | undefined
  for (let i = activity.length - 1; i >= 0; i--) {
    const a = activity[i]
    if (a.kind === 'tool') { tool = a.tool; break }
  }
  return {
    thinking: activity[activity.length - 1]?.kind === 'thinking',
    tool,
    compacted: activity.some(a => a.kind === 'compaction'),
  }
}

// Keyed on visible content, not run count: the orchestrator's run is created empty on the first event,
// so a run-count check would hide the dots before the plan appears.
export function showLiveSpinner(args: {
  streaming: boolean
  hasDag: boolean
  answerText: string
  visibleActivityCount: number
}): boolean {
  return args.streaming && !args.hasDag && !args.answerText && args.visibleActivityCount === 0
}
