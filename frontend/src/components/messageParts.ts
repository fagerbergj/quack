// The data model for an assistant message: an ordered tree of MessageParts plus
// the pure reducers that fold streamed events into it. No JSX here so this stays
// trivially unit-testable; rendering lives in AgentParts.tsx.
//
// The shape is a tree of actor groups. The backend tags the stream with explicit
// lifecycle events — agent_start when the orchestrator dispatches to a specialist,
// agent_end when that specialist's turn completes — so we nest activity under the
// actor that produced it instead of guessing from message content. Answer/narrative
// text streams at the top level (the readable output); thinking and tool calls nest
// inside their actor's group.
//
// Self-refine and judge rounds are also collapsible containers nested inside the
// actor's group. They open on *_start events and close on the corresponding result
// events. Thinking from those model calls routes into the open container.

// MessagePart is one node of the message tree.
export type MessagePart = TextPart | ThinkingPart | ToolCallPart | AgentPart | SelfRefinePart | RevisePart | JudgeVerdictPart | JudgeUnavailablePart

export interface TextPart {
  kind: 'text'
  text: string
}

// SelfRefinePart is a collapsible container for one self-refine pass.
// Opens on self_refine_start and closes on self_refine. Thinking events that
// arrive between those two wire events nest inside `items`.
export interface SelfRefinePart {
  kind: 'self_refine'
  changed?: boolean  // set when done
  items: MessagePart[]
  done: boolean
}

// JudgeVerdictPart is a collapsible container for one judge round.
// Opens on judge_start and closes on judge_verdict. Thinking events that
// arrive between those two wire events nest inside `items`.
export interface JudgeVerdictPart {
  kind: 'judge_verdict'
  round: number
  score?: number    // set when done
  passed?: boolean  // set when done
  feedback?: string // set when done
  items: MessagePart[]
  done: boolean
}

// RevisePart records that the gate revised the worker's answer in response to
// judge feedback before starting the next judge round.
export interface RevisePart {
  kind: 'revise'
  round: number
}

// JudgeUnavailablePart records that the judge failed and the answer was surfaced
// unvetted. The UI shows a quality-cannot-be-guaranteed warning banner.
export interface JudgeUnavailablePart {
  kind: 'judge_unavailable'
  round: number
  reason: string
}

export interface ThinkingPart {
  kind: 'thinking'
  text: string
}

export interface ToolCallPart {
  kind: 'tool_call'
  name: string
  args: Record<string, unknown>
  // result is set once the tool returns; while undefined the UI shows "running…".
  result?: unknown
}

// AgentPart is one actor's group: its activity (thinking / tool calls) and any
// sub-agents it dispatched, in order. `done` flips when the actor's turn ends.
export interface AgentPart {
  kind: 'agent'
  agent: string
  items: MessagePart[]
  done: boolean
}

// openAgent pushes a new actor group under the deepest open agent (nesting a
// dispatched specialist inside its dispatcher), or at the top level if none is open.
export function openAgent(parts: MessagePart[], agent: string): MessagePart[] {
  const node: AgentPart = { kind: 'agent', agent, items: [], done: false }
  return intoOpenAgent(parts, items => [...items, node]) ?? [...parts, node]
}

// closeAgent marks the named actor's group done (the innermost open one with that
// name), along with any still-open descendants, so later activity stops nesting
// under it. Matching by name keeps the tree balanced even if a nested specialist's
// agent_end is dropped: closing the orchestrator then also closes the orphaned
// child rather than mistakenly closing that child in the orchestrator's place.
export function closeAgent(parts: MessagePart[], agent: string): MessagePart[] {
  return closeNamedOpen(parts, agent) ?? parts
}

// appendThinkingPart folds reasoning into the deepest open container (agent,
// self_refine, or judge_verdict), coalescing with a trailing thinking node.
export function appendThinkingPart(parts: MessagePart[], text: string): MessagePart[] {
  return intoOpenContainer(parts, items => pushThinking(items, text)) ?? parts
}

// appendToolCall nests a tool call under the deepest open container. Using
// intoOpenContainer (rather than intoOpenAgent) ensures that tool calls emitted
// during agentic self-refine land inside the open self_refine block, matching
// where thinking events for the same pass are routed.
export function appendToolCall(parts: MessagePart[], name: string, args: Record<string, unknown>): MessagePart[] {
  const call: ToolCallPart = { kind: 'tool_call', name, args }
  return intoOpenContainer(parts, items => [...items, call]) ?? [...parts, call]
}

