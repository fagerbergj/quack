import { readAgentStream, attachAgentEventSource, type AgentStreamHandlers, type DagNodeDef, type DagEdgeDef, type NodeDoneMeta, type Stage, type ArtifactRevisionPayload, type ArtifactJudgeRoundPayload } from './agentStream'
import {
  startRun,
  appendRunThinking,
  appendRunToolCall,
  fillRunToolResult,
  appendRunCompaction,
  completeRun,
  freezeOpenRuns,
  type AgentRun,
} from '../components/AgentParts'
import type { Turn, DagOutputItem, MessageOutputItem, NodeStatus, PauseReason, QueuedMessage, Usage, A2UiAction, SendMessageBody } from '../generated'
import {
  editNodeTask as sdkEditNodeTask,
  editQueuedMessage as sdkEditQueuedMessage,
  queueNodeMessage as sdkQueueNodeMessage,
  removeQueuedMessage as sdkRemoveQueuedMessage,
  startNode as sdkStartNode,
  stopNode as sdkStopNode,
  updateNodeStatus,
  updateResponseStatus,
} from '../generated'
import { api } from '../api'
import { a2uiActionText, A2UI_SURFACE_KIND } from '../lib/a2ui'
import { agentLabel } from '../components/messageParts'

export type { NodeStatus, PauseReason, QueuedMessage }

// Unclamped server start: a finished duration is server_finish - server_start on one clock, so skew never enters.
// LiveTimer floors the live case at 0; Date.now() covers a live event with no server timestamp.
function anchorTime(serverMs?: number): number {
  return serverMs ?? Date.now()
}

export interface NodeState {
  status: NodeStatus
  outputPreview?: string
  error?: string
  // Set while status === 'needs_input': the node's question, answered via startNode(chatId, nodeId, answer).
  question?: string
  // Best-effort for a live pause (node_paused/node_needs_input omit it); authoritative after a reload.
  pauseReason?: PauseReason
  startedAt?: number
  finishedAt?: number
  outputChars?: number
  model?: string
  promptTokens?: number
  completionTokens?: number
  reasoningTokens?: number
  totalTokens?: number
  cachedTokens?: number
  // Last measured prompt tokens from the node's latest worker/revise round: the context meter's "used" reading.
  contextTokens?: number
  finishReason?: string
  serverDurationMs?: number
  judgeRounds?: number
  judgeFinalScore?: number
  judgePassed?: boolean
  // OTel trace id for this node's run; render via clientConfig.traceUrl(). ""/absent when otel is disabled.
  traceId?: string
  // Set when this dispatch reused an existing node id (node_start's
  // resumed_from) - the prior context it continues on. Absent for a fresh node.
  resumedFrom?: string
  steers?: string[]   // guidance folded in when a queued message was delivered, in order
  // Optimistic local copy of the node's message queue; no SSE event syncs it.
  // Cleared on node_steered, when the queue was drained and delivered.
  queue?: QueuedMessage[]
}

export interface DagTurnState {
  planId: string
  nodes: DagNodeDef[]
  edges: DagEdgeDef[]
  nodeStates: Record<string, NodeState>
  nodeRuns: Record<string, AgentRun[]>   // ordered agent runs per node
  nodeAnswer: Record<string, string>     // final vetted answer text per node
  startedAt?: number
  finishedAt?: number
}

// LiveTurn is the in-progress / seeded state for one chat turn.
interface LiveTurn {
  id: string             // turn ID (response_id) - empty string while streaming before first event
  userText: string
  // Server start time; known only for a persisted turn attach() lifted back into `live`.
  createdAt?: string
  dag?: DagTurnState
  // Top-level fields for orchestrator responses that don't go through a DAG node.
  text: string           // accumulated answer text from node-less agent_token events
  runs: AgentRun[]       // agent runs (thinking, tool calls) at the top level
  streaming: boolean
  error: string
  // The turn's persisted answer: shown once the run ends, since a replayed or retried run streams only the
  // nodes it re-ran. Set (even '') for an existing turn's run, which re-reads it at done.
  answer?: string
}

// A follow-up typed while the run streams: held client-side (no endpoint, unlike the per-node queue)
// and auto-submitted in order once the run ends.
export interface QueuedTurn {
  id: string
  text: string
}

export interface ChatState {
  // Completed turns from history (seeded from GET /chats/{id})
  turns: Turn[]
  // The turn currently streaming (or most recently completed, until next submit)
  live?: LiveTurn
  error: string
  // True between submit and the first stream event, so the UI can show a
  // loading indicator instantly instead of waiting on the archive round-trip.
  submitting?: boolean
  pendingUserText?: string
  // Follow-ups queued while `live.streaming` was true, in send order.
  queue: QueuedTurn[]
  // Chat-wide token aggregate from ChatDetail.usage - a snapshot as of the
  // last seed(), not updated live while a run streams.
  usage?: Usage
  // Latest artifact SSE events for late subscribers (ArtifactPanel) via subscribe().
  // seq bumps on every event so a repeated payload (e.g. a reconnect replay) still registers.
  artifactEvents?: { revision?: ArtifactRevisionPayload; judgeRound?: ArtifactJudgeRoundPayload; seq: number }
  // Bumped only by a2ui_surface revisions; artifactEvents is last-write-wins and render_ui's quiz_key event follows.
  surfaceSeq?: number
  // Surface artifact id -> live turn its first revision streamed in; fallback when turn_id is absent (MCP runs).
  surfacePins?: Record<string, string>
}

type Listener = () => void
type SdkResult = { error?: unknown; response?: Response }

export const EMPTY_STATE: ChatState = { turns: [], error: '', queue: [] }

// retrySet returns nodeId plus every node transitively downstream of it (via the
// DAG edges) - the subgraph a retry re-runs because the target's output feeds them.
function retrySet(edges: DagEdgeDef[], nodeId: string): Set<string> {
  const dependents = new Map<string, string[]>()
  for (const e of edges) {
    const arr = dependents.get(e.from) ?? []
    arr.push(e.to)
    dependents.set(e.from, arr)
  }
  const set = new Set<string>()
  const walk = (id: string) => {
    if (set.has(id)) return
    set.add(id)
    for (const d of dependents.get(id) ?? []) walk(d)
  }
  walk(nodeId)
  return set
}

// Persisted-shaped Turn from a finished LiveTurn, for when submit()'s refetch races the server persisting it.
// DAG turns keep only the sinks' answer text: enough to render, not a full DagOutputItem.
function turnFromLiveTurn(live: LiveTurn): Turn {
  const answer = live.dag && sinkNodeIds(live.dag.nodes).length > 0 ? dagAnswer(live.dag) : undefined
  const text = answer ? liveAnswerText(live) : live.text
  const stopped = answer?.stopped || undefined
  return {
    id: live.id,
    created_at: live.createdAt ?? new Date().toISOString(),
    input: { role: 'user', content: live.userText },
    output: [{ id: `${live.id}-msg`, type: 'message', status: 'completed', content: [{ type: 'output_text', text }], stopped }],
  }
}

