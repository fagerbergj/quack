import { readAgentStream, attachAgentEventSource, AGENT_EVENT_NAMES, type AgentStreamHandlers, type DagNodeDef, type DagEdgeDef, type NodeDoneMeta, type Stage } from './agentStream'
import {
  startRun,
  appendRunThinking,
  appendRunToolCall,
  fillRunToolResult,
  completeRun,
  freezeOpenRuns,
  type AgentRun,
} from '../components/AgentParts'
import type { Turn, DagOutputItem, NodeStatus, QueuedMessage, Usage } from '../generated'

// Re-exported so existing importers (e.g. components/DagNode.tsx) keep working
// unchanged - the generated enum is now the one source of truth for node states.
export type { NodeStatus, QueuedMessage }

export interface NodeState {
  status: NodeStatus
  outputPreview?: string
  error?: string
  // Set while status === 'needs_input': the question the node asked the user.
  // The next message sent on the chat is delivered to the node as the answer.
  question?: string
  startedAt?: number
  finishedAt?: number
  outputChars?: number
  model?: string
  promptTokens?: number
  completionTokens?: number
  reasoningTokens?: number
  totalTokens?: number
  cachedTokens?: number
  finishReason?: string
  serverDurationMs?: number
  judgeRounds?: number
  judgeFinalScore?: number
  judgePassed?: boolean
  steers?: string[]   // guidance folded in when a queued message was delivered, in order
  // Local, optimistic tracking of the node's message queue (add/edit/remove
  // responses tell the caller what changed; there's no separate SSE sync
  // event - see openapi.yaml's sendMessage description). Cleared on
  // node_steered (the queue was drained and delivered).
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
  dag?: DagTurnState
  // Top-level fields for orchestrator responses that don't go through a DAG node.
  text: string           // accumulated answer text from node-less agent_token events
  runs: AgentRun[]       // agent runs (thinking, tool calls) at the top level
  streaming: boolean
  error: string
}

// QueuedTurn is a follow-up message typed while the chat's run is still
// streaming - held client-side (no endpoint; this is a pure send deferral,
// unlike the per-node queue) and auto-submitted in order once the run ends.
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
}

type Listener = () => void

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

// turnFromLiveTurn synthesizes a persisted-shaped Turn from a finished LiveTurn,
// for when a refetch races the server's own persistence of that turn (submit()
// archiving a previous `live` before sending a follow-up). DAG turns keep only
// the terminal node's answer text - enough to render, not a full DagOutputItem.
function turnFromLiveTurn(live: LiveTurn): Turn {
  const finalId = live.dag ? terminalNodeId(live.dag.nodes) : undefined
  const text = live.dag ? (finalId != null ? (live.dag.nodeAnswer[finalId] ?? '') : live.text) : live.text
  return {
    id: live.id,
    created_at: new Date().toISOString(),
    input: { role: 'user', content: live.userText },
    output: [{ id: `${live.id}-msg`, type: 'message', status: 'completed', content: [{ type: 'output_text', text }] }],
  }
}