// fillToolResult attaches a result to the most recent matching pending tool call in
// the active actor group.
export function fillToolResult(parts: MessagePart[], name: string, result: unknown): MessagePart[] {
  return fillPending(parts, name, result) ?? parts
}

// openSelfRefine pushes a new open self-refine container under the active agent.
// Called on self_refine_start; closed by closeSelfRefine.
export function openSelfRefine(parts: MessagePart[]): MessagePart[] {
  const node: SelfRefinePart = { kind: 'self_refine', items: [], done: false }
  return intoOpenAgent(parts, items => [...items, node]) ?? [...parts, node]
}

// closeSelfRefine marks the innermost open self_refine container done.
// Called on self_refine.
export function closeSelfRefine(parts: MessagePart[], changed: boolean): MessagePart[] {
  return closeOpenSelfRefineHelper(parts, changed) ?? parts
}

// openJudgeVerdict pushes a new open judge_verdict container under the active agent.
// Called on judge_start; closed by closeJudgeVerdict.
export function openJudgeVerdict(parts: MessagePart[], round: number): MessagePart[] {
  const node: JudgeVerdictPart = { kind: 'judge_verdict', round, items: [], done: false }
  return intoOpenAgent(parts, items => [...items, node]) ?? [...parts, node]
}

// closeJudgeVerdict fills in the verdict on the innermost open judge_verdict container.
// Called on judge_verdict. _round is accepted for API symmetry with openJudgeVerdict;
// the implementation closes the deepest open container regardless of round number.
export function closeJudgeVerdict(parts: MessagePart[], _round: number, score: number, passed: boolean, feedback: string): MessagePart[] {
  return closeOpenJudgeVerdictHelper(parts, score, passed, feedback) ?? parts
}

// appendRevise nests a revise marker under the active actor group.
export function appendRevise(parts: MessagePart[], round: number): MessagePart[] {
  const node: RevisePart = { kind: 'revise', round }
  return intoOpenAgent(parts, items => [...items, node]) ?? [...parts, node]
}

// appendJudgeUnavailable closes any open judge_verdict container (the judge failed
// before producing a verdict), then appends a judge-unavailable warning.
export function appendJudgeUnavailable(parts: MessagePart[], round: number, reason: string): MessagePart[] {
  // Close the open judge container (if any) so it doesn't spin forever.
  const next = closeOpenJudgeVerdictHelper(parts, 0, false, '') ?? parts
  const node: JudgeUnavailablePart = { kind: 'judge_unavailable', round, reason }
  return intoOpenAgent(next, items => [...items, node]) ?? [...next, node]
}

// appendTextPart appends answer/narrative text at the top level, coalescing with a
// trailing text node. Text is the readable output and never nests in a group.
export function appendTextPart(parts: MessagePart[], text: string): MessagePart[] {
  const last = parts[parts.length - 1]
  if (last && last.kind === 'text') {
    const next = [...parts]
    next[next.length - 1] = { ...last, text: last.text + text }
    return next
  }
  return [...parts, { kind: 'text', text }]
}

// partsToText concatenates the top-level text nodes (for copy/download). Actor
// groups (thinking, tool calls, nested agents) are omitted.
export function partsToText(parts: MessagePart[]): string {
  return parts.filter((p): p is TextPart => p.kind === 'text').map(p => p.text).join('')
}

// --- internal tree helpers ---

// intoOpenAgent applies `transform` to the items of the deepest open AGENT along
// the last-open spine, rebuilding the path immutably. Used for operations that
// should land in the active agent group but NOT inside sub-containers (self_refine,
// judge_verdict) — e.g. opening a new agent, adding tool calls, opening containers.
// Returns null if no agent is open at this level.
function intoOpenAgent(items: MessagePart[], transform: (children: MessagePart[]) => MessagePart[]): MessagePart[] | null {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]
    if (it.kind === 'agent' && !it.done) {
      const deeper = intoOpenAgent(it.items, transform)
      const next = [...items]
      next[i] = { ...it, items: deeper ?? transform(it.items) }
      return next
    }
  }
  return null
}

// intoOpenContainer applies `transform` to the items of the deepest open container
// (agent, self_refine, or judge_verdict) along the last-open spine. Used for
// operations that should land inside any open container — specifically thinking.
// Returns null if no container is open at this level.
function intoOpenContainer(items: MessagePart[], transform: (children: MessagePart[]) => MessagePart[]): MessagePart[] | null {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]
    if (!isOpenContainer(it)) continue
    const deeper = intoOpenContainer(it.items, transform)
    const next = [...items]
    next[i] = { ...it, items: deeper ?? transform(it.items) }
    return next
  }
  return null
}