// A first revision streaming in means the live turn created that surface.
function surfaceEventState(s: ChatState, d: ArtifactRevisionPayload): Partial<ChatState> {
  if (d.kind !== A2UI_SURFACE_KIND) return {}
  const pin = d.revision === 1 && s.live?.id && !s.surfacePins?.[d.id] ? { [d.id]: s.live.id } : {}
  return { surfaceSeq: (s.surfaceSeq ?? 0) + 1, surfacePins: { ...s.surfacePins, ...pin } }
}

// Capped exponential backoff so a dead server isn't hammered, bounded so a gone server surfaces as an error.
const MAX_RECONNECT_ATTEMPTS = 6
const RECONNECT_BASE_DELAY_MS = 1000
const RECONNECT_MAX_DELAY_MS = 15000
function reconnectDelay(attempt: number): number {
  return Math.min(RECONNECT_BASE_DELAY_MS * 2 ** attempt, RECONNECT_MAX_DELAY_MS)
}

export class ChatStore {
  private states = new Map<string, ChatState>()
  private listeners = new Map<string, Set<Listener>>()
  private controllers = new Map<string, AbortController>()
  private eventSources = new Map<string, () => void>()  // chatID → teardown for an attached subscribe stream
  private reconnectTimers = new Map<string, ReturnType<typeof setTimeout>>()  // chatID → pending reconnect
  private generations = new Map<string, number>()
  private onTitleCallbacks = new Map<string, (title: string) => void>()  // chatID → last submit()'s onTitle, for drainQueue
  private notifyScheduled = new Set<string>()  // chatID → a coalesced notify is already queued for the next frame

  get(chatId: string): ChatState {
    return this.states.get(chatId) ?? EMPTY_STATE
  }

  subscribe(chatId: string, listener: Listener): () => void {
    let set = this.listeners.get(chatId)
    if (!set) {
      set = new Set()
      this.listeners.set(chatId, set)
    }
    set.add(listener)
    return () => {
      set!.delete(listener)
      if (set!.size === 0) this.listeners.delete(chatId)
    }
  }

  seed(chatId: string, turns: Turn[], usage?: Usage): void {
    const cur = this.states.get(chatId)
    if (cur && (cur.live?.streaming || cur.turns.length > 0)) return
    // Preserve a queue accumulated before the chat ever had a turn (e.g. queued
    // during the very first, still-streaming run) across this reseed.
    this.write(chatId, { ...EMPTY_STATE, turns, usage, queue: cur?.queue ?? [] })
  }

  clear(chatId: string): void {
    this.controllers.get(chatId)?.abort()
    this.controllers.delete(chatId)
    this.eventSources.get(chatId)?.()
    this.eventSources.delete(chatId)
    this.cancelReconnect(chatId)
    this.onTitleCallbacks.delete(chatId)
    this.states.delete(chatId)
    this.bumpGeneration(chatId)
    this.notify(chatId)
  }

  private cancelReconnect(chatId: string): void {
    const t = this.reconnectTimers.get(chatId)
    if (t) {
      clearTimeout(t)
      this.reconnectTimers.delete(chatId)
    }
  }

  // An A2UI button press is an ordinary turn whose user text is the action line the backend persists.
  submitA2uiAction(chatId: string, action: A2UiAction): Promise<void> {
    return this.submit(chatId, '', undefined, undefined, action)
  }

