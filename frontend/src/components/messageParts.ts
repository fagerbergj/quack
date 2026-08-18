// The data model for an assistant message under the flat, agent-centric event
// model. The DAG (nodes) is the static structure; within each node the gate runs
// a SEQUENCE of agent invocations ("runs") - the worker draft, optional
// self-refine, each judge round, each revision. Every run is delimited by
// agent_start / agent_complete and carries a run_id + stage; its activity
// references that run_id. We key everything by run_id and pair tools by call_id -
// no open-container heuristics. No JSX here so this stays trivially testable;
// rendering lives in AgentParts.tsx / DagNode.tsx.

export type Stage = 'worker' | 'judge' | 'revise'

// agentLabel maps an agent bundle name to a human-readable display label. Shared
// by DagNode's node header, per-run stage cards, and the bubble-attribution
// header (BubbleHeader / QuestionBubble) so an agent reads the same everywhere.
export function agentLabel(name: string): string {
  if (name === 'web-researcher') return 'Web researcher'
  if (name === 'synthesizer') return 'Synthesizer'
  if (name === 'orchestrator') return 'Orchestrator'
  return name
}

// ToolCall is one tool invocation within a run; result fills in when it returns.
export interface ToolCall {
  callId: string
  name: string
  args: Record<string, unknown>
  result?: unknown
  done: boolean
  // title carries the ACP-supplied human label for calls the backend maps to
  // the catch-all kind "other" (a stage_review MCP call, "Loaded skill: …") -
  // name alone is meaningless for those, so the row should prefer title (#959).
  title?: string
}

// Activity is one ordered item inside a run: reasoning, a tool call, or a
// context-compaction event (the run's history was rewritten mid-round).
export type Activity =
  | { kind: 'thinking'; text: string }
  | { kind: 'tool'; tool: ToolCall }
  | { kind: 'compaction'; tokensBefore: number; tokensAfter: number }

// AgentRun is one agent invocation within a node. Result fields are populated on
// agent_complete and vary by stage.
export interface AgentRun {
  runId: string
  agent: string
  stage: Stage
  round?: number
  activity: Activity[]
  // Index of the run's most recent thinking item, so appendRunThinking can
  // fold a delta into it in O(1) even across intervening tool calls (#959),
  // rather than rescanning activity for the last thinking item.
  lastThinkIdx?: number
  done: boolean
  startedAt?: number    // ms timestamp when the run opened
  durationMs?: number   // set on complete
  // results (set on complete)
  score?: number       // judge
  passed?: boolean     // judge
  feedback?: string    // judge
  status?: string      // '' ok | 'unavailable' (judge unreachable) | 'no_verdict' (judge ran, never committed one)
  reason?: string
  finishReason?: string // worker
  model?: string
  totalTokens?: number
}

// PendingChoice is an unanswered get_user_choice clarification surfaced for the UI.
export interface PendingChoice {
  callId: string
  question: string
  options: string[]
}

// pendingChoice finds a get_user_choice clarification awaiting the user's answer in
// a run list. The tool returns a `{status:"pending"}` placeholder, so the call is
// `done` with that result; the real answer arrives as a separate later turn and
// never overwrites it. So a get_user_choice whose result is still the pending
// placeholder is awaiting input. Returns the first such call, or null.
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

// startRun appends a new run. Idempotent on run_id (a duplicate start is ignored).
export function startRun(runs: AgentRun[], r: { runId: string; agent: string; stage: Stage; round?: number; startedAt?: number }): AgentRun[] {
  if (runs.some(x => x.runId === r.runId)) return runs
  return [...runs, { runId: r.runId, agent: r.agent, stage: r.stage, round: r.round, activity: [], done: false, startedAt: r.startedAt }]
}

// appendRunThinking coalesces into the most recent thinking item even across
// intervening tool calls (#959), using lastThinkIdx for O(1) lookup.
// Mutates run.activity in place (push / index-assign) rather than spreading the
// whole array - an event's cost is O(1) amortized, not O(run length), so a run
// with N events stays O(N) total instead of O(N²) (#379). The run itself still
// gets a new object identity (below) so callers keep seeing a fresh reference.
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

// appendRunToolCall adds a pending tool call to a run (mutated in place, see above).
// An ACP call re-announces itself under the SAME call_id once its kind/args
// resolve (translate.go emits the pending call, then pairs the resolved one) -
// update that row in place rather than pushing a second, since the eventual
// result only ever fills the most recent match, orphaning the first (#746).
export function appendRunToolCall(runs: AgentRun[], runId: string, callId: string, name: string, args: Record<string, unknown>, title?: string): AgentRun[] {
  return mapRun(runs, runId, run => {
    const idx = callId === '' ? -1 : run.activity.findIndex(a => a.kind === 'tool' && !a.tool.done && a.tool.callId === callId)
    if (idx >= 0) {
      run.activity[idx] = { kind: 'tool', tool: { callId, name, args, title, done: false } }
      return { ...run }
    }
    run.activity.push({ kind: 'tool', tool: { callId, name, args, title, done: false } })
    return { ...run }
  })
}

// fillRunToolResult attaches a result to the matching pending tool call (by
// call_id, falling back to the most recent pending call of the same name).
// Mutated in place (see appendRunThinking) - only the matched slot is replaced.
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

// appendRunCompaction records a compaction event in a run's activity feed
// (mutated in place, see appendRunThinking) - a no-op if the run isn't found
// (e.g. the client attached after the run's own agent_start already scrolled by).
export function appendRunCompaction(runs: AgentRun[], runId: string, tokensBefore: number, tokensAfter: number): AgentRun[] {
  return mapRun(runs, runId, run => {
    run.activity.push({ kind: 'compaction', tokensBefore, tokensAfter })
    return { ...run }
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

// LiveStatus is a run's activity reduced to what's worth showing while it's
// still RUNNING: whether the tail end is reasoning, and the most recent tool
// call (independent of whether the tail is thinking again after it returned).
export interface LiveStatus {
  thinking: boolean
  tool?: ToolCall
}

// liveStatusLine computes LiveStatus from a run's activity - the substitute
// for rendering the full list while running (#725: re-rendering an
// ever-growing activity list on every streamed token is what locks the tab).
export function liveStatusLine(activity: Activity[]): LiveStatus {
  let tool: ToolCall | undefined
  for (let i = activity.length - 1; i >= 0; i--) {
    const a = activity[i]
    if (a.kind === 'tool') { tool = a.tool; break }
  }
  return { thinking: activity[activity.length - 1]?.kind === 'thinking', tool }
}

// showLiveSpinner decides whether the live (streaming) turn shows the "thinking"
// dots: it's streaming and nothing visible has arrived yet. Keyed on VISIBLE
// content (DAG, answer text, or visible activity) - NOT run count: the
// orchestrator's top-level run is created empty on the first stream event (to
// hold its plan/execute tool calls), so a run-count check hides the dots during
// the gap before the plan appears (regression fixed 2026-06).
export function showLiveSpinner(args: {
  streaming: boolean
  hasDag: boolean
  answerText: string
  visibleActivityCount: number
}): boolean {
  return args.streaming && !args.hasDag && !args.answerText && args.visibleActivityCount === 0
}
