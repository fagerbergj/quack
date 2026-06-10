// Shared event vocabulary and dispatch for the agent SSE stream. Both
// transports — fetched ReadableStream (chat) and EventSource (job live log) —
// route events through dispatchAgentEvent so the per-event JSON shape lives
// in one place.

export interface ConfirmationRequestPayload {
  callId: string
  toolName: string
  hint: string
  payload: Record<string, unknown>
}

// JudgeVerdictPayload is one round of the independent judge's score.
export interface JudgeVerdictPayload {
  round: number
  score: number
  passed: boolean
  feedback: string
}

// DagNodeDef is one node in a DAG plan, as received from the server.
export interface DagNodeDef {
  id: string
  agent: string
  task: string
  depends_on: string[]
}

// DagEdgeDef is one edge in a DAG plan.
export interface DagEdgeDef {
  from: string
  to: string
}

// DagPlanPayload is the dag_plan event payload.
export interface DagPlanPayload {
  plan_id: string
  nodes: DagNodeDef[]
  edges: DagEdgeDef[]
}

export interface AgentStreamHandlers {
  onToken?: (text: string, nodeId?: string) => void
  onThinking?: (text: string, nodeId?: string) => void
  onToolCall?: (name: string, args: Record<string, unknown>, nodeId?: string) => void
  onToolResult?: (name: string, result: unknown, nodeId?: string) => void
  onAgentStart?: (agent: string, nodeId?: string) => void
  onAgentEnd?: (agent: string, nodeId?: string) => void
  onSelfRefineStart?: (nodeId?: string) => void
  onSelfRefine?: (changed: boolean, nodeId?: string) => void
  onJudgeStart?: (round: number, nodeId?: string) => void
  onRevise?: (round: number, nodeId?: string) => void
  onJudgeVerdict?: (v: JudgeVerdictPayload, nodeId?: string) => void
  onJudgeUnavailable?: (round: number, reason: string, nodeId?: string) => void
  onConfirmationRequest?: (req: ConfirmationRequestPayload) => void
  onChatTitle?: (title: string) => void
  onError?: (msg: string) => void
  onDone?: () => void
  // DAG lifecycle events (M3)
  onDagPlan?: (plan: DagPlanPayload) => void
  onNodeQueued?: (nodeId: string) => void
  onNodeStart?: (nodeId: string, agent: string) => void
  onNodeDone?: (nodeId: string, preview: string) => void
  onNodeFailed?: (nodeId: string, error: string) => void
}

// Wire-level event names. Mirrors internal/stream/event.go.
export const AGENT_EVENT_NAMES = [
  'token', 'thinking', 'tool_call', 'tool_result',
  'agent_start', 'agent_end',
  'self_refine_start', 'self_refine', 'judge_start', 'revise', 'judge_verdict', 'judge_unavailable',
  'confirmation_request', 'chat_title', 'error', 'done',
  // DAG events (M3)
  'dag_plan', 'node_queued', 'node_start', 'node_done', 'node_failed',
] as const
export type AgentEventName = typeof AGENT_EVENT_NAMES[number]

// nodeIdOf extracts the optional node_id field from a parsed payload.
function nodeIdOf(parsed: unknown): string | undefined {
  const p = parsed as { node_id?: string }
  return typeof p?.node_id === 'string' && p.node_id ? p.node_id : undefined
}