  async submit(chatId: string, content: string, files?: File[], onTitle?: (title: string) => void, a2uiAction?: A2UiAction): Promise<void> {
    const trimmed = a2uiAction ? a2uiActionText(a2uiAction) : content.trim()
    if (!trimmed) return
    let cur = this.get(chatId)
    // submitting covers the archive GET below, before live.streaming flips on - a double click lands there.
    if (cur.live?.streaming || cur.submitting) return
    // Remembered so drainQueue's auto-submit of a later queued message can
    // still report a title change, without the caller having to re-pass it.
    if (onTitle) this.onTitleCallbacks.set(chatId, onTitle)

    // A finished turn still sits in `live`: archive it into `turns` via a refetch made BEFORE posting the new turn.
    // `submitting` shows the spinner at once; the old `live` stays rendered until `turns` repopulates.
    if (cur.live) {
      this.write(chatId, { ...cur, submitting: true, pendingUserText: trimmed, error: '' })
      let turns = cur.turns
      try {
        turns = (await api.getChat(chatId)).turns ?? turns
      } catch { /* keep local state; worst case the previous turn drops until refresh */ }
      // The refetch can race the server persisting the finished turn; synthesize it from `live` rather than drop it.
      if (cur.live && !turns.some(t => t.id === cur.live!.id)) {
        turns = [...turns, turnFromLiveTurn(cur.live)]
      }
      cur = { ...this.get(chatId), turns }
    }

    const live: LiveTurn = { id: '', userText: trimmed, streaming: true, error: '', text: '', runs: [] }
    this.write(chatId, { ...cur, live, submitting: false, pendingUserText: undefined, error: '' })

    await this.runStream(
      chatId,
      signal => {
        if (files && files.length > 0) {
          const fd = new FormData()
          fd.append('content', trimmed)
          for (const f of files) fd.append('files', f)
          return fetch(`/api/v1/chats/${chatId}/responses`, { method: 'POST', body: fd, signal })
        }
        const body: SendMessageBody = a2uiAction ? { content: '', a2ui_action: a2uiAction } : { content: trimmed }
        return fetch(`/api/v1/chats/${chatId}/responses`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
          signal,
        })
      },
      onTitle,
    )
  }

  // Holds a follow-up client-side while the run streams; drainQueue submits it when the run ends.
  queueTurn(chatId: string, text: string): void {
    const trimmed = text.trim()
    if (!trimmed) return
    const s = this.get(chatId)
    this.write(chatId, { ...s, queue: [...s.queue, { id: crypto.randomUUID(), text: trimmed }] })
  }

  // unqueueTurn drops a not-yet-sent queued message.
  unqueueTurn(chatId: string, id: string): void {
    const s = this.get(chatId)
    this.write(chatId, { ...s, queue: s.queue.filter(m => m.id !== id) })
  }

  // Called from finishStream, so it fires however the run ended (normal, stopped, or reconnected).
  // One at a time: the auto submit() re-enters finishStream when it completes, draining the rest in order.
  private drainQueue(chatId: string): void {
    const s = this.states.get(chatId)
    if (!s || s.queue.length === 0) return
    const [next, ...rest] = s.queue
    this.write(chatId, { ...s, queue: rest })
    void this.submit(chatId, next.text, undefined, this.onTitleCallbacks.get(chatId))
  }

  // Cancels by the response id from response_created; before it arrives only the local connection aborts.
  stop(chatId: string): void {
    const responseId = this.states.get(chatId)?.live?.id
    if (responseId) {
      void updateResponseStatus({ path: { chat_id: chatId, response_id: responseId }, body: { status: 'cancelled' } })
    }
    this.controllers.get(chatId)?.abort()
  }

  // Cancels one node (queued, running, or paused); the rest of the DAG keeps going and the local stream stays open.
  stopNode(chatId: string, nodeId: string): void {
    void sdkStopNode({ path: { chat_id: chatId, node_id: nodeId } }).then(r => this.rejected(chatId, nodeId, 'stop', r))
  }

  // Suspends a running node at its next turn boundary, keeping its work. Not optimistic, like stopNode.
  // reason defaults to "user" server-side.
  pauseNode(chatId: string, nodeId: string, reason?: PauseReason): void {
    void updateNodeStatus({ path: { chat_id: chatId, node_id: nodeId }, body: { status: 'paused', reason } })
      .then(r => this.rejected(chatId, nodeId, 'pause', r))
  }

  // Starts a queued node or resumes a paused one, reusing the plan's other stored outputs.
  // answer replies to a node paused awaiting_input; same optimistic reset + resubscribe as retryNode.
  startNode(chatId: string, nodeId: string, answer?: string): void {
    const s = this.states.get(chatId)
    if (!s?.live?.dag) return
    if (s.live.streaming) {
      // Mid-run start would race the open stream; the menu hides Start then, but popup/bubble paths still land here.
      this.markNodeError(chatId, nodeId, 'a run is still streaming; wait for it to finish before starting this node')
      return
    }
    this.rerunNode(chatId, nodeId, 'start', () =>
      sdkStartNode({ path: { chat_id: chatId, node_id: nodeId }, body: { content: answer } }))
  }

  // Queues a message for a running node, delivered at its next turn boundary, never mid-turn.
  // 404s (shown as a node error note) if the node isn't running.
  async queueNodeMessage(chatId: string, nodeId: string, text: string): Promise<void> {
    const message = text.trim()
    if (!message) return
    const r = await sdkQueueNodeMessage({ path: { chat_id: chatId, node_id: nodeId }, body: { message } })
    const created = r.data
    if (this.rejected(chatId, nodeId, 'queue', r) || !created) return
    this.updateNodeQueue(chatId, nodeId, q => [...q, created])
  }

  // editQueuedMessage rewrites a not-yet-delivered queued message.
  async editQueuedMessage(chatId: string, nodeId: string, messageId: string, text: string): Promise<void> {
    const message = text.trim()
    if (!message) return
    const r = await sdkEditQueuedMessage({ path: { chat_id: chatId, node_id: nodeId, message_id: messageId }, body: { message } })
    if (r.response?.ok) {
      this.updateNodeQueue(chatId, nodeId, q => q.map(m => m.id === messageId ? { ...m, text: message } : m))
    }
  }

  // removeQueuedMessage drops a not-yet-delivered queued message.
  async removeQueuedMessage(chatId: string, nodeId: string, messageId: string): Promise<void> {
    const r = await sdkRemoveQueuedMessage({ path: { chat_id: chatId, node_id: nodeId, message_id: messageId } })
    if (r.response?.ok) {
      this.updateNodeQueue(chatId, nodeId, q => q.filter(m => m.id !== messageId))
    }
  }

  private updateNodeQueue(chatId: string, nodeId: string, fn: (q: QueuedMessage[]) => QueuedMessage[]): void {
    const s = this.get(chatId)
    const dag = s.live?.dag
    if (!s.live || !dag?.nodeStates[nodeId]) return
    const prev = dag.nodeStates[nodeId].queue ?? []
    const nodeStates = { ...dag.nodeStates, [nodeId]: { ...dag.nodeStates[nodeId], queue: fn(prev) } }
    this.write(chatId, { ...s, live: { ...s.live, dag: { ...dag, nodeStates } } })
  }

  // Only legal before the node starts (409 after, e.g. a downstream node waiting on its deps).
  // Updates the local plan def on success.
  async editNodeTask(chatId: string, nodeId: string, task: string): Promise<boolean> {
    const trimmed = task.trim()
    if (!trimmed) return false
    const r = await sdkEditNodeTask({ path: { chat_id: chatId, node_id: nodeId }, body: { task: trimmed } })
    if (this.rejected(chatId, nodeId, 'edit', r)) return false
    const s = this.get(chatId)
    const dag = s.live?.dag
    if (s.live && dag) {
      const nodes = dag.nodes.map(n => n.id === nodeId ? { ...n, task: trimmed } : n)
      this.write(chatId, { ...s, live: { ...s.live, dag: { ...dag, nodes } } })
    }
    return true
  }

  // markNodeError annotates a live DAG node with a transient error note (used
  // for rejected control actions - the next stream event for the node clears it).
  private markNodeError(chatId: string, nodeId: string, msg: string): void {
    const cur = this.get(chatId)
    const dag = cur.live?.dag
    if (!cur.live || !dag?.nodeStates[nodeId]) return
    const nodeStates = { ...dag.nodeStates, [nodeId]: { ...dag.nodeStates[nodeId], error: msg } }
    this.write(chatId, { ...cur, live: { ...cur.live, dag: { ...dag, nodeStates } } })
  }

  // rejected reports a control request that failed, noting the server's message on the node;
  // a network failure (no response) stays silent.
  private rejected(chatId: string, nodeId: string, verb: string, r: SdkResult): boolean {
    if (r.response?.ok) return false
    if (r.response) {
      const msg = (r.error as { error?: string } | undefined)?.error
      this.markNodeError(chatId, nodeId, msg || `${verb} rejected (HTTP ${r.response.status})`)
    }
    return true
  }

  // Re-runs a finished node and its descendants, reusing the rest's stored outputs; guidance folds into its task.
  // The PUT returns at once, so the subgraph resets to queued locally and the GET stream relay rebuilds it.
  retryNode(chatId: string, nodeId: string, guidance?: string): void {
    const s = this.states.get(chatId)
    if (!s?.live?.dag || s.live.streaming) return
    const g = guidance?.trim() || undefined
    this.rerunNode(chatId, nodeId, 'retry', () =>
      updateNodeStatus({ path: { chat_id: chatId, node_id: nodeId }, body: { status: 'queued', guidance: g } }))
  }

  // Resets nodeId and its descendants to queued, then watches the re-run over the GET stream once
  // request succeeds. Callers check the chat has an idle live DAG.
  private rerunNode(chatId: string, nodeId: string, label: string, request: () => Promise<SdkResult>): void {
    const s = this.get(chatId)
    const dag = s.live!.dag!
    const nodeStates = { ...dag.nodeStates }
    const nodeAnswer = { ...dag.nodeAnswer }
    const nodeRuns = { ...dag.nodeRuns }
    for (const id of retrySet(dag.edges, nodeId)) {
      nodeStates[id] = { status: 'queued' }
      nodeAnswer[id] = ''
      nodeRuns[id] = []
    }
    this.write(chatId, { ...s, live: { ...s.live!, streaming: true, error: '', answer: '', dag: { ...dag, nodeStates, nodeAnswer, nodeRuns, finishedAt: undefined } } })
    const generation = this.bumpGeneration(chatId)
    void request().then(r => {
      if (r.response?.ok) return this.subscribeToStream(chatId, generation)
      const cur = this.states.get(chatId)
      if (!cur?.live) return
      const msg = r.response ? `${label} failed: HTTP ${r.response.status}` : (r.error as Error | undefined)?.message || `${label} failed`
      this.write(chatId, { ...cur, error: msg, live: { ...cur.live, streaming: false } })
    })
  }

  isStreaming(chatId: string): boolean {
    return this.states.get(chatId)?.live?.streaming ?? false
  }

  private async runStream(
    chatId: string,
    fetchFn: (signal: AbortSignal) => Promise<Response>,
    onTitle?: (title: string) => void,
  ): Promise<void> {
    const controller = new AbortController()
    this.controllers.set(chatId, controller)
    const generation = this.bumpGeneration(chatId)
    let handedOff = false
    try {
      const res = await fetchFn(controller.signal)
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        throw new Error((data as { error?: string }).error || `${res.status} ${res.statusText}`)
      }
      if (!res.body) throw new Error('No response body')

      let streamError = ''
      const result = await readAgentStream(res.body, this.streamHandlers(chatId, msg => { streamError = msg }, onTitle))
      if (streamError) throw new Error(streamError)
      if (!result.done) {
        // The body ended without `done`: the connection dropped mid-run. Hand off to the resumable GET stream,
        // resuming past the last applied id so it doesn't replay what the POST body delivered.
        handedOff = true
        this.openEventSource(chatId, generation, result.lastEventId, 0)
      }
    } catch (err: unknown) {
      if ((err as Error)?.name !== 'AbortError') {
        const msg = (err as Error)?.message || 'Request failed'
        const s = this.states.get(chatId)
        if (s) this.write(chatId, { ...s, error: msg })
      }
    } finally {
      if (this.controllers.get(chatId) === controller) {
        this.controllers.delete(chatId)
      }
      if (!handedOff) this.finishStream(chatId, generation)
    }
  }

  // Subscribes a client that did NOT post this run (a refresh, another device); the hub replays it, then tails live.
  // No-op if this client already streams, so a run it started is never double-fed.
  attach(chatId: string): void {
    if (this.isStreaming(chatId) || this.eventSources.has(chatId)) return
    const cur = this.get(chatId)
    // Lift the in-progress run (the latest seeded turn) into `live`, seeded from what GET /chats/{id} persisted:
    // the hub only publishes NEW events, so a late attach would otherwise show nothing until the next one.
    const last = cur.turns[cur.turns.length - 1]
    const dagItem = last ? dagFromTurn(last) : undefined
    const live: LiveTurn = {
      id: last?.id ?? '',
      userText: last?.input.content ?? '',
      createdAt: last?.created_at,
      streaming: true,
      error: '',
      text: last ? textFromTurn(last) : '',
      runs: last ? activityFromTurn(last) : [],
      dag: dagItem ? dagTurnStateFromItem(dagItem) : undefined,
      answer: dagItem ? textFromTurn(last) : undefined,
    }
    this.write(chatId, { ...cur, turns: cur.turns.slice(0, -1), live })

    const generation = this.bumpGeneration(chatId)
    this.subscribeToStream(chatId, generation)
  }

  // reattach watches a run started elsewhere (a CLI retry, a webhook) on an open chat: re-seeded from the
  // server, attach lifts that run's own turn, never a finished one. No-op while this page streams.
  reattach(chatId: string, turns: Turn[]): void {
    const cur = this.get(chatId)
    if (cur.live?.streaming || this.eventSources.has(chatId)) return
    this.write(chatId, { ...cur, turns, live: undefined })
    this.attach(chatId)
  }

  // Watches a run over the GET stream from event 0 (attach, retryNode/startNode); callers seed `live` first.
  // No resume cursor: attach has no durable one yet, and a re-run's seq restarts at 1 (EventLog.Reset).
  private subscribeToStream(chatId: string, generation: number): void {
    this.openEventSource(chatId, generation, 0, 0)
  }

  // lastEventId resumes from the server's event log; attempt drives capped backoff, reset by every event received.
  // The server closes after `done`, which EventSource reports as `error` too; sawDone tells that apart from a drop.
  private openEventSource(chatId: string, generation: number, lastEventId: number, attempt: number): void {
    const url = lastEventId > 0
      ? `/api/v1/chats/${chatId}/stream?last_event_id=${lastEventId}`
      : `/api/v1/chats/${chatId}/stream`
    const es = new EventSource(url)
    let sawDone = false
    let latestId = lastEventId
    const handlers: AgentStreamHandlers = {
      ...this.streamHandlers(chatId, msg => {
        const s = this.states.get(chatId)
        if (s) this.write(chatId, { ...s, error: msg })
      }),
      onDone: () => { sawDone = true; this.teardownStream(chatId, generation) },
    }
    // reconnect reopens from resumeFromId with bounded backoff, on a connection error or an id gap.
    // `close` is assigned below (shouldDispatch needs reconnect); reconnect only runs once an event or error lands.
    let close: () => void = () => {}
    const reconnect = (resumeFromId: number) => {
      close()
      this.eventSources.delete(chatId)
      if (this.generations.get(chatId) !== generation) return  // superseded by a newer run
      if (attempt >= MAX_RECONNECT_ATTEMPTS) {
        const s = this.states.get(chatId)
        if (s) this.write(chatId, { ...s, error: 'Lost connection to the server - reload to resume.' })
        this.finishStream(chatId, generation)
        return
      }
      const timer = setTimeout(() => {
        this.reconnectTimers.delete(chatId)
        if (this.generations.get(chatId) !== generation) return
        this.openEventSource(chatId, generation, resumeFromId, attempt + 1)
      }, reconnectDelay(attempt))
      this.reconnectTimers.set(chatId, timer)
    }
    // Gates every event before handlers: only a contiguous id advances the cursor and resets backoff.
    // A jump means the hub dropped us as a slow subscriber; reconnect from the last contiguous id to refill the gap.
    const shouldDispatch = (e: MessageEvent): boolean => {
      const id = Number(e.lastEventId)
      if (!Number.isFinite(id) || id <= latestId) { attempt = 0; return true }
      if (id === latestId + 1) {
        latestId = id
        attempt = 0
        return true
      }
      reconnect(latestId)
      return false
    }
    close = attachAgentEventSource(es, handlers, shouldDispatch)
    es.onerror = () => {
      if (sawDone) return  // teardownStream already ran from onDone
      reconnect(latestId)
    }
    this.eventSources.set(chatId, close)
  }

  // Ends an attached run on `done`; generation keeps a stale teardown from clobbering a newer run.
  private teardownStream(chatId: string, generation: number): void {
    this.detachStream(chatId)
    this.finishStream(chatId, generation)
  }

  // Closes the subscribe stream without ending the run. Clearing `streaming` (only if an EventSource closed) lets
  // a return trip re-attach; with none, this client's own POST is in flight and attach() must not feed it twice.
  detachStream(chatId: string): void {
    const close = this.eventSources.get(chatId)
    if (close) {
      close()
      this.eventSources.delete(chatId)
    }
    this.cancelReconnect(chatId)
    const s = this.states.get(chatId)
    if (close && s?.live?.streaming) {
      this.write(chatId, { ...s, live: { ...s.live, streaming: false } })
    }
  }

  // streamHandlers builds the store-updating handler set shared by both transports:
  // the POST response body (runStream) and the EventSource subscribe (attach).
  private streamHandlers(
    chatId: string,
    onError: (msg: string) => void,
    onTitle?: (title: string) => void,
  ): AgentStreamHandlers {
      // Set once THIS stream's own top-level run starts, so onDagPlan can tell it from runs merely seeded by attach()
      // from a prior turn, which it purges.
      let sawTopLevelAgentStart = false
      const updateNodeRuns = (nodeId: string | undefined, fn: (runs: AgentRun[]) => AgentRun[]) => {
        if (!nodeId) return
        const s = this.states.get(chatId)
        if (!s?.live?.dag) return
        const prev = s.live.dag.nodeRuns[nodeId] ?? []
        const dag = { ...s.live.dag, nodeRuns: { ...s.live.dag.nodeRuns, [nodeId]: fn(prev) } }
        this.write(chatId, { ...s, live: { ...s.live, dag } })
      }

      // updateTopLevelRuns updates the orchestrator-level run list (no DAG node).
      const updateTopLevelRuns = (fn: (runs: AgentRun[]) => AgentRun[]) => {
        const s = this.states.get(chatId)
        if (!s?.live) return
        this.write(chatId, { ...s, live: { ...s.live, runs: fn(s.live.runs) } })
      }

      const updateNodeAnswer = (nodeId: string | undefined, text: string) => {
        if (!nodeId) return
        const s = this.states.get(chatId)
        if (!s?.live?.dag) return
        const prev = s.live.dag.nodeAnswer[nodeId] ?? ''
        const dag = { ...s.live.dag, nodeAnswer: { ...s.live.dag.nodeAnswer, [nodeId]: prev + text } }
        const ns = dag.nodeStates[nodeId] ?? { status: 'queued' as NodeStatus }
        dag.nodeStates = { ...dag.nodeStates, [nodeId]: { ...ns, outputChars: (ns.outputChars ?? 0) + text.length } }
        this.write(chatId, { ...s, live: { ...s.live, dag } })
      }

      // updateTopLevelText appends to the orchestrator's top-level answer (no DAG node).
      const updateTopLevelText = (text: string) => {
        const s = this.states.get(chatId)
        if (!s?.live) return
        this.write(chatId, { ...s, live: { ...s.live, text: s.live.text + text } })
      }

      const updateNodeState = (nodeId: string, patch: Partial<NodeState>) => {
        const s = this.states.get(chatId)
        if (!s?.live?.dag) return
        const prev = s.live.dag.nodeStates[nodeId] ?? { status: 'queued' as NodeStatus }
        const dag = { ...s.live.dag, nodeStates: { ...s.live.dag.nodeStates, [nodeId]: { ...prev, ...patch } } }
        const allDone = dag.nodes.every(n => {
          const st = dag.nodeStates[n.id]?.status
          return st === 'done' || st === 'failed' || st === 'cancelled'
        })
        // Anchor the plan total to the latest server-stamped node finishedAt so a replay can't recompute it from now;
        // Date.now() only when no node carried one.
        if (allDone && !dag.finishedAt) {
          const nodeFinishTimes = Object.values(dag.nodeStates).map(n => n.finishedAt).filter((t): t is number => t != null)
          dag.finishedAt = nodeFinishTimes.length ? Math.max(...nodeFinishTimes) : Date.now()
        }
        this.write(chatId, { ...s, live: { ...s.live, dag } })
      }

      const runArgs = (d: { runId: string; agent: string; stage: import('./agentStream').Stage; round?: number; startedAtMs?: number }) =>
        ({ runId: d.runId, agent: d.agent, stage: d.stage, round: d.round, startedAt: anchorTime(d.startedAtMs) })

      // Worker and revise text IS the node's answer; judge commentary gets its own card. resetAnswer clears the
      // accumulator per run so HITL re-runs don't concatenate and a revision replaces the rejected draft.
      const ANSWER_STAGES: ReadonlySet<Stage> = new Set(['worker', 'revise'])
      const resetAnswer = (nodeId: string | undefined, stage: Stage) => {
        if (!nodeId || !ANSWER_STAGES.has(stage)) return
        const s = this.states.get(chatId)
        if (!s?.live?.dag) return
        this.write(chatId, { ...s, live: { ...s.live, dag: { ...s.live.dag, nodeAnswer: { ...s.live.dag.nodeAnswer, [nodeId]: '' } } } })
      }

      // resetTopLevelText clears the orchestrator's top-level answer accumulator
      // (no DAG node) - the top-level counterpart of resetAnswer above.
      const resetTopLevelText = () => {
        const s = this.states.get(chatId)
        if (!s?.live) return
        this.write(chatId, { ...s, live: { ...s.live, text: '' } })
      }

      const TERMINAL_NODE_STATUSES: ReadonlySet<NodeStatus> = new Set(['done', 'failed', 'cancelled'])

      return {
        onAgentStart: d => {
          // A fresh top-level run supersedes text from a prior one; otherwise two top-level runs on one live turn
          // (e.g. the GitHub dispatch's no-plan-ran nudge) render the answer doubled.
          if (d.nodeId) {
            resetAnswer(d.nodeId, d.stage)
            // A starting run means the node is running whatever its status said, except a terminal status:
            // a run can't legitimately restart on a finished node.
            const s = this.states.get(chatId)
            const current = s?.live?.dag?.nodeStates[d.nodeId]?.status
            if (current === undefined || !TERMINAL_NODE_STATUSES.has(current)) {
              updateNodeState(d.nodeId, { status: 'running' })
            }
          } else {
            resetTopLevelText()
            sawTopLevelAgentStart = true
          }
          return d.nodeId
            ? updateNodeRuns(d.nodeId, r => startRun(r, runArgs(d)))
            : updateTopLevelRuns(r => startRun(r, runArgs(d)))
        },
        onAgentThinking: (runId, text, nid) => nid
          ? updateNodeRuns(nid, r => appendRunThinking(r, runId, text))
          : updateTopLevelRuns(r => appendRunThinking(r, runId, text)),
        onAgentToolCall: (runId, callId, name, args, nid) => {
          // A tool call means the run's narration so far was pre-action filler, not the answer;
          // mirrors the backend reset in translate.go.
          if (nid) {
            const st = this.states.get(chatId)
            const run = st?.live?.dag?.nodeRuns?.[nid]?.find(r => r.runId === runId)
            if (run) resetAnswer(nid, run.stage)
            updateNodeRuns(nid, r => appendRunToolCall(r, runId, callId, name, args))
          } else {
            resetTopLevelText()
            updateTopLevelRuns(r => appendRunToolCall(r, runId, callId, name, args))
          }
        },
        onAgentToolResult: (runId, callId, name, result, nid) => nid
          ? updateNodeRuns(nid, r => fillRunToolResult(r, runId, callId, name, result))
          : updateTopLevelRuns(r => fillRunToolResult(r, runId, callId, name, result)),
        onAgentToken: (runId, text, nid) => {
          if (!nid) { updateTopLevelText(text); return }
          // Only answer-stage text belongs in the node's answer; judge text goes to its own card as thinking,
          // since judgePartEmitter only routes Thought-marked parts there and local models rarely mark them.
          const st = this.states.get(chatId)
          const run = st?.live?.dag?.nodeRuns?.[nid]?.find(r => r.runId === runId)
          if (run && !ANSWER_STAGES.has(run.stage)) {
            updateNodeRuns(nid, r => appendRunThinking(r, runId, text))
            return
          }
          updateNodeAnswer(nid, text)
        },
        onAgentComplete: d => {
          const completeArgs = {
            score: d.score, passed: d.passed, threshold: d.threshold, feedback: d.feedback,
            status: d.status, reason: d.reason, finishReason: d.finishReason, model: d.model, totalTokens: d.totalTokens,
          }
          // Server clock freezes the duration; Date.now() is a fallback for older servers and is wrong on replay.
          const nowMs = d.finishedAtMs ?? Date.now()
          if (d.nodeId) {
            updateNodeRuns(d.nodeId, r => completeRun(r, d.runId, completeArgs, nowMs))
            // Context meter tracks only the answer-producing stages - a judge
            // run's own context has nothing to do with the worker's window.
            if (ANSWER_STAGES.has(d.stage) && d.contextTokens != null) {
              updateNodeState(d.nodeId, { contextTokens: d.contextTokens })
            }
          } else {
            updateTopLevelRuns(r => completeRun(r, d.runId, completeArgs, nowMs))
          }
        },
        onCompaction: d => updateNodeRuns(d.nodeId, r => appendRunCompaction(r, d.runId, {
          kind: 'compaction',
          startTimestamp: d.startTimestamp,
          endTimestamp: d.endTimestamp,
          summaryInputTokens: d.summaryInputTokens,
          summaryOutputTokens: d.summaryOutputTokens,
        })),
        onChatTitle: title => onTitle?.(title),
        onError,
        // The very first event of a run: captures the response id so stop()
        // can cancel this run by id (PUT .../responses/{id}/status).
        onResponseCreated: responseId => {
          const s = this.states.get(chatId)
          if (!s?.live) return
          this.write(chatId, { ...s, live: { ...s.live, id: responseId } })
        },
        onDagPlan: plan => {
          const s = this.states.get(chatId)
          if (!s?.live) return
          // Same planId means the plan grew: keep earlier node cards. The orchestrator's
          // own run is never purged here, since its execute calls continue after dag_plan.
          const prevDag = s.live.dag
          const grown = prevDag?.planId === plan.planId
          const nodeStates: Record<string, NodeState> = grown ? { ...prevDag.nodeStates } : {}
          for (const n of plan.nodes) {
            if (!nodeStates[n.id]) nodeStates[n.id] = { status: 'queued' }
          }
          const dag: DagTurnState = {
            planId: plan.planId,
            nodes: plan.nodes,
            edges: plan.edges,
            nodeStates,
            nodeRuns: grown ? prevDag.nodeRuns : {},
            nodeAnswer: grown ? prevDag.nodeAnswer : {},
            startedAt: grown ? prevDag.startedAt : anchorTime(plan.startedAtMs),
          }
          // A FRESH plan purges runs only seeded by attach() from a prior turn (no agent_start on this stream yet);
          // their stale narration and tool calls would otherwise render under the new plan.
          const purgeSeededRuns = !grown && !sawTopLevelAgentStart
          this.write(chatId, {
            ...s,
            live: { ...s.live, dag, text: grown ? s.live.text : '', runs: purgeSeededRuns ? [] : s.live.runs },
          })
        },
        onNodeQueued: nodeId => updateNodeState(nodeId, { status: 'queued' }),
        // A mid-run worker/judge swap re-admits without a fresh node_start
        // (that fires once, at first dispatch) - flip back to running here.
        onNodeAdmitted: nodeId => updateNodeState(nodeId, { status: 'running' }),
        // Anchor timers to the server's start time (epoch ms) so a reconnect/replay
        // shows true elapsed time instead of restarting from the replay moment.
        onNodeStart: (nodeId, _agent, startedAtMs, traceId, resumedFrom) => updateNodeState(nodeId, { status: 'running', startedAt: anchorTime(startedAtMs), traceId, resumedFrom }),
        onNodeDone: (nodeId, preview, meta: NodeDoneMeta) => {
          // finishedAtMs is the server's own clock; only an older server with no
          // such field falls back to Date.now() (wrong on replay/reconnect).
          const finishedAt = meta.finishedAtMs ?? Date.now()
          // Freeze any run still counting - the node is done, so no run is live.
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, finishedAt))
          updateNodeState(nodeId, {
            status: 'done', finishedAt, outputPreview: preview,
            model: meta.model,
            promptTokens: meta.promptTokens,
            completionTokens: meta.completionTokens,
            reasoningTokens: meta.reasoningTokens,
            totalTokens: meta.totalTokens,
            cachedTokens: meta.cachedTokens,
            contextTokens: meta.contextTokens,
            finishReason: meta.finishReason,
            serverDurationMs: meta.durationMs,
            judgeRounds: meta.judgeRounds,
            judgeFinalScore: meta.judgeFinalScore,
            judgePassed: meta.judgePassed,
          })
        },
        onNodeFailed: (nodeId, error, finishedAtMs) => {
          const finishedAt = finishedAtMs ?? Date.now()
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, finishedAt))
          updateNodeState(nodeId, { status: 'failed', finishedAt, error })
        },
        onNodeCancelled: (nodeId, finishedAtMs) => {
          // Stopped by the user: rendered neutrally ("stopped"), not as a red failure.
          const finishedAt = finishedAtMs ?? Date.now()
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, finishedAt))
          updateNodeState(nodeId, { status: 'cancelled', finishedAt, error: undefined })
        },
        onNodePaused: nodeId => {
          // Suspended, resumable, and not terminal for allDone. The event carries no reason, so "user" holds until
          // the next reload reads the persisted pause_reason.
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          updateNodeState(nodeId, { status: 'paused', error: undefined, pauseReason: 'user' })
        },
        onNodeNeedsInput: (nodeId, _interruptId, message) => {
          // Mid-node HITL: the node paused to ask the user. Freeze its open runs
          // and mark it waiting; the answer is delivered via startNode(chatId, nodeId, answer).
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          updateNodeState(nodeId, { status: 'needs_input', question: message, pauseReason: 'awaiting_input' })
        },
        onNodeSteered: (nodeId, guidance) => {
          // Re-running with new guidance in the same session: status stays 'running', since running->queued is illegal
          // backend-side. A fresh node_start through node_done follows on this stream.
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          const s = this.states.get(chatId)
          const prevSteers = s?.live?.dag?.nodeStates[nodeId]?.steers ?? []
          updateNodeState(nodeId, { status: 'running', error: undefined, steers: [...prevSteers, guidance], queue: [] })
        },
        // A delivery can carry the trace id a missed node_start had (e.g. reconnect mid-run); fill the link in.
        onDeliveryResult: d => {
          if (!d.traceId) return
          const s = this.states.get(chatId)
          if (s?.live?.dag?.nodeStates[d.nodeId]?.traceId) return
          updateNodeState(d.nodeId, { traceId: d.traceId })
        },
        onArtifactRevision: d => {
          const s = this.get(chatId)
          const seq = (s.artifactEvents?.seq ?? 0) + 1
          this.write(chatId, { ...s, ...surfaceEventState(s, d), artifactEvents: { ...s.artifactEvents, revision: d, seq } })
        },
        onArtifactJudgeRound: d => {
          const s = this.get(chatId)
          const seq = (s.artifactEvents?.seq ?? 0) + 1
          this.write(chatId, { ...s, artifactEvents: { ...s.artifactEvents, judgeRound: d, seq } })
        },
      }
  }

  private finishStream(chatId: string, generation: number): void {
    if (this.generations.get(chatId) !== generation) return
    const s = this.states.get(chatId)
    if (!s?.live) return
    this.write(chatId, { ...s, live: { ...s.live, streaming: false } })
    if (s.live.dag && s.live.answer !== undefined && s.live.id) void this.refetchAnswer(chatId, s.live.id)
    this.drainQueue(chatId)
  }

  // refetchAnswer re-reads a re-run turn's persisted answer: its stream carried only the re-run nodes.
  private async refetchAnswer(chatId: string, turnId: string): Promise<void> {
    let turn: Turn | null
    try {
      turn = await api.getResponse(chatId, turnId)
    } catch {
      return
    }
    const s = this.states.get(chatId)
    if (!turn || !s?.live || s.live.id !== turnId || s.live.streaming) return
    this.write(chatId, { ...s, live: { ...s.live, answer: textFromTurn(turn) } })
  }

  private bumpGeneration(chatId: string): number {
    const next = (this.generations.get(chatId) ?? 0) + 1
    this.generations.set(chatId, next)
    return next
  }

  private write(chatId: string, next: ChatState): void {
    this.states.set(chatId, next)
    this.notify(chatId)
  }

  // Coalesced to one notify per frame: a busy node emits thousands of SSE events/sec and a re-render per event
  // locks the tab. State is already current (write() is synchronous); this only throttles listeners.
  private notify(chatId: string): void {
    if (this.notifyScheduled.has(chatId)) return
    this.notifyScheduled.add(chatId)
    const flush = () => {
      this.notifyScheduled.delete(chatId)
      const set = this.listeners.get(chatId)
      if (!set) return
      for (const l of set) l()
    }
    if (typeof requestAnimationFrame === 'function') requestAnimationFrame(flush)
    else setTimeout(flush, 16)
  }
}

