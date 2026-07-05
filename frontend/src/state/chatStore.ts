import { readAgentStream, attachAgentEventSource, type AgentStreamHandlers, type DagNodeDef, type DagEdgeDef, type NodeDoneMeta, type Stage } from './agentStream'
import {
  startRun,
  appendRunThinking,
  appendRunToolCall,
  fillRunToolResult,
  completeRun,
  freezeOpenRuns,
  type AgentRun,
} from '../components/AgentParts'
import type { Turn, DagOutputItem, NodeStatus } from '../generated'

// Re-exported so existing importers (e.g. components/DagNode.tsx) keep working
// unchanged — the generated enum is now the one source of truth for node states.
export type { NodeStatus }

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
  finishReason?: string
  serverDurationMs?: number
  judgeRounds?: number
  judgeFinalScore?: number
  judgePassed?: boolean
  steers?: string[]   // guidance applied via mid-node steering, in order
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
export interface LiveTurn {
  id: string             // turn ID (response_id) — empty string while streaming before first event
  userText: string
  dag?: DagTurnState
  // Top-level fields for orchestrator responses that don't go through a DAG node.
  text: string           // accumulated answer text from node-less agent_token events
  runs: AgentRun[]       // agent runs (thinking, tool calls) at the top level
  streaming: boolean
  error: string
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
}

type Listener = () => void

export const EMPTY_STATE: ChatState = { turns: [], error: '' }

// retrySet returns nodeId plus every node transitively downstream of it (via the
// DAG edges) — the subgraph a retry re-runs because the target's output feeds them.
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

export class ChatStore {
  private states = new Map<string, ChatState>()
  private listeners = new Map<string, Set<Listener>>()
  private controllers = new Map<string, AbortController>()
  private eventSources = new Map<string, () => void>()  // chatID → teardown for an attached subscribe stream
  private generations = new Map<string, number>()

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

  seed(chatId: string, turns: Turn[]): void {
    const cur = this.states.get(chatId)
    if (cur && (cur.live?.streaming || cur.turns.length > 0)) return
    this.write(chatId, { ...EMPTY_STATE, turns })
  }

  clear(chatId: string): void {
    this.controllers.get(chatId)?.abort()
    this.controllers.delete(chatId)
    this.eventSources.get(chatId)?.()
    this.eventSources.delete(chatId)
    this.states.delete(chatId)
    this.bumpGeneration(chatId)
    this.notify(chatId)
  }