// dispatchAgentEvent routes one already-parsed SSE payload to the matching
// handler. Returns true if the event was recognized (whether or not a
// handler was registered for it).
export function dispatchAgentEvent(
  event: string,
  parsed: unknown,
  handlers: AgentStreamHandlers,
): boolean {
  switch (event) {
    case 'token':
      if (hasStringField(parsed, 'text')) handlers.onToken?.(parsed.text, nodeIdOf(parsed))
      return true
    case 'thinking':
      if (hasStringField(parsed, 'text')) handlers.onThinking?.(parsed.text, nodeIdOf(parsed))
      return true
    case 'tool_call':
      if (hasStringField(parsed, 'name')) {
        const args = (parsed as { args?: Record<string, unknown> }).args ?? {}
        handlers.onToolCall?.(parsed.name, args, nodeIdOf(parsed))
      }
      return true
    case 'tool_result':
      if (hasStringField(parsed, 'name')) {
        const result = (parsed as unknown as { result: unknown }).result
        handlers.onToolResult?.(parsed.name, result, nodeIdOf(parsed))
      }
      return true
    case 'agent_start':
      if (hasStringField(parsed, 'agent')) handlers.onAgentStart?.(parsed.agent, nodeIdOf(parsed))
      return true
    case 'agent_end':
      if (hasStringField(parsed, 'agent')) handlers.onAgentEnd?.(parsed.agent, nodeIdOf(parsed))
      return true
    case 'self_refine_start':
      handlers.onSelfRefineStart?.(nodeIdOf(parsed))
      return true
    case 'self_refine': {
      const changed = (parsed as { changed?: boolean }).changed === true
      handlers.onSelfRefine?.(changed, nodeIdOf(parsed))
      return true
    }
    case 'judge_start': {
      const p = parsed as { round?: number }
      handlers.onJudgeStart?.(typeof p.round === 'number' ? p.round : 0, nodeIdOf(parsed))
      return true
    }
    case 'revise': {
      const p = parsed as { round?: number }
      handlers.onRevise?.(typeof p.round === 'number' ? p.round : 0, nodeIdOf(parsed))
      return true
    }
    case 'judge_verdict': {
      const p = parsed as { round?: number; score?: number; passed?: boolean; feedback?: string }
      handlers.onJudgeVerdict?.({
        round: typeof p.round === 'number' ? p.round : 0,
        score: typeof p.score === 'number' ? p.score : 0,
        passed: p.passed === true,
        feedback: typeof p.feedback === 'string' ? p.feedback : '',
      }, nodeIdOf(parsed))
      return true
    }
    case 'judge_unavailable': {
      const p = parsed as { round?: number; reason?: string }
      handlers.onJudgeUnavailable?.(
        typeof p.round === 'number' ? p.round : 0,
        typeof p.reason === 'string' ? p.reason : '',
        nodeIdOf(parsed),
      )
      return true
    }
    case 'confirmation_request':
      if (hasStringField(parsed, 'call_id')) {
        const p = parsed as { call_id: string; tool_name?: string; hint?: string; payload?: Record<string, unknown> }
        handlers.onConfirmationRequest?.({
          callId: p.call_id,
          toolName: p.tool_name ?? '',
          hint: p.hint ?? '',
          payload: p.payload ?? {},
        })
      }
      return true
    case 'chat_title':
      if (hasStringField(parsed, 'title')) handlers.onChatTitle?.(parsed.title)
      return true
    case 'error':
      if (hasStringField(parsed, 'error')) handlers.onError?.(parsed.error)
      return true
    case 'done':
      handlers.onDone?.()
      return true
    // DAG lifecycle events (M3)
    case 'dag_plan': {
      const p = parsed as { plan_id?: string; nodes?: unknown[]; edges?: unknown[] }
      handlers.onDagPlan?.({
        plan_id: typeof p.plan_id === 'string' ? p.plan_id : '',
        nodes: (p.nodes ?? []) as DagNodeDef[],
        edges: (p.edges ?? []) as DagEdgeDef[],
      })
      return true
    }
    case 'node_queued':
      if (hasStringField(parsed, 'node_id')) handlers.onNodeQueued?.(parsed.node_id)
      return true
    case 'node_start': {
      const p = parsed as { node_id?: string; agent?: string }
      if (typeof p.node_id === 'string') {
        handlers.onNodeStart?.(p.node_id, typeof p.agent === 'string' ? p.agent : '')
      }
      return true
    }
    case 'node_done': {
      const p = parsed as { node_id?: string; output_preview?: string }
      if (typeof p.node_id === 'string') {
        handlers.onNodeDone?.(p.node_id, typeof p.output_preview === 'string' ? p.output_preview : '')
      }
      return true
    }
    case 'node_failed': {
      const p = parsed as { node_id?: string; error?: string }
      if (typeof p.node_id === 'string') {
        handlers.onNodeFailed?.(p.node_id, typeof p.error === 'string' ? p.error : '')
      }
      return true
    }
  }
  return false
}

// readAgentStream parses a fetched SSE ReadableStream (used by the chat send
// flow, which posts a request body and reads the response stream).
export async function readAgentStream(
  body: ReadableStream<Uint8Array>,
  handlers: AgentStreamHandlers,
): Promise<void> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  let currentEvent = 'message'
  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    const lines = buf.split('\n')
    buf = lines.pop()!
    for (const line of lines) {
      if (line.startsWith('event: ')) {
        currentEvent = line.slice(7).trim()
        continue
      }
      if (!line.startsWith('data: ')) continue
      const raw = line.slice(6).trim()
      if (!raw) continue
      let parsed: unknown
      try { parsed = JSON.parse(raw) } catch { continue }
      dispatchAgentEvent(currentEvent, parsed, handlers)
    }
  }
}

// attachAgentEventSource wires an EventSource (used by the job live log) to
// the same handler shape readAgentStream consumes. Returns a teardown that
// closes the EventSource.
export function attachAgentEventSource(
  es: EventSource,
  handlers: AgentStreamHandlers,
): () => void {
  for (const name of AGENT_EVENT_NAMES) {
    es.addEventListener(name, (e) => {
      let parsed: unknown = {}
      const data = (e as MessageEvent).data
      if (typeof data === 'string' && data.length > 0) {
        try { parsed = JSON.parse(data) } catch { return }
      }
      dispatchAgentEvent(name, parsed, handlers)
    })
  }
  return () => es.close()
}

function hasStringField<K extends string>(v: unknown, field: K): v is Record<K, string> {
  return typeof v === 'object' && v !== null && typeof (v as Record<string, unknown>)[field] === 'string'
}
