// Shared event vocabulary and dispatch for the agent SSE stream. Both
// transports - fetched ReadableStream (a chat's initial POST) and EventSource
// (chatStore's GET reconnect) - route events through dispatchAgentEvent so
// the per-event JSON shape lives in one place.

interface ConfirmationRequestPayload {
  callId: string
  toolName: string
  hint: string
  payload: Record<string, unknown>
}

export type Stage = 'worker' | 'judge' | 'revise'

// AgentStartPayload opens an agent run within a node.
interface AgentStartPayload {
  nodeId?: string
  runId: string
  agent: string
  stage: Stage
  round?: number
  // Server wall-clock (epoch ms) the run began - anchors the sub-step timer
  // across reconnect/replay, mirroring node_start's started_at_ms.
  startedAtMs?: number
  // Cross-references into the OTel trace for this run; absent when otel is disabled.
  traceId?: string
}

// AgentCompletePayload closes an agent run with its stage-specific result.
interface AgentCompletePayload {
  nodeId?: string
  runId: string
  stage: Stage
  round?: number
  score?: number
  passed?: boolean
  threshold?: number
  feedback?: string
  status?: string
  reason?: string
  finishReason?: string
  model?: string
  totalTokens?: number
  // contextTokens is the LAST measured prompt-token count of this run (not
  // summed across tool round trips like totalTokens) - the context meter's
  // live "used" reading.
  contextTokens?: number
  // The server wall-clock (epoch ms) the run closed - lets a replayed/reconnected
  // client compute this run's duration from two server timestamps instead of "now"
  // at replay time. Absent from an older-server event; callers fall back to Date.now() then.
  finishedAtMs?: number
}

// DagNodeDef is one node in a DAG plan, as received from the server.
export interface DagNodeDef {
  id: string
  agent: string
  task: string
  depends_on: string[]
  // context_window is the assigned agent's configured limit (0/absent if
  // unset) - the context meter's static ceiling.
  context_window?: number
  // artifact is the node's declared output artifact kind - the record name
  // its output is saved as on gate pass; absent when the node declares none.
  artifact?: string
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
  cachedTokens?: number
  contextTokens?: number
  finishReason?: string
  durationMs?: number
  // finishedAtMs is the server wall-clock (epoch ms) the node finished - see
  // AgentCompletePayload.finishedAtMs.
  finishedAtMs?: number
  judgeRounds?: number
  judgeFinalScore?: number
  judgePassed?: boolean
}

// The compaction event payload: a node's worker session was rewritten
// mid-round by adk's own runner-level compaction. runId is quack's own run_id
// (stream.RunIDFromBranch) - the same one the round's agent_start carries, so it matches by exact run_id, not a heuristic.
interface CompactionPayload {
  nodeId: string
  runId: string
  startTimestamp?: string
  endTimestamp?: string
  summaryInputTokens?: number
  summaryOutputTokens?: number
}

// DagPlanPayload is the dag_plan event payload.
interface DagPlanPayload {
  planId: string
  nodes: DagNodeDef[]
  edges: DagEdgeDef[]
  startedAtMs?: number // server start time, for a reconnect-stable total timer
  // Cross-references into the OTel trace for this plan; absent when otel is disabled.
  traceId?: string
}

// One staged item's ACTUAL outward-boundary outcome (push + PR/review/comment),
// as the delivering extension observed it - never the worker's self-report.
// "none" is the phantom-success class: a judge-passed work-request that recorded no delivery attempt at all.
interface DeliveryResultPayload {
  nodeId: string
  outcome: 'delivered' | 'draft' | 'failed' | 'none'
  kind?: string
  url?: string
  error?: string
  traceId?: string
}

// ArtifactRevisionPayload mirrors internal/stream.ArtifactRevisionData - one
// artifact revision written by a judge/worker round (#1092).
export interface ArtifactRevisionPayload {
  id: string
  revision: number
  kind: string
  nodeId: string
  round: number
}

// ArtifactScoredRef mirrors internal/stream.ScoredRef.
interface ArtifactScoredRef {
  artifactId: string
  revision: number
}

