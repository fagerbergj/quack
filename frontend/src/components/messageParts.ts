// The data model for an assistant message under the flat, agent-centric event
// model. The DAG (nodes) is the static structure; within each node the gate runs
// a SEQUENCE of agent invocations ("runs") — the worker draft, optional
// self-refine, each judge round, each revision. Every run is delimited by
// agent_start / agent_complete and carries a run_id + stage; its activity
// references that run_id. We key everything by run_id and pair tools by call_id —
// no open-container heuristics. No JSX here so this stays trivially testable;
// rendering lives in AgentParts.tsx / DagNode.tsx.

export type Stage = 'worker' | 'self_refine' | 'judge' | 'revise'

// ToolCall is one tool invocation within a run; result fills in when it returns.
export interface ToolCall {
  callId: string
  name: string
  args: Record<string, unknown>
  result?: unknown
  done: boolean
}

// Activity is one ordered item inside a run: reasoning or a tool call.
export type Activity =
  | { kind: 'thinking'; text: string }
  | { kind: 'tool'; tool: ToolCall }

// AgentRun is one agent invocation within a node. Result fields are populated on
// agent_complete and vary by stage.
export interface AgentRun {
  runId: string
  agent: string
  stage: Stage
  round?: number
  activity: Activity[]
  done: boolean
  startedAt?: number    // ms timestamp when the run opened
  durationMs?: number   // set on complete
  // results (set on complete)
  changed?: boolean    // self_refine
  score?: number       // judge
  passed?: boolean     // judge
  feedback?: string    // judge
  status?: string      // '' ok | 'unavailable'
  reason?: string
  finishReason?: string // worker
  model?: string
  totalTokens?: number
}

// startRun appends a new run. Idempotent on run_id (a duplicate start is ignored).
export function startRun(runs: AgentRun[], r: { runId: string; agent: string; stage: Stage; round?: number; startedAt?: number }): AgentRun[] {
  if (runs.some(x => x.runId === r.runId)) return runs
  return [...runs, { runId: r.runId, agent: r.agent, stage: r.stage, round: r.round, activity: [], done: false, startedAt: r.startedAt }]
}

// appendRunThinking adds reasoning to a run, coalescing with a trailing thinking item.
export function appendRunThinking(runs: AgentRun[], runId: string, text: string): AgentRun[] {
  return mapRun(runs, runId, run => {
    const last = run.activity[run.activity.length - 1]
    if (last && last.kind === 'thinking') {
      const activity = [...run.activity]
      activity[activity.length - 1] = { kind: 'thinking', text: last.text + text }
      return { ...run, activity }
    }
    return { ...run, activity: [...run.activity, { kind: 'thinking', text }] }
  })
}

// appendRunToolCall adds a pending tool call to a run.
export function appendRunToolCall(runs: AgentRun[], runId: string, callId: string, name: string, args: Record<string, unknown>): AgentRun[] {
  return mapRun(runs, runId, run => ({
    ...run,
    activity: [...run.activity, { kind: 'tool', tool: { callId, name, args, done: false } }],
  }))
}

// fillRunToolResult attaches a result to the matching pending tool call (by
// call_id, falling back to the most recent pending call of the same name).
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
    const activity = [...run.activity]
    const a = activity[idx] as { kind: 'tool'; tool: ToolCall }
    activity[idx] = { kind: 'tool', tool: { ...a.tool, result, done: true } }
    return { ...run, activity }
  })
}

// completeRun marks a run done, records its stage-specific result, and freezes
// its duration from startedAt to nowMs (when both are available).
export function completeRun(runs: AgentRun[], runId: string, data: Partial<AgentRun>, nowMs?: number): AgentRun[] {
  return mapRun(runs, runId, run => {
    const durationMs = nowMs != null && run.startedAt != null ? nowMs - run.startedAt : run.durationMs
    return { ...run, ...data, done: true, durationMs }
  })
}

// freezeOpenRuns marks any still-open run done, freezing its live timer. Called
// when a node finishes: a node can't be "done" while one of its runs is still
// counting, so this is the backstop if an agent_complete is ever dropped,
// reordered, or never sent (e.g. a stage that ends without a matching complete).
export function freezeOpenRuns(runs: AgentRun[], nowMs?: number): AgentRun[] {
  if (!runs.some(r => !r.done)) return runs
  return runs.map(run => {
    if (run.done) return run
    const durationMs = nowMs != null && run.startedAt != null ? nowMs - run.startedAt : run.durationMs
    return { ...run, done: true, durationMs }
  })
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