  async submit(chatId: string, content: string, files?: File[], onTitle?: (title: string) => void): Promise<void> {
    const trimmed = content.trim()
    if (!trimmed) return
    let cur = this.get(chatId)
    if (cur.live?.streaming) return

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

  // stop cancels the chat's active run by response id (the id captured from
  // the run's opening response_created event) — a no-op if that id hasn't
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

  // steerNode interrupts one running node and re-runs it with new guidance against
  // its same session (prior tool calls/results retained). The re-run streams over
  // the SAME open connection — no abort, no re-plan.
  steerNode(chatId: string, nodeId: string, guidance: string): void {
    const g = guidance.trim()
    if (!g) return
    fetch(`/api/v1/chats/${chatId}/nodes/${nodeId}/status`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ status: 'running', guidance: g }),
    }).catch(() => {})
  }

  // retryNode re-runs a FINISHED (failed/done/cancelled) node and its
  // descendants, reusing the stored outputs of the rest. Optional guidance is
  // folded into the node's task. The PUT itself returns immediately (an
  // optimistic "queued" state); the re-run happens in the background and its
  // progress streams over the chat's GET .../stream relay — the affected
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
    try {
      const res = await fetchFn(controller.signal)
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        throw new Error((data as { error?: string }).error || `${res.status} ${res.statusText}`)
      }
      if (!res.body) throw new Error('No response body')

      let streamError = ''
      await readAgentStream(res.body, this.streamHandlers(chatId, msg => { streamError = msg }, onTitle))
      if (streamError) throw new Error(streamError)
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
      this.finishStream(chatId, generation)
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
    // The in-progress run is the latest seeded turn; lift it into `live` so the
    // replayed events rebuild its DAG there (history renders only completed turns).
    const last = cur.turns[cur.turns.length - 1]
    const live: LiveTurn = { id: last?.id ?? '', userText: last?.input.content ?? '', streaming: true, error: '', text: '', runs: [] }
    this.write(chatId, { ...cur, turns: cur.turns.slice(0, -1), live })

    const generation = this.bumpGeneration(chatId)
    this.subscribeToStream(chatId, generation)
  }

  // subscribeToStream opens the GET .../stream EventSource and wires it through
  // the same handlers the POST path uses — shared by attach() (reconnect to an
  // in-progress run) and retryNode (watch a background re-run's progress, since
  // its PUT no longer returns its own SSE body). Callers own seeding/resetting
  // `live` beforehand; this only wires the subscription + teardown.
  private subscribeToStream(chatId: string, generation: number): void {
    const es = new EventSource(`/api/v1/chats/${chatId}/stream`)
    const handlers: AgentStreamHandlers = {
      ...this.streamHandlers(chatId, msg => {
        const s = this.states.get(chatId)
        if (s) this.write(chatId, { ...s, error: msg })
      }),
      onDone: () => this.teardownStream(chatId, generation),
    }
    const close = attachAgentEventSource(es, handlers)
    // The server closes the stream once the run's `done` is delivered; EventSource
    // sees that as an error and would otherwise reconnect and re-replay the whole
    // buffer (duplicating appended tokens). Tear down instead — `done` already fired.
    es.onerror = () => this.teardownStream(chatId, generation)
    this.eventSources.set(chatId, close)
  }

  // teardownStream ends an attached run: closes its subscribe stream and flips the
  // live turn out of streaming. Used on the stream's `done` / connection close.
  // generation guards finishStream so a stale teardown can't clobber a newer run.
  private teardownStream(chatId: string, generation: number): void {
    this.detachStream(chatId)
    this.finishStream(chatId, generation)
  }

  // detachStream closes an attached subscribe stream WITHOUT ending the turn — the
  // run continues server-side. For chat switch / unmount. Re-attach re-subscribes
  // and the hub replays. ponytail: returning to a still-streaming chat shows its
  // frozen last state (attach no-ops while streaming) — reload to resume live.
  detachStream(chatId: string): void {
    const close = this.eventSources.get(chatId)
    if (close) {
      close()
      this.eventSources.delete(chatId)
    }
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
      // draft). Advisor/judge/self-refine are internal gate commentary, never the
      // answer. A node can go through several worker-stage runs now (mid-node HITL
      // re-asks, each re-entering as a fresh 'worker'-stage run) — resetAnswer below
      // clears the accumulator per run so those don't concatenate together, and a
      // revision doesn't glue onto the judge-rejected draft it replaces.
      const ANSWER_STAGES: ReadonlySet<Stage> = new Set(['worker', 'revise'])
      const resetAnswer = (nodeId: string | undefined, stage: Stage) => {
        if (!nodeId || !ANSWER_STAGES.has(stage)) return
        const s = this.states.get(chatId)
        if (!s?.live?.dag) return
        this.write(chatId, { ...s, live: { ...s.live, dag: { ...s.live.dag, nodeAnswer: { ...s.live.dag.nodeAnswer, [nodeId]: '' } } } })
      }

      return {
        onAgentStart: d => {
          resetAnswer(d.nodeId, d.stage)
          return d.nodeId
            ? updateNodeRuns(d.nodeId, r => startRun(r, runArgs(d)))
            : updateTopLevelRuns(r => startRun(r, runArgs(d)))
        },
        onAgentThinking: (runId, text, nid) => nid
          ? updateNodeRuns(nid, r => appendRunThinking(r, runId, text))
          : updateTopLevelRuns(r => appendRunThinking(r, runId, text)),
        onAgentToolCall: (runId, callId, name, args, nid) => nid
          ? updateNodeRuns(nid, r => appendRunToolCall(r, runId, callId, name, args))
          : updateTopLevelRuns(r => appendRunToolCall(r, runId, callId, name, args)),
        onAgentToolResult: (runId, callId, name, result, nid) => nid
          ? updateNodeRuns(nid, r => fillRunToolResult(r, runId, callId, name, result))
          : updateTopLevelRuns(r => fillRunToolResult(r, runId, callId, name, result)),
        onAgentToken: (runId, text, nid) => {
          if (!nid) { updateTopLevelText(text); return }
          // Only an answer-stage run's text belongs in the node's answer box. The
          // advisor consult and the judge are internal gate commentary shown as
          // their own runs — without this, they leaked into the answer (e.g. a
          // failed node still displayed the advisor's critique as its "answer").
          const st = this.states.get(chatId)
          const run = st?.live?.dag?.nodeRuns?.[nid]?.find(r => r.runId === runId)
          if (run && !ANSWER_STAGES.has(run.stage)) return
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
          this.write(chatId, { ...s, live: { ...s.live, dag } })
        },
        onNodeQueued: nodeId => updateNodeState(nodeId, { status: 'queued' }),
        // Anchor timers to the server's start time (epoch ms) so a reconnect/replay
        // shows true elapsed time instead of restarting from the replay moment.
        onNodeStart: (nodeId, _agent, startedAtMs) => updateNodeState(nodeId, { status: 'running', startedAt: startedAtMs ?? Date.now() }),
        onNodeDone: (nodeId, preview, meta: NodeDoneMeta) => {
          // Freeze any run still counting — the node is done, so no run is live.
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          updateNodeState(nodeId, {
            status: 'done', finishedAt: Date.now(), outputPreview: preview,
            model: meta.model,
            promptTokens: meta.promptTokens,
            completionTokens: meta.completionTokens,
            reasoningTokens: meta.reasoningTokens,
            totalTokens: meta.totalTokens,
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
          // The node was stopped by the user — rendered neutrally ("stopped"),
          // not as a red failure (node_cancelled is a distinct event now, not
          // inferred from a node_failed error string).
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          updateNodeState(nodeId, { status: 'cancelled', finishedAt: Date.now(), error: undefined })
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
          updateNodeState(nodeId, { status: 'queued', error: undefined, steers: [...prevSteers, guidance] })
        },
      }
  }

  private finishStream(chatId: string, generation: number): void {
    if (this.generations.get(chatId) !== generation) return
    const s = this.states.get(chatId)
    if (!s?.live) return
    this.write(chatId, { ...s, live: { ...s.live, streaming: false } })
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

  private notify(chatId: string): void {
    const set = this.listeners.get(chatId)
    if (!set) return
    for (const l of set) l()
  }
}

// isTurnInProgress reports whether a turn's DAG is still running — the gate for
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