// Gates re-subscribing on chat open/reload. After a restart, FailStaleDagNodes fails orphaned nodes, so a dead run
// reads completed and isn't re-attached.
export function isTurnInProgress(turn: Turn | undefined): boolean {
  if (!turn) return false
  return dagFromTurn(turn)?.status === 'in_progress'
}

export function dagFromTurn(turn: Turn): DagOutputItem | undefined {
  for (const item of turn.output) {
    if (item.type === 'quack:dag') return item as DagOutputItem
  }
  return undefined
}

// Persisted DagOutputItem -> DagTurnState for DagView; runs/answers stay empty since streamed content isn't persisted.
// Shared by TurnView and attach().
export function dagTurnStateFromItem(item: DagOutputItem): DagTurnState {
  const nodeStates: DagTurnState['nodeStates'] = {}
  let startedAt: number | undefined
  let finishedAt: number | undefined
  for (const [id, ns] of Object.entries(item.node_states)) {
    nodeStates[id] = {
      status: ns.status as DagTurnState['nodeStates'][string]['status'],
      outputPreview: ns.output_preview,
      error: ns.error,
      startedAt: ns.started_at_ms,
      finishedAt: ns.finished_at_ms,
      model: ns.model,
      promptTokens: ns.prompt_tokens,
      completionTokens: ns.completion_tokens,
      totalTokens: ns.total_tokens,
      cachedTokens: ns.cached_tokens,
      finishReason: ns.finish_reason,
      serverDurationMs: ns.server_duration_ms,
      pauseReason: ns.pause_reason,
      question: ns.pending_question,
      queue: ns.queued_messages,
      traceId: ns.trace_id,
    }
    if (ns.started_at_ms != null)
      startedAt = startedAt == null ? ns.started_at_ms : Math.min(startedAt, ns.started_at_ms)
    if (ns.finished_at_ms != null)
      finishedAt = finishedAt == null ? ns.finished_at_ms : Math.max(finishedAt, ns.finished_at_ms)
  }
  return {
    planId: item.plan_id,
    nodes: item.nodes,
    edges: item.edges,
    nodeStates,
    nodeRuns: {},
    nodeAnswer: {},
    startedAt,
    finishedAt,
  }
}