// ArtifactJudgeRoundPayload mirrors internal/stream.ArtifactJudgeRoundData -
// the judge_round record a round wrote, referencing revisions already
// announced via artifact_revision.
export interface ArtifactJudgeRoundPayload {
  id: string
  passed: boolean
  score: number
  scored: ArtifactScoredRef[]
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
  // response_created is the very first event of a run, naming the turn
  // (response_id) so the client can cancel it via
  // PUT /chats/{chat_id}/responses/{response_id}/status.
  onResponseCreated?: (responseId: string) => void
  // DAG lifecycle
  onDagPlan?: (plan: DagPlanPayload) => void
  onNodeQueued?: (nodeId: string) => void
  // resumedFrom: present only when this dispatch reused an existing node id -
  // the prior context it continues on.
  onNodeStart?: (nodeId: string, agent: string, startedAtMs?: number, traceId?: string, resumedFrom?: string) => void
  onNodeDone?: (nodeId: string, preview: string, meta: NodeDoneMeta) => void
  // finishedAtMs: see AgentCompletePayload.finishedAtMs.
  onNodeFailed?: (nodeId: string, error: string, finishedAtMs?: number) => void
  // The node was stopped by the user (PUT node status {"status":"cancelled"}) -
  // rendered neutrally ("stopped"), distinct from a real gate failure.
  // finishedAtMs: see AgentCompletePayload.finishedAtMs.
  onNodeCancelled?: (nodeId: string, finishedAtMs?: number) => void
  // The node was suspended by the user (PUT node status {"status":"paused"}) -
  // keeps its accumulated work; resumable via {"status":"running"}.
  onNodePaused?: (nodeId: string) => void
  // One staged item's actual delivery outcome - not yet rendered in the UI;
  // wired so the event is parsed rather than silently dropped (see M13/OTel
  // observability: this is the phantom-success visibility signal).
  onDeliveryResult?: (d: DeliveryResultPayload) => void
  onNodeSteered?: (nodeId: string, guidance: string) => void
  // A node paused to ask the user a question (mid-node HITL). The next message
  // sent on the chat is delivered to the node as the answer.
  onNodeNeedsInput?: (nodeId: string, interruptId: string, message: string) => void
  // A node's worker session was compacted mid-round by adk's own runner-level
  // compaction (#1185 follow-up).
  onCompaction?: (d: CompactionPayload) => void
  // One artifact revision written by a round - fires before the round's own
  // artifact_judge_round event (#1092).
  onArtifactRevision?: (d: ArtifactRevisionPayload) => void
  // The judge_round record a round wrote.
  onArtifactJudgeRound?: (d: ArtifactJudgeRoundPayload) => void
}

// Wire-level event names. Mirrors internal/stream/event.go.
const AGENT_EVENT_NAMES = [
  'agent_start', 'agent_thinking', 'agent_tool_call', 'agent_tool_result', 'agent_token', 'agent_complete',
  'confirmation_request', 'chat_title', 'error', 'done', 'response_created',
  'dag_plan', 'node_queued', 'node_start', 'node_done', 'node_failed', 'node_cancelled', 'node_paused', 'node_steered', 'node_needs_input',
  'delivery_result', 'compaction', 'artifact_revision', 'artifact_judge_round',
] as const

// nodeIdOf extracts the optional node_id field from a parsed payload.
function nodeIdOf(parsed: unknown): string | undefined {
  const p = parsed as { node_id?: string }
  return typeof p?.node_id === 'string' && p.node_id ? p.node_id : undefined
}