function isOpenContainer(it: MessagePart): it is AgentPart | SelfRefinePart | JudgeVerdictPart {
  return (it.kind === 'agent' || it.kind === 'self_refine' || it.kind === 'judge_verdict') && !it.done
}

// closeNamedOpen marks the innermost open agent named `agent` done — together with
// any of its still-open descendants — rebuilding the path immutably. Returns null
// if no open group with that name exists.
function closeNamedOpen(items: MessagePart[], agent: string): MessagePart[] | null {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]
    if (it.kind !== 'agent' || it.done) continue
    // Prefer an inner match so the deepest matching group closes first.
    const deeper = closeNamedOpen(it.items, agent)
    if (deeper) {
      const next = [...items]
      next[i] = { ...it, items: deeper }
      return next
    }
    if (it.agent === agent) {
      const next = [...items]
      next[i] = closeWithDescendants(it)
      return next
    }
  }
  return null
}

// closeWithDescendants marks an agent group and every still-open descendant done.
function closeWithDescendants(node: AgentPart): AgentPart {
  return {
    ...node,
    done: true,
    items: node.items.map(it => {
      if (it.kind === 'agent' && !it.done) return closeWithDescendants(it)
      if ((it.kind === 'self_refine' || it.kind === 'judge_verdict') && !it.done) return { ...it, done: true }
      return it
    }),
  }
}

// closeOpenSelfRefineHelper finds the innermost open self_refine container in the
// active agent spine and marks it done with the given changed flag.
function closeOpenSelfRefineHelper(items: MessagePart[], changed: boolean): MessagePart[] | null {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]
    if (it.kind !== 'agent' || it.done) continue
    // Try deeper first.
    const deeper = closeOpenSelfRefineHelper(it.items, changed)
    if (deeper) {
      const next = [...items]
      next[i] = { ...it, items: deeper }
      return next
    }
    // Look for an open self_refine in this agent's items.
    for (let j = it.items.length - 1; j >= 0; j--) {
      const child = it.items[j]
      if (child.kind === 'self_refine' && !child.done) {
        const nextItems = [...it.items]
        nextItems[j] = { ...child, changed, done: true }
        const next = [...items]
        next[i] = { ...it, items: nextItems }
        return next
      }
    }
    break // only follow the active agent spine
  }
  return null
}

// closeOpenJudgeVerdictHelper finds the innermost open judge_verdict container in
// the active agent spine and closes it with verdict data.
function closeOpenJudgeVerdictHelper(items: MessagePart[], score: number, passed: boolean, feedback: string): MessagePart[] | null {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]
    if (it.kind !== 'agent' || it.done) continue
    // Try deeper first.
    const deeper = closeOpenJudgeVerdictHelper(it.items, score, passed, feedback)
    if (deeper) {
      const next = [...items]
      next[i] = { ...it, items: deeper }
      return next
    }
    // Look for an open judge_verdict in this agent's items.
    for (let j = it.items.length - 1; j >= 0; j--) {
      const child = it.items[j]
      if (child.kind === 'judge_verdict' && !child.done) {
        const nextItems = [...it.items]
        nextItems[j] = { ...child, score, passed, feedback, done: true }
        const next = [...items]
        next[i] = { ...it, items: nextItems }
        return next
      }
    }
    break // only follow the active agent spine
  }
  return null
}

// fillPending sets result on the most recent pending tool call, searching the open
// container spine first (where the call was just made), then this level.
// Recurses into any open container (agent, self_refine, judge_verdict) so tool
// calls placed inside a self_refine block by appendToolCall are found here too.
function fillPending(items: MessagePart[], name: string, result: unknown): MessagePart[] | null {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]
    if (isOpenContainer(it)) {
      const deeper = fillPending(it.items, name, result)
      if (deeper) {
        const next = [...items]
        next[i] = { ...it, items: deeper } as MessagePart
        return next
      }
      break // only follow the active spine
    }
  }
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]
    if (it.kind === 'tool_call' && it.result === undefined && it.name === name) {
      const next = [...items]
      next[i] = { ...it, result }
      return next
    }
  }
  return null
}

// pushThinking appends or coalesces a thinking node onto an items list.
function pushThinking(items: MessagePart[], text: string): MessagePart[] {
  const last = items[items.length - 1]
  if (last && last.kind === 'thinking') {
    const next = [...items]
    next[next.length - 1] = { ...last, text: last.text + text }
    return next
  }
  return [...items, { kind: 'thinking', text }]
}