// Rebuilds the orchestrator's persisted 'quack:activity' tool calls as one synthetic AgentRun, so history renders
// the same ActivityList as the live stream.
export function activityFromTurn(turn: Turn): AgentRun[] {
  for (const item of turn.output) {
    if (item.type !== 'quack:activity') continue
    const activity: AgentRun['activity'] = item.tool_calls.map(tc => ({
      kind: 'tool' as const,
      tool: {
        callId: tc.call_id,
        name: tc.name,
        args: (tc.args ?? {}) as Record<string, unknown>,
        result: tc.result,
        done: true,
      },
    }))
    return [{ runId: 'orchestrator', agent: 'orchestrator', stage: 'worker', activity, done: true }]
  }
  return []
}

// stoppedFromTurn reports the turn's answer came from a node the user stopped (server-marked).
export function stoppedFromTurn(turn: Turn): boolean {
  return turn.output.some(item => item.type === 'message' && (item as MessageOutputItem).stopped === true)
}

export function textFromTurn(turn: Turn): string {
  for (const item of turn.output) {
    if (item.type === 'message') {
      const msg = item as import('../generated').MessageOutputItem
      return msg.content
        .filter(p => p.type === 'output_text')
        .map(p => (p as import('../generated').OutputTextPart).text)
        .join('')
    }
  }
  return ''
}