// dispatchAgentEvent routes one already-parsed SSE payload to the matching
// handler. Returns true if the event was recognized (whether or not a
// handler was registered for it).
function dispatchAgentEvent(
  event: string,
  parsed: unknown,
  handlers: AgentStreamHandlers,
): boolean {
  switch (event) {
    case 'agent_start': {
      const p = parsed as { run_id?: string; agent?: string; stage?: string; round?: number; started_at_ms?: number; trace_id?: string }
      if (typeof p.run_id === 'string') {
        handlers.onAgentStart?.({
          nodeId: nodeIdOf(parsed),
          runId: p.run_id,
          agent: typeof p.agent === 'string' ? p.agent : '',
          stage: (p.stage ?? 'worker') as Stage,
          round: typeof p.round === 'number' ? p.round : undefined,
          startedAtMs: typeof p.started_at_ms === 'number' ? p.started_at_ms : undefined,
          traceId: typeof p.trace_id === 'string' ? p.trace_id : undefined,
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
          score: typeof p.score === 'number' ? p.score : undefined,
          passed: p.passed === true,
          threshold: typeof (p.envelope as { threshold?: unknown } | undefined)?.threshold === 'number' ? (p.envelope as { threshold: number }).threshold : undefined,
          feedback: typeof p.feedback === 'string' ? p.feedback : undefined,
          status: typeof p.status === 'string' ? p.status : undefined,
          reason: typeof p.reason === 'string' ? p.reason : undefined,
          finishReason: typeof p.finish_reason === 'string' ? p.finish_reason : undefined,
          model: typeof p.model === 'string' ? p.model : undefined,
          totalTokens: typeof p.total_tokens === 'number' ? p.total_tokens : undefined,
          contextTokens: typeof p.context_tokens === 'number' ? p.context_tokens : undefined,
          finishedAtMs: typeof p.finished_at_ms === 'number' ? p.finished_at_ms : undefined,
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
    case 'response_created':
      if (hasStringField(parsed, 'response_id')) handlers.onResponseCreated?.(parsed.response_id)
      return true
    // DAG lifecycle events (M3)
    case 'dag_plan': {
      const p = parsed as { plan_id?: string; nodes?: unknown[]; edges?: unknown[]; started_at_ms?: number; trace_id?: string }
      handlers.onDagPlan?.({
        planId: typeof p.plan_id === 'string' ? p.plan_id : '',
        nodes: (p.nodes ?? []) as DagNodeDef[],
        edges: (p.edges ?? []) as DagEdgeDef[],
        startedAtMs: typeof p.started_at_ms === 'number' ? p.started_at_ms : undefined,
        traceId: typeof p.trace_id === 'string' ? p.trace_id : undefined,
      })
      return true
    }
    case 'node_queued':
      if (hasStringField(parsed, 'node_id')) handlers.onNodeQueued?.(parsed.node_id)
      return true
    case 'node_start': {
      const p = parsed as { node_id?: string; agent?: string; started_at_ms?: number; trace_id?: string; resumed_from?: string }
      if (typeof p.node_id === 'string') {
        handlers.onNodeStart?.(p.node_id, typeof p.agent === 'string' ? p.agent : '',
          typeof p.started_at_ms === 'number' ? p.started_at_ms : undefined,
          typeof p.trace_id === 'string' ? p.trace_id : undefined,
          typeof p.resumed_from === 'string' ? p.resumed_from : undefined)
      }
      return true
    }
    case 'node_done': {
      const p = parsed as {
        node_id?: string; output_preview?: string
        model?: string; prompt_tokens?: number; completion_tokens?: number
        reasoning_tokens?: number; total_tokens?: number; cached_tokens?: number; context_tokens?: number
        finish_reason?: string; duration_ms?: number; finished_at_ms?: number
        judge_rounds?: number; judge_final_score?: number; judge_passed?: boolean
      }
      if (typeof p.node_id === 'string') {
        const meta: NodeDoneMeta = {
          model: p.model,
          promptTokens: p.prompt_tokens,
          completionTokens: p.completion_tokens,
          reasoningTokens: p.reasoning_tokens,
          totalTokens: p.total_tokens,
          cachedTokens: p.cached_tokens,
          contextTokens: p.context_tokens,
          finishReason: p.finish_reason,
          durationMs: p.duration_ms,
          finishedAtMs: typeof p.finished_at_ms === 'number' ? p.finished_at_ms : undefined,
          judgeRounds: p.judge_rounds,
          judgeFinalScore: p.judge_final_score,
          judgePassed: p.judge_passed,
        }
        handlers.onNodeDone?.(p.node_id, typeof p.output_preview === 'string' ? p.output_preview : '', meta)
      }
      return true
    }
    case 'node_failed': {
      const p = parsed as { node_id?: string; error?: string; finished_at_ms?: number }
      if (typeof p.node_id === 'string') {
        handlers.onNodeFailed?.(p.node_id, typeof p.error === 'string' ? p.error : '',
          typeof p.finished_at_ms === 'number' ? p.finished_at_ms : undefined)
      }
      return true
    }
    case 'node_cancelled': {
      const p = parsed as { node_id?: string; finished_at_ms?: number }
      if (typeof p.node_id === 'string') {
        handlers.onNodeCancelled?.(p.node_id, typeof p.finished_at_ms === 'number' ? p.finished_at_ms : undefined)
      }
      return true
    }
    case 'node_paused':
      if (hasStringField(parsed, 'node_id')) handlers.onNodePaused?.(parsed.node_id)
      return true
    case 'node_steered': {
      const p = parsed as { node_id?: string; guidance?: string }
      if (typeof p.node_id === 'string') {
        handlers.onNodeSteered?.(p.node_id, typeof p.guidance === 'string' ? p.guidance : '')
      }
      return true
    }
    case 'node_needs_input': {
      const p = parsed as { node_id?: string; interrupt_id?: string; message?: string }
      if (typeof p.node_id === 'string') {
        handlers.onNodeNeedsInput?.(p.node_id,
          typeof p.interrupt_id === 'string' ? p.interrupt_id : '',
          typeof p.message === 'string' ? p.message : '')
      }
      return true
    }
    case 'compaction': {
      const p = parsed as {
        node_id?: string; run_id?: string; start_timestamp?: string; end_timestamp?: string
        summary_input_tokens?: number; summary_output_tokens?: number
      }
      if (typeof p.node_id === 'string') {
        handlers.onCompaction?.({
          nodeId: p.node_id,
          runId: typeof p.run_id === 'string' ? p.run_id : '',
          startTimestamp: p.start_timestamp,
          endTimestamp: p.end_timestamp,
          summaryInputTokens: p.summary_input_tokens,
          summaryOutputTokens: p.summary_output_tokens,
        })
      }
      return true
    }
    case 'delivery_result': {
      const p = parsed as { node_id?: string; outcome?: string; kind?: string; url?: string; error?: string; trace_id?: string }
      if (typeof p.node_id === 'string' && typeof p.outcome === 'string') {
        handlers.onDeliveryResult?.({
          nodeId: p.node_id,
          outcome: p.outcome as DeliveryResultPayload['outcome'],
          kind: typeof p.kind === 'string' ? p.kind : undefined,
          url: typeof p.url === 'string' ? p.url : undefined,
          error: typeof p.error === 'string' ? p.error : undefined,
          traceId: typeof p.trace_id === 'string' ? p.trace_id : undefined,
        })
      }
      return true
    }
    case 'artifact_revision': {
      const p = parsed as { id?: string; revision?: number; kind?: string; node_id?: string; round?: number }
      if (typeof p.id === 'string') {
        handlers.onArtifactRevision?.({
          id: p.id,
          revision: typeof p.revision === 'number' ? p.revision : 0,
          kind: typeof p.kind === 'string' ? p.kind : '',
          nodeId: typeof p.node_id === 'string' ? p.node_id : '',
          round: typeof p.round === 'number' ? p.round : 0,
        })
      }
      return true
    }
    case 'artifact_judge_round': {
      const p = parsed as { id?: string; passed?: boolean; score?: number; scored?: { artifact_id?: string; revision?: number }[] }
      if (typeof p.id === 'string') {
        handlers.onArtifactJudgeRound?.({
          id: p.id,
          passed: p.passed === true,
          score: typeof p.score === 'number' ? p.score : 0,
          scored: (p.scored ?? []).map(s => ({
            artifactId: typeof s.artifact_id === 'string' ? s.artifact_id : '',
            revision: typeof s.revision === 'number' ? s.revision : 0,
          })),
        })
      }
      return true
    }
  }
  return false
}

// Parses a fetched SSE ReadableStream (the chat send flow: request body +
// response stream). Returns whether a `done` event was actually seen before
// the body ended - the caller's only signal the stream ended cleanly (vs. a dropped connection worth reconnecting over) - plus the highest `id:` line seen, so a dropped-connection handoff can resume past it instead of replaying the whole run (every event carries `id:`, same sseWriter as the GET stream; EventSource just parses it there). A read error (anything but an intentional abort) is treated the same as the body closing: report done=false and let the caller reconnect.
export async function readAgentStream(
  body: ReadableStream<Uint8Array>,
  handlers: AgentStreamHandlers,
): Promise<{ done: boolean; lastEventId: number }> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  let currentEvent = 'message'
  let sawDone = false
  let lastEventId = 0
  // pendingId is the `id:` line's value for the event currently being parsed;
  // it only becomes lastEventId once that event's `data:` line actually
  // dispatches, so a drop between the two (a real TCP boundary) can't advance past an event never applied.
  let pendingId = 0
  while (true) {
    let chunk: ReadableStreamReadResult<Uint8Array>
    try {
      chunk = await reader.read()
    } catch (err) {
      if ((err as Error)?.name === 'AbortError') throw err
      break
    }
    if (chunk.done) break
    buf += decoder.decode(chunk.value, { stream: true })
    const lines = buf.split('\n')
    buf = lines.pop()!
    for (const line of lines) {
      if (line.startsWith('id: ')) {
        const id = Number(line.slice(4).trim())
        if (Number.isFinite(id)) pendingId = id
        continue
      }
      if (line.startsWith('event: ')) {
        currentEvent = line.slice(7).trim()
        continue
      }
      if (!line.startsWith('data: ')) continue
      const raw = line.slice(6).trim()
      if (!raw) continue
      let parsed: unknown
      try { parsed = JSON.parse(raw) } catch { continue }
      if (currentEvent === 'done') sawDone = true
      dispatchAgentEvent(currentEvent, parsed, handlers)
      if (pendingId > lastEventId) lastEventId = pendingId
    }
  }
  return { done: sawDone, lastEventId }
}

// attachAgentEventSource wires an EventSource (chatStore.openEventSource, the chat stream) to
// the same handler shape readAgentStream consumes. Returns a teardown that
// closes the EventSource. shouldDispatch, if given, gates each event BEFORE
// it reaches handlers - e.g. chatStore's id-contiguity check (#audit-6):
// a gap must never be applied, so the gate has to run ahead of dispatch,
// not as a second independent listener racing it.
export function attachAgentEventSource(
  es: EventSource,
  handlers: AgentStreamHandlers,
  shouldDispatch?: (e: MessageEvent) => boolean,
): () => void {
  for (const name of AGENT_EVENT_NAMES) {
    es.addEventListener(name, (e) => {
      if (shouldDispatch && !shouldDispatch(e as MessageEvent)) return
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
