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

// MessagePart is one node of the message tree.
export type MessagePart = TextPart | ThinkingPart | ToolCallPart | AgentPart | SelfRefinePart | JudgeVerdictPart

export interface TextPart {
  kind: 'text'
  text: string
}

// SelfRefinePart records that the trust gate ran the worker's self-refine pass,
// and whether it changed the answer. Nests inside its actor group.
export interface SelfRefinePart {
  kind: 'self_refine'
  changed: boolean
}

// JudgeVerdictPart records one round of the independent judge's verdict. Nests
// inside its actor group.
export interface JudgeVerdictPart {
  kind: 'judge_verdict'
  round: number
  score: number
  passed: boolean
  feedback: string
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

// appendThinkingPart folds reasoning into the active actor group, coalescing with a
// trailing thinking node.
export function appendThinkingPart(parts: MessagePart[], text: string): MessagePart[] {
  return intoOpenAgent(parts, items => pushThinking(items, text)) ?? parts
}

// appendToolCall nests a tool call under the active actor group. (The built-in
// transfer tool never reaches here — the backend surfaces it as agent_start.)
export function appendToolCall(parts: MessagePart[], name: string, args: Record<string, unknown>): MessagePart[] {
  const call: ToolCallPart = { kind: 'tool_call', name, args }
  return intoOpenAgent(parts, items => [...items, call]) ?? [...parts, call]
}

// fillToolResult attaches a result to the most recent matching pending tool call in
// the active actor group.
export function fillToolResult(parts: MessagePart[], name: string, result: unknown): MessagePart[] {
  return fillPending(parts, name, result) ?? parts
}

// appendSelfRefine nests a self-refine marker under the active actor group.
export function appendSelfRefine(parts: MessagePart[], changed: boolean): MessagePart[] {
  const node: SelfRefinePart = { kind: 'self_refine', changed }
  return intoOpenAgent(parts, items => [...items, node]) ?? [...parts, node]
}

// appendJudgeVerdict nests a judge verdict under the active actor group.
export function appendJudgeVerdict(parts: MessagePart[], v: Omit<JudgeVerdictPart, 'kind'>): MessagePart[] {
  const node: JudgeVerdictPart = { kind: 'judge_verdict', ...v }
  return intoOpenAgent(parts, items => [...items, node]) ?? [...parts, node]
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

// intoOpenAgent applies `transform` to the items of the deepest open agent along
// the last-open spine, rebuilding the path immutably. Returns null if no agent is
// open at this level.
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
    items: node.items.map(it =>
      it.kind === 'agent' && !it.done ? closeWithDescendants(it) : it,
    ),
  }
}

// fillPending sets result on the most recent pending tool call, searching the open
// agent spine first (where the call was just made), then this level.
function fillPending(items: MessagePart[], name: string, result: unknown): MessagePart[] | null {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]
    if (it.kind === 'agent' && !it.done) {
      const deeper = fillPending(it.items, name, result)
      if (deeper) {
        const next = [...items]
        next[i] = { ...it, items: deeper }
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