// Every assistant bubble is authored: a DAG turn's answer by its terminal node's agent, a plain reply by the
// orchestrator. These helpers compute that attribution for live and persisted turns.

export interface Attribution {
  agent: string
  model?: string
  tokens?: number
  // The answering node was stopped, so its text is an unreviewed draft.
  stopped?: boolean
}

// sinkNodeIds returns the DAG's sinks - nodes with no successor - in plan order; their answers ARE
// the turn's response. Shared by DagView's topology rendering and the answer bubble.
export function sinkNodeIds(nodes: DagNodeDef[]): string[] {
  const hasSuccessor = new Set<string>()
  for (const n of nodes) for (const dep of n.depends_on ?? []) hasSuccessor.add(dep)
  return nodes.filter(n => !hasSuccessor.has(n.id)).map(n => n.id)
}

// answeringSinks are the sinks the answer is built from: with several, those that answered, were stopped,
// or ran to done here with no output (the server's "_No output._") - not earlier turns' sinks.
function answeringSinks(dag: DagTurnState): string[] {
  const ids = sinkNodeIds(dag.nodes)
  if (ids.length <= 1) return ids
  const ranEmpty = (id: string) => dag.nodeStates[id]?.status === 'done' && (dag.nodeRuns[id]?.length ?? 0) > 0
  return ids.filter(id => !!dag.nodeAnswer[id] || dag.nodeStates[id]?.status === 'cancelled' || ranEmpty(id))
}