// Reconnect tuning for a dropped SSE stream: capped exponential backoff so a
// dead server/proxy isn't hammered, bounded so a permanently-gone server
// eventually surfaces as an error instead of retrying forever.
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
  private onTitleCallbacks = new Map<string, (title: string) => void>()  // chatID → last submit()'s onTitle, reused by drainQueue
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

  async submit(chatId: string, content: string, files?: File[], onTitle?: (title: string) => void): Promise<void> {
    const trimmed = content.trim()
    if (!trimmed) return
    let cur = this.get(chatId)
    if (cur.live?.streaming) return
    // Remembered so drainQueue's auto-submit of a later queued message can
    // still report a title change, without the caller having to re-pass it.
    if (onTitle) this.onTitleCallbacks.set(chatId, onTitle)

    // A finished previous turn still lives in `live` (finishStream only flips
    // the streaming flag). Replacing `live` would drop it from the UI, so first
    // archive it into `turns` by re-fetching from the server, where it is fully
    // persisted. Fetch BEFORE posting so the new turn's row isn't included yet.
    // Show the `submitting` indicator immediately so the spinner doesn't wait on
    // this round-trip; the old `live` stays rendered until `turns` is repopulated,
    // so the previous answer never blinks out.
    if (cur.live) {
      this.write(chatId, { ...cur, submitting: true, pendingUserText: trimmed, error: '' })
      let turns = cur.turns
      try {
        const res = await fetch(`/api/v1/chats/${chatId}`)
        if (res.ok) turns = ((await res.json()) as { turns?: Turn[] }).turns ?? turns
      } catch { /* keep local state; worst case the previous turn drops until refresh */ }
      // The refetch can race the server's own persistence of the turn that just
      // finished streaming - if it's missing from `turns`, keep it by synthesizing
      // a Turn from the in-memory `live` rather than dropping the answer.
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
        return fetch(`/api/v1/chats/${chatId}/responses`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content: trimmed }),
          signal,
        })
      },
      onTitle,
    )
  }

  // queueTurn holds a follow-up message locally while the chat's run streams
  // (drainQueue submits it once that run ends) - the top-level counterpart of
  // queueNodeMessage, but purely client-side: no endpoint, since deferring
  // store.submit needs no server involvement.
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

  // drainQueue submits the next queued follow-up once a run finishes - called
  // from finishStream so it fires whether the run ended normally, was
  // stopped, or dropped and reconnected. Submits one at a time: the auto
  // submit() call re-enters finishStream on ITS completion, draining the rest
  // in order rather than firing all queued messages at once.
  private drainQueue(chatId: string): void {
    const s = this.states.get(chatId)
    if (!s || s.queue.length === 0) return
    const [next, ...rest] = s.queue
    this.write(chatId, { ...s, queue: rest })
    void this.submit(chatId, next.text, undefined, this.onTitleCallbacks.get(chatId))
  }

  // stop cancels the chat's active run by response id (the id captured from
  // the run's opening response_created event) - a no-op if that id hasn't
  // arrived yet (the client also aborts its own connection either way).
  stop(chatId: string): void {
    const responseId = this.states.get(chatId)?.live?.id
    if (responseId) {
      fetch(`/api/v1/chats/${chatId}/responses/${responseId}/status`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status: 'cancelled' }),
      }).catch(() => {})
    }
    this.controllers.get(chatId)?.abort()
  }

  // cancelNode stops one running node; the rest of the DAG keeps going
  // (continue-but-warn). The local stream stays open.
  cancelNode(chatId: string, nodeId: string): void {
    fetch(`/api/v1/chats/${chatId}/nodes/${nodeId}/status`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ status: 'cancelled' }),
    }).catch(() => {})
  }

  // pauseNode suspends one running node at its next turn boundary, keeping its
  // accumulated work (resumable). Not optimistic, same reasoning as cancelNode.
  pauseNode(chatId: string, nodeId: string): void {
    fetch(`/api/v1/chats/${chatId}/nodes/${nodeId}/status`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ status: 'paused' }),
    }).then(async res => {
      if (res.ok) return
      const body = (await res.json().catch(() => ({}))) as { error?: string }
      this.markNodeError(chatId, nodeId, body.error || `pause rejected (HTTP ${res.status})`)
    }).catch(() => {})
  }

  // resumeNode resumes a paused node: a fresh re-run (like retry), reusing the
  // rest of the plan's stored outputs. Mirrors retryNode's optimistic local
  // reset + resubscribe.
  resumeNode(chatId: string, nodeId: string): void {
    const s = this.states.get(chatId)
    if (!s?.live?.dag || s.live.streaming) return
    const dag = s.live.dag
    const affected = retrySet(dag.edges, nodeId)
    const nodeStates = { ...dag.nodeStates }
    const nodeAnswer = { ...dag.nodeAnswer }
    const nodeRuns = { ...dag.nodeRuns }
    for (const id of affected) {
      nodeStates[id] = { status: 'queued' }
      nodeAnswer[id] = ''
      nodeRuns[id] = []
    }
    this.write(chatId, { ...s, live: { ...s.live, streaming: true, error: '', dag: { ...dag, nodeStates, nodeAnswer, nodeRuns, finishedAt: undefined } } })
    const generation = this.bumpGeneration(chatId)
    fetch(`/api/v1/chats/${chatId}/nodes/${nodeId}/status`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ status: 'running' }),
    })
      .then(res => {
        if (!res.ok) throw new Error(`resume failed: HTTP ${res.status}`)
        this.subscribeToStream(chatId, generation)
      })
      .catch((err: unknown) => {
        const cur = this.states.get(chatId)
        if (!cur?.live) return
        const msg = (err as Error)?.message || 'resume failed'
        this.write(chatId, { ...cur, error: msg, live: { ...cur.live, streaming: false } })
      })
  }

  // queueNodeMessage appends a message to a running node's queue - delivered
  // at its next turn boundary, never mid-turn (replaces the old interrupt-based
  // steer). 404s (surfaced as a node error note) if the node isn't running.
  async queueNodeMessage(chatId: string, nodeId: string, text: string): Promise<void> {
    const message = text.trim()
    if (!message) return
    const res = await fetch(`/api/v1/chats/${chatId}/nodes/${nodeId}/queue`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message }),
    }).catch(() => undefined)
    if (!res) return
    if (!res.ok) {
      const body = (await res.json().catch(() => ({}))) as { error?: string }
      this.markNodeError(chatId, nodeId, body.error || `queue rejected (HTTP ${res.status})`)
      return
    }
    const created = (await res.json()) as QueuedMessage
    this.updateNodeQueue(chatId, nodeId, q => [...q, created])
  }

  // editQueuedMessage rewrites a not-yet-delivered queued message.
  async editQueuedMessage(chatId: string, nodeId: string, messageId: string, text: string): Promise<void> {
    const message = text.trim()
    if (!message) return
    const res = await fetch(`/api/v1/chats/${chatId}/nodes/${nodeId}/queue/${messageId}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message }),
    }).catch(() => undefined)
    if (res?.ok) {
      this.updateNodeQueue(chatId, nodeId, q => q.map(m => m.id === messageId ? { ...m, text: message } : m))
    }
  }

  // removeQueuedMessage drops a not-yet-delivered queued message.
  async removeQueuedMessage(chatId: string, nodeId: string, messageId: string): Promise<void> {
    const res = await fetch(`/api/v1/chats/${chatId}/nodes/${nodeId}/queue/${messageId}`, { method: 'DELETE' }).catch(() => undefined)
    if (res?.ok) {
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

  // editNodeTask replaces a not-yet-started node's task text (only legal before
  // the node has started - 409 once it has, e.g. a downstream node waiting on
  // its dependencies). Updates the local plan def optimistically on success.
  async editNodeTask(chatId: string, nodeId: string, task: string): Promise<boolean> {
    const trimmed = task.trim()
    if (!trimmed) return false
    const res = await fetch(`/api/v1/chats/${chatId}/nodes/${nodeId}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ task: trimmed }),
    }).catch(() => undefined)
    if (!res?.ok) {
      if (res) {
        const body = (await res.json().catch(() => ({}))) as { error?: string }
        this.markNodeError(chatId, nodeId, body.error || `edit rejected (HTTP ${res.status})`)
      }
      return false
    }
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

  // retryNode re-runs a FINISHED (failed/done/cancelled) node and its
  // descendants, reusing the stored outputs of the rest. Optional guidance is
  // folded into the node's task. The PUT itself returns immediately (an
  // optimistic "queued" state); the re-run happens in the background and its
  // progress streams over the chat's GET .../stream relay - the affected
  // subgraph is reset to queued locally (answer + runs cleared) so the
  // incoming node events rebuild it in place once that subscription is live.
  retryNode(chatId: string, nodeId: string, guidance?: string): void {
    const s = this.states.get(chatId)
    if (!s?.live?.dag || s.live.streaming) return
    const dag = s.live.dag
    const affected = retrySet(dag.edges, nodeId)
    const nodeStates = { ...dag.nodeStates }
    const nodeAnswer = { ...dag.nodeAnswer }
    const nodeRuns = { ...dag.nodeRuns }
    for (const id of affected) {
      nodeStates[id] = { status: 'queued' }
      nodeAnswer[id] = ''
      nodeRuns[id] = []
    }
    this.write(chatId, { ...s, live: { ...s.live, streaming: true, error: '', dag: { ...dag, nodeStates, nodeAnswer, nodeRuns, finishedAt: undefined } } })
    const g = guidance?.trim()
    const generation = this.bumpGeneration(chatId)
    fetch(`/api/v1/chats/${chatId}/nodes/${nodeId}/status`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(g ? { status: 'queued', guidance: g } : { status: 'queued' }),
    })
      .then(res => {
        if (!res.ok) throw new Error(`retry failed: HTTP ${res.status}`)
        this.subscribeToStream(chatId, generation)
      })
      .catch((err: unknown) => {
        const cur = this.states.get(chatId)
        if (!cur?.live) return
        const msg = (err as Error)?.message || 'retry failed'
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
        // The body ended without a `done` event - the connection dropped
        // mid-run, not a normal completion. Hand off to the resumable GET
        // stream (which supports Last-Event-ID reconnect) instead of failing
        // the turn outright.
        handedOff = true
        this.resetLiveForResume(chatId)
        this.openEventSource(chatId, generation, 0, 0)
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

  // attach subscribes a client that did NOT post this run to its live stream
  // (GET /chats/{id}/stream): the same browser after a refresh, or a second
  // device. The hub replays the run so far, then tails live, through the same
  // handlers the POST path uses. Callers gate on an in-progress run; it no-ops if
  // this client is already streaming (so a run we started is never double-fed).
  attach(chatId: string): void {
    if (this.isStreaming(chatId) || this.eventSources.has(chatId)) return
    const cur = this.get(chatId)
    // The in-progress run is the latest seeded turn; lift it into `live`. Seed
    // dag/text/runs from what GET /chats/{id} already persisted for it (#463) -
    // the hub only publishes NEW events, so a client attaching after they've
    // already fired would otherwise see nothing until the run's next event.
    const last = cur.turns[cur.turns.length - 1]
    const dagItem = last ? dagFromTurn(last) : undefined
    const live: LiveTurn = {
      id: last?.id ?? '',
      userText: last?.input.content ?? '',
      streaming: true,
      error: '',
      text: last ? textFromTurn(last) : '',
      runs: last ? activityFromTurn(last) : [],
      dag: dagItem ? dagTurnStateFromItem(dagItem) : undefined,
    }
    this.write(chatId, { ...cur, turns: cur.turns.slice(0, -1), live })

    const generation = this.bumpGeneration(chatId)
    this.subscribeToStream(chatId, generation)
  }

  // subscribeToStream opens the GET .../stream EventSource and wires it through
  // the same handlers the POST path uses - shared by attach() (reconnect to an
  // in-progress run) and retryNode (watch a background re-run's progress, since
  // its PUT no longer returns its own SSE body). Callers own seeding/resetting
  // `live` beforehand; this only wires the subscription + teardown.
  private subscribeToStream(chatId: string, generation: number): void {
    this.openEventSource(chatId, generation, 0, 0)
  }

  // openEventSource opens (or, after a mid-run drop, reopens) the GET
  // .../stream EventSource. lastEventId resumes from the server's durable
  // event log (Last-Event-ID resume, M8) instead of replaying the whole run
  // again. attempt drives a capped-exponential reconnect backoff so a dead
  // server/proxy isn't hammered; it resets on every event actually received.
  //
  // The server closes the connection once the run's `done` is delivered;
  // EventSource sees that as an `error` too, so onerror must tell a genuine
  // drop (worth retrying) apart from that expected close (tear down, `done`
  // already fired) - sawDone is the flag that distinguishes them.
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
    const close = attachAgentEventSource(es, handlers)
    // Track the latest SSE id seen (EventSource populates MessageEvent.lastEventId
    // from the `id:` field) so a later reconnect resumes past it, and treat any
    // received event as proof the connection is healthy again.
    for (const name of AGENT_EVENT_NAMES) {
      es.addEventListener(name, e => {
        attempt = 0
        const id = Number((e as MessageEvent).lastEventId)
        if (Number.isFinite(id) && id > latestId) latestId = id
      })
    }
    es.onerror = () => {
      if (sawDone) return  // teardownStream already ran from onDone
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
        this.openEventSource(chatId, generation, latestId, attempt + 1)
      }, reconnectDelay(attempt))
      this.reconnectTimers.set(chatId, timer)
    }
    this.eventSources.set(chatId, close)
  }

  // resetLiveForResume clears a live turn's accumulated DAG/text/runs before
  // handing off to a full stream replay (openEventSource with lastEventId=0)
  // - the POST body carries no event ids to resume past, so the replay starts
  // from the beginning; clearing first is what keeps that idempotent instead
  // of duplicating everything already applied.
  private resetLiveForResume(chatId: string): void {
    const s = this.states.get(chatId)
    if (!s?.live) return
    this.write(chatId, { ...s, live: { ...s.live, dag: undefined, text: '', runs: [] } })
  }

  // teardownStream ends an attached run: closes its subscribe stream and flips the
  // live turn out of streaming. Used on the stream's `done` / connection close.
  // generation guards finishStream so a stale teardown can't clobber a newer run.
  private teardownStream(chatId: string, generation: number): void {
    this.detachStream(chatId)
    this.finishStream(chatId, generation)
  }

  // detachStream closes an attached subscribe stream WITHOUT ending the turn - the
  // run continues server-side. For chat switch / unmount. Re-attach re-subscribes
  // and the hub replays. ponytail: returning to a still-streaming chat shows its
  // frozen last state (attach no-ops while streaming) - reload to resume live.
  detachStream(chatId: string): void {
    const close = this.eventSources.get(chatId)
    if (close) {
      close()
      this.eventSources.delete(chatId)
    }
    this.cancelReconnect(chatId)
  }

  // streamHandlers builds the store-updating handler set shared by both transports:
  // the POST response body (runStream) and the EventSource subscribe (attach).
  private streamHandlers(
    chatId: string,
    onError: (msg: string) => void,
    onTitle?: (title: string) => void,
  ): AgentStreamHandlers {
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
        if (allDone && !dag.finishedAt) dag.finishedAt = Date.now()
        this.write(chatId, { ...s, live: { ...s.live, dag } })
      }

      const runArgs = (d: { runId: string; agent: string; stage: import('./agentStream').Stage; round?: number }) =>
        ({ runId: d.runId, agent: d.agent, stage: d.stage, round: d.round, startedAt: Date.now() })

      // ANSWER_STAGES are the stages whose streamed text IS the node's answer: the
      // initial worker draft AND each revision (which fully replaces the prior
      // draft). Judge is internal gate commentary, never the answer (as are any
      // ask_advisor consults - those are ordinary tool calls inside a worker/revise
      // run, not a separate stage, so they never reach this check at all). A node
      // can go through several worker-stage runs now (mid-node HITL re-asks, each
      // re-entering as a fresh 'worker'-stage run) - resetAnswer below clears the
      // accumulator per run so those don't concatenate together, and a revision
      // doesn't glue onto the judge-rejected draft it replaces.
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

      return {
        onAgentStart: d => {
          // A fresh top-level (no-node) run starting means any text already
          // accumulated from a PRIOR top-level run is a stale, superseded
          // attempt at the same reply - not a continuation to concatenate onto.
          // Without this, two full top-level runs against the same live turn
          // (e.g. the GitHub dispatch's no-plan-ran nudge re-driving the
          // orchestrator, #422) render the answer doubled: the first attempt's
          // text followed by the second's. Mirrors resetAnswer's per-node reset
          // below, which already does this for DAG node runs.
          if (d.nodeId) resetAnswer(d.nodeId, d.stage)
          else resetTopLevelText()
          return d.nodeId
            ? updateNodeRuns(d.nodeId, r => startRun(r, runArgs(d)))
            : updateTopLevelRuns(r => startRun(r, runArgs(d)))
        },
        onAgentThinking: (runId, text, nid) => nid
          ? updateNodeRuns(nid, r => appendRunThinking(r, runId, text))
          : updateTopLevelRuns(r => appendRunThinking(r, runId, text)),
        onAgentToolCall: (runId, callId, name, args, nid) => {
          // A tool call means everything narrated so far in this run was
          // pre-action throat-clearing, not the answer - mirrors the reset
          // internal/acp/translate.go performs backend-side (#358), applied
          // here to the LIVE stream (#387) so narration ahead of a tool call
          // never renders as if it were the final answer.
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
          // Only an answer-stage run's text belongs in the node's answer box. The
          // judge is internal gate commentary shown as its own run - without this,
          // it leaked into the answer (e.g. a failed node still displayed the
          // judge's critique as its "answer"). It still belongs in the judge's OWN
          // card though: judgePartEmitter only routes a part to agent_thinking when
          // the model marks it Thought, and local models mostly don't - so returning
          // here discarded nearly all of the judge's reasoning (#696).
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
            score: d.score, passed: d.passed, feedback: d.feedback,
            status: d.status, reason: d.reason, finishReason: d.finishReason, model: d.model, totalTokens: d.totalTokens,
          }
          if (d.nodeId) {
            updateNodeRuns(d.nodeId, r => completeRun(r, d.runId, completeArgs, Date.now()))
          } else {
            updateTopLevelRuns(r => completeRun(r, d.runId, completeArgs, Date.now()))
          }
        },
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
          // #463: when a fresh DAG arrives (e.g. after pre-DAG orchestrator
          // narration, or a new hub dispatch replaying into the same LiveTurn),
          // purge stale top-level accumulators that don't belong under this DAG.
          const nodeStates: Record<string, NodeState> = {}
          for (const n of plan.nodes) nodeStates[n.id] = { status: 'queued' }
          const dag: DagTurnState = {
            planId: plan.plan_id,
            nodes: plan.nodes,
            edges: plan.edges,
            nodeStates,
            nodeRuns: {},
            nodeAnswer: {},
            startedAt: plan.started_at_ms ?? Date.now(),
          }
          this.write(chatId, { ...s, live: { ...s.live, dag, text: '', runs: [] } })
        },
        onNodeQueued: nodeId => updateNodeState(nodeId, { status: 'queued' }),
        // Anchor timers to the server's start time (epoch ms) so a reconnect/replay
        // shows true elapsed time instead of restarting from the replay moment.
        onNodeStart: (nodeId, _agent, startedAtMs) => updateNodeState(nodeId, { status: 'running', startedAt: startedAtMs ?? Date.now() }),
        onNodeDone: (nodeId, preview, meta: NodeDoneMeta) => {
          // Freeze any run still counting - the node is done, so no run is live.
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          updateNodeState(nodeId, {
            status: 'done', finishedAt: Date.now(), outputPreview: preview,
            model: meta.model,
            promptTokens: meta.promptTokens,
            completionTokens: meta.completionTokens,
            reasoningTokens: meta.reasoningTokens,
            totalTokens: meta.totalTokens,
            cachedTokens: meta.cachedTokens,
            finishReason: meta.finishReason,
            serverDurationMs: meta.durationMs,
            judgeRounds: meta.judgeRounds,
            judgeFinalScore: meta.judgeFinalScore,
            judgePassed: meta.judgePassed,
          })
        },
        onNodeFailed: (nodeId, error) => {
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          updateNodeState(nodeId, { status: 'failed', finishedAt: Date.now(), error })
        },
        onNodeCancelled: nodeId => {
          // The node was stopped by the user - rendered neutrally ("stopped"),
          // not as a red failure (node_cancelled is a distinct event now, not
          // inferred from a node_failed error string).
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          updateNodeState(nodeId, { status: 'cancelled', finishedAt: Date.now(), error: undefined })
        },
        onNodePaused: nodeId => {
          // The node was suspended by the user - keeps its accumulated work,
          // resumable (unlike cancel). Not a terminal/finished state for the
          // allDone check below.
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          updateNodeState(nodeId, { status: 'paused', error: undefined })
        },
        onNodeNeedsInput: (nodeId, _interruptId, message) => {
          // Mid-node HITL: the node paused to ask the user. Freeze its open runs
          // and mark it waiting; the answer goes out as a normal chat message
          // (the backend routes it to the paused node).
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          updateNodeState(nodeId, { status: 'needs_input', question: message })
        },
        onNodeSteered: (nodeId, guidance) => {
          // The node was interrupted and is re-running with new guidance (same
          // session). Freeze the interrupted run, re-queue the node, and record
          // the steer; a fresh node_start → … → node_done follows on this stream.
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          const s = this.states.get(chatId)
          const prevSteers = s?.live?.dag?.nodeStates[nodeId]?.steers ?? []
          updateNodeState(nodeId, { status: 'queued', error: undefined, steers: [...prevSteers, guidance], queue: [] })
        },
      }
  }

  private finishStream(chatId: string, generation: number): void {
    if (this.generations.get(chatId) !== generation) return
    const s = this.states.get(chatId)
    if (!s?.live) return
    this.write(chatId, { ...s, live: { ...s.live, streaming: false } })
    this.drainQueue(chatId)
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

  // notify is coalesced to at most once per animation frame: a busy node can
  // emit thousands of SSE events/sec (token-level agent_thinking deltas), and
  // a React re-render per event - not per frame - is what locks the tab (#725,
  // measured: ~7s of pure re-render overhead for one node's real event volume).
  // state itself is already up to date (write() is synchronous); this only
  // throttles how often listeners are told to re-read it.
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

// isTurnInProgress reports whether a turn's DAG is still running - the gate for
// re-subscribing to a live run on chat open/reload. After a server restart,
// FailStaleDagNodes flips orphaned nodes to failed, so the DAG reads completed
// and we don't (wrongly) re-attach to a dead run.
export function isTurnInProgress(turn: Turn | undefined): boolean {
  if (!turn) return false
  return dagFromTurn(turn)?.status === 'in_progress'
}

// dagFromOutputItem extracts DagOutputItem from a Turn's output array.
export function dagFromTurn(turn: Turn): DagOutputItem | undefined {
  for (const item of turn.output) {
    if (item.type === 'quack:dag') return item as DagOutputItem
  }
  return undefined
}

// dagTurnStateFromItem converts a persisted DagOutputItem into a DagTurnState
// suitable for DagView. Runs/answers are empty (streaming content isn't persisted).
// Shared by TurnView (completed turns) and attach() (snapshotting an in-progress one).
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

// activityFromTurn reconstructs the orchestrator's persisted tool calls (the
// 'quack:activity' output item) as a single synthetic AgentRun, so chat history
// renders the same ActivityList the live stream uses. Empty array if none.
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

// textFromTurn extracts the final answer text from a completed Turn.
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

// ── answer-bubble attribution ────────────────────────────────────────────────
// Every assistant bubble is authored by someone: a DAG turn's answer is really
// produced by the terminal node's agent, a plain reply by the orchestrator
// itself. These helpers compute that attribution (agent + model + tokens) for
// both a live DagTurnState and a persisted Turn, shared by TurnView and Chat.

export interface Attribution {
  agent: string
  model?: string
  tokens?: number
}

// terminalNodeId returns the DAG's terminal node - the one with no successor,
// whose answer IS the turn's response. Shared by DagView's topology rendering
// and the answer-bubble attribution (which node actually produced this answer).
export function terminalNodeId(nodes: DagNodeDef[]): string | undefined {
  const hasSuccessor = new Set<string>()
  for (const n of nodes) for (const dep of n.depends_on ?? []) hasSuccessor.add(dep)
  return nodes.find(n => !hasSuccessor.has(n.id))?.id
}

// dagTotalTokens sums total_tokens across every node in a DAG - the DAG bubble
// header's token count.
export function dagTotalTokens(dag: DagTurnState): number {
  return dag.nodes.reduce((sum, n) => sum + (dag.nodeStates[n.id]?.totalTokens ?? 0), 0)
}

// dagAnswerAttribution is the answer bubble's header for a DAG turn: the
// terminal node's agent + that node's own model/tokens (not the DAG-wide total).
export function dagAnswerAttribution(dag: DagTurnState): Attribution | undefined {
  const id = terminalNodeId(dag.nodes)
  if (id == null) return undefined
  const node = dag.nodes.find(n => n.id === id)
  if (!node) return undefined
  const state = dag.nodeStates[id]
  return { agent: node.agent, model: state?.model, tokens: state?.totalTokens }
}

// turnUsageTotal sums a persisted Turn's usage (input + output tokens), or
// undefined when usage wasn't recorded (e.g. a DAG-only turn - its tokens are
// surfaced per-node instead) or is all-zero.
export function turnUsageTotal(turn: Turn): number | undefined {
  const u = turn.usage
  if (!u) return undefined
  const total = (u.input_tokens ?? 0) + (u.output_tokens ?? 0)
  return total > 0 ? total : undefined
}

// plainReplyAttribution is the answer bubble's header for a reloaded plain-reply
// turn: the orchestrator, with the actual model persisted on the turn row at run
// end (turn.model - never the currently-configured model, which could silently
// rewrite history) and the turn's total tokens - matching the live stream.
export function plainReplyAttribution(turn: Turn): Attribution {
  return { agent: 'orchestrator', model: turn.model, tokens: turnUsageTotal(turn) }
}

// pendingNodeQuestion finds a paused mid-node HITL question awaiting an answer
// in a live DAG - the node's own conversation-level "question bubble" (as
// opposed to the orchestrator's get_user_choice, surfaced via pendingChoice).
export function pendingNodeQuestion(dag: DagTurnState): { nodeId: string; agent: string; question: string } | undefined {
  for (const n of dag.nodes) {
    const st = dag.nodeStates[n.id]
    if (st?.status === 'needs_input' && st.question) {
      return { nodeId: n.id, agent: n.agent, question: st.question }
    }
  }
  return undefined
}

// ── chat header: model chip(s) + session usage ──────────────────────────────

// distinctModels collects the non-empty, deduplicated, sorted model names
// used across a set of node states - a DAG turn credits models per-node,
// never on the turn itself (see plainReplyAttribution for the plain-reply case).
function distinctModels(nodeStates: Record<string, { model?: string }>): string[] {
  const models = new Set<string>()
  for (const ns of Object.values(nodeStates)) {
    if (ns.model) models.add(ns.model)
  }
  return [...models].sort()
}

// sessionModels is the chat header's model chip set: the current (live, if
// streaming) or most recent turn's orchestrator model when set, else the
// distinct models its DAG nodes used. Empty while nothing has run yet.
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
