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

export type Stage = 'worker' | 'self_refine' | 'judge' | 'revise'

// AgentStartPayload opens an agent run within a node.
export interface AgentStartPayload {
  nodeId?: string
  runId: string
  agent: string
  stage: Stage
  round?: number
}

// AgentCompletePayload closes an agent run with its stage-specific result.
export interface AgentCompletePayload {
  nodeId?: string
  runId: string
  stage: Stage
  round?: number
  changed?: boolean
  score?: number
  passed?: boolean
  feedback?: string
  status?: string
  reason?: string
  finishReason?: string
  model?: string
  totalTokens?: number
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

// NodeDoneMeta carries optional completion metadata from node_done.
export interface NodeDoneMeta {
  model?: string
  promptTokens?: number
  completionTokens?: number
  reasoningTokens?: number
  totalTokens?: number
  finishReason?: string
  durationMs?: number
  selfRefined?: boolean
  judgeRounds?: number
  judgeFinalScore?: number
  judgePassed?: boolean
}

// DagPlanPayload is the dag_plan event payload.
export interface DagPlanPayload {
  plan_id: string
  nodes: DagNodeDef[]
  edges: DagEdgeDef[]
}

export interface AgentStreamHandlers {
  // Agent-run lifecycle + typed activity (flat; each carries node_id + run_id).
  onAgentStart?: (d: AgentStartPayload) => void
  onAgentThinking?: (runId: string, text: string, nodeId?: string) => void
  onAgentToolCall?: (runId: string, callId: string, name: string, args: Record<string, unknown>, nodeId?: string) => void
  onAgentToolResult?: (runId: string, callId: string, name: string, result: unknown, nodeId?: string) => void
  onAgentToken?: (runId: string, text: string, nodeId?: string) => void
  onAgentComplete?: (d: AgentCompletePayload) => void
  onConfirmationRequest?: (req: ConfirmationRequestPayload) => void
  onChatTitle?: (title: string) => void
  onError?: (msg: string) => void
  onDone?: () => void
  // DAG lifecycle
  onDagPlan?: (plan: DagPlanPayload) => void
  onNodeQueued?: (nodeId: string) => void
  onNodeStart?: (nodeId: string, agent: string) => void
  onNodeDone?: (nodeId: string, preview: string, meta: NodeDoneMeta) => void
  onNodeFailed?: (nodeId: string, error: string) => void
}

// Wire-level event names. Mirrors internal/stream/event.go.
export const AGENT_EVENT_NAMES = [
  'agent_start', 'agent_thinking', 'agent_tool_call', 'agent_tool_result', 'agent_token', 'agent_complete',
  'confirmation_request', 'chat_title', 'error', 'done',
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
    case 'agent_start': {
      const p = parsed as { run_id?: string; agent?: string; stage?: string; round?: number }
      if (typeof p.run_id === 'string') {
        handlers.onAgentStart?.({
          nodeId: nodeIdOf(parsed),
          runId: p.run_id,
          agent: typeof p.agent === 'string' ? p.agent : '',
          stage: (p.stage ?? 'worker') as Stage,
          round: typeof p.round === 'number' ? p.round : undefined,
        })
      }
      return true
    }
    case 'agent_thinking': {
      const p = parsed as { run_id?: string; text?: string }
      if (typeof p.text === 'string') handlers.onAgentThinking?.(p.run_id ?? '', p.text, nodeIdOf(parsed))
      return true
    }
    case 'agent_tool_call': {
      const p = parsed as { run_id?: string; call_id?: string; name?: string; args?: Record<string, unknown> }
      if (typeof p.name === 'string') {
        handlers.onAgentToolCall?.(p.run_id ?? '', p.call_id ?? '', p.name, p.args ?? {}, nodeIdOf(parsed))
      }
      return true
    }
    case 'agent_tool_result': {
      const p = parsed as { run_id?: string; call_id?: string; name?: string; result?: unknown }
      if (typeof p.name === 'string') {
        handlers.onAgentToolResult?.(p.run_id ?? '', p.call_id ?? '', p.name, p.result, nodeIdOf(parsed))
      }
      return true
    }
    case 'agent_token': {
      const p = parsed as { run_id?: string; text?: string }
      if (typeof p.text === 'string') handlers.onAgentToken?.(p.run_id ?? '', p.text, nodeIdOf(parsed))
      return true
    }
    case 'agent_complete': {
      const p = parsed as Record<string, unknown>
      if (typeof p.run_id === 'string') {
        handlers.onAgentComplete?.({
          nodeId: nodeIdOf(parsed),
          runId: p.run_id,
          stage: (typeof p.stage === 'string' ? p.stage : 'worker') as Stage,
          round: typeof p.round === 'number' ? p.round : undefined,
          changed: p.changed === true,
          score: typeof p.score === 'number' ? p.score : undefined,
          passed: p.passed === true,
          feedback: typeof p.feedback === 'string' ? p.feedback : undefined,
          status: typeof p.status === 'string' ? p.status : undefined,
          reason: typeof p.reason === 'string' ? p.reason : undefined,
          finishReason: typeof p.finish_reason === 'string' ? p.finish_reason : undefined,
          model: typeof p.model === 'string' ? p.model : undefined,
          totalTokens: typeof p.total_tokens === 'number' ? p.total_tokens : undefined,
        })
      }
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
      const p = parsed as {
        node_id?: string; output_preview?: string
        model?: string; prompt_tokens?: number; completion_tokens?: number
        reasoning_tokens?: number; total_tokens?: number; finish_reason?: string; duration_ms?: number
        self_refined?: boolean; judge_rounds?: number; judge_final_score?: number; judge_passed?: boolean
      }
      if (typeof p.node_id === 'string') {
        const meta: NodeDoneMeta = {
          model: p.model,
          promptTokens: p.prompt_tokens,
          completionTokens: p.completion_tokens,
          reasoningTokens: p.reasoning_tokens,
          totalTokens: p.total_tokens,
          finishReason: p.finish_reason,
          durationMs: p.duration_ms,
          selfRefined: p.self_refined,
          judgeRounds: p.judge_rounds,
          judgeFinalScore: p.judge_final_score,
          judgePassed: p.judge_passed,
        }
        handlers.onNodeDone?.(p.node_id, typeof p.output_preview === 'string' ? p.output_preview : '', meta)
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