// Mirrors stream.StoppedSinkNote: a stopped sink's draft never passed review.
const STOPPED_SINK_NOTE = '_Stopped, not reviewed._'

// dagAnswer is a DAG turn's answer as the server delivers it (stream.JoinSinkAnswers): one sink's
// text as is, else a "## id" section per sink, a stopped one masked unless every one stopped.
export function dagAnswer(dag: DagTurnState): { text: string; stopped: boolean } {
  const ids = answeringSinks(dag)
  const isStopped = (id: string) => dag.nodeStates[id]?.status === 'cancelled'
  const allStopped = ids.length > 0 && ids.every(isStopped)
  if (ids.length <= 1) return { text: ids.length ? (dag.nodeAnswer[ids[0]] ?? '') : '', stopped: allStopped }
  const body = (id: string) => (!allStopped && isStopped(id)) ? STOPPED_SINK_NOTE : ((dag.nodeAnswer[id] ?? '').trim() || '_No output._')
  return { text: ids.map(id => `## ${id}\n\n${body(id)}`).join('\n\n'), stopped: allStopped }
}

// dagTotalTokens sums total_tokens across every node in a DAG - the DAG bubble
// header's token count.
export function dagTotalTokens(dag: DagTurnState): number {
  return dag.nodes.reduce((sum, n) => sum + (dag.nodeStates[n.id]?.totalTokens ?? 0), 0)
}

// liveAnswerText is a live DAG turn's answer: its persisted answer once the run ends, when known,
// else the sinks' streamed answers.
export function liveAnswerText(live: { dag?: DagTurnState; answer?: string; streaming: boolean }): string {
  if (!live.streaming && live.answer) return live.answer
  return live.dag ? dagAnswer(live.dag).text : ''
}

// attributedSinks are the sinks an answer came from: the "## id" sections of text when it has them,
// else those that streamed, else (a reloaded turn) the one whose preview opens text, or that finished last.
function attributedSinks(dag: DagTurnState, text?: string): string[] {
  const ids = sinkNodeIds(dag.nodes)
  if (ids.length <= 1) return ids
  const sectioned = text ? ids.filter(id => text.startsWith(`## ${id}\n`) || text.includes(`\n## ${id}\n`)) : []
  if (sectioned.length > 1) return sectioned
  const answering = answeringSinks(dag)
  if (answering.length > 0) return answering
  const previewed = ids.find(id => !!text && !!dag.nodeStates[id]?.outputPreview && text.startsWith(dag.nodeStates[id]!.outputPreview!.replace(/…$/, '')))
  if (previewed) return [previewed]
  const last = ids.reduce((a, b) => ((dag.nodeStates[b]?.finishedAt ?? 0) > (dag.nodeStates[a]?.finishedAt ?? 0) ? b : a))
  return [last]
}

// dagAnswerAttribution is the answer bubble's header for a DAG turn: the answering sinks - "2 nodes"
// with their agents when several - and their own model/summed tokens (not the DAG-wide total).
export function dagAnswerAttribution(dag: DagTurnState, text?: string): Attribution | undefined {
  const ids = attributedSinks(dag, text)
  const nodes = ids.map(id => dag.nodes.find(n => n.id === id)).filter(n => n != null)
  if (nodes.length === 0) return undefined
  const states = ids.map(id => dag.nodeStates[id])
  const models = [...new Set(states.map(s => s?.model))]
  const tokens = states.some(s => s?.totalTokens != null) ? states.reduce((sum, s) => sum + (s?.totalTokens ?? 0), 0) : undefined
  const agents = [...new Set(nodes.map(n => agentLabel(n.agent)))].join(', ')
  return {
    agent: nodes.length > 1 ? `${nodes.length} nodes (${agents})` : nodes[0].agent,
    model: models.length === 1 ? models[0] : undefined,
    tokens,
    stopped: dagAnswer(dag).stopped || undefined,
  }
}

// Total input + output tokens, or undefined when unrecorded (a DAG-only turn's tokens show per node) or zero.
export function turnUsageTotal(turn: Turn): number | undefined {
  const u = turn.usage
  if (!u) return undefined
  const total = (u.input_tokens ?? 0) + (u.output_tokens ?? 0)
  return total > 0 ? total : undefined
}

// Header for a reloaded plain reply: the orchestrator with turn.model, persisted at run end (never the current config,
// which could rewrite history), and the turn's total tokens.
export function plainReplyAttribution(turn: Turn): Attribution {
  return { agent: 'orchestrator', model: turn.model, tokens: turnUsageTotal(turn) }
}

// A paused mid-node HITL question in a live DAG: the node's own question bubble, unlike the orchestrator's
// get_user_choice (pendingChoice).
export function pendingNodeQuestion(dag: DagTurnState): { nodeId: string; agent: string; question: string } | undefined {
  for (const n of dag.nodes) {
    const st = dag.nodeStates[n.id]
    if ((st?.status === 'needs_input' || (st?.status === 'paused' && st.pauseReason === 'awaiting_input')) && st.question) {
      return { nodeId: n.id, agent: n.agent, question: st.question }
    }
  }
  return undefined
}


// Sorted distinct non-empty models across node states; a DAG turn credits models per node, never on the turn.
function distinctModels(nodeStates: Record<string, { model?: string }>): string[] {
  const models = new Set<string>()
  for (const ns of Object.values(nodeStates)) {
    if (ns.model) models.add(ns.model)
  }
  return [...models].sort()
}

// The header's model chips: the latest turn's orchestrator model when set, else its DAG nodes' distinct models.
// Empty while nothing has run yet.
export function sessionModels(state: ChatState): string[] {
  if (state.live) {
    return state.live.dag ? distinctModels(state.live.dag.nodeStates) : []
  }
  const turn = state.turns[state.turns.length - 1]
  if (!turn) return []
  if (turn.model) return [turn.model]
  const dag = dagFromTurn(turn)
  return dag ? distinctModels(dag.node_states) : []
}

// cacheRate is the header's expandable-breakdown cache-hit percentage,
// undefined when there's nothing cached to report (never shown as "0%").
export function cacheRate(usage: Usage | undefined): number | undefined {
  const cached = usage?.cached_tokens ?? 0
  const input = usage?.input_tokens ?? 0
  if (cached <= 0 || input <= 0) return undefined
  return Math.round((cached / input) * 100)
}
