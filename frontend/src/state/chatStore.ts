import { readAgentStream, type DagNodeDef, type DagEdgeDef, type NodeDoneMeta } from './agentStream'
import {
  startRun,
  appendRunThinking,
  appendRunToolCall,
  fillRunToolResult,
  completeRun,
  freezeOpenRuns,
  type AgentRun,
} from '../components/AgentParts'
import type { Turn, DagOutputItem } from '../generated'

export type NodeStatus = 'queued' | 'running' | 'done' | 'failed'

export interface NodeState {
  status: NodeStatus
  outputPreview?: string
  error?: string
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
  selfRefined?: boolean
  judgeRounds?: number
  judgeFinalScore?: number
  judgePassed?: boolean
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
  streaming: boolean
  error: string
}

export interface ChatState {
  // Completed turns from history (seeded from GET /chats/{id})
  turns: Turn[]
  // The turn currently streaming (or most recently completed, until next submit)
  live?: LiveTurn
  error: string
}

type Listener = () => void

export const EMPTY_STATE: ChatState = { turns: [], error: '' }

export class ChatStore {
  private states = new Map<string, ChatState>()
  private listeners = new Map<string, Set<Listener>>()
  private controllers = new Map<string, AbortController>()
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
    this.states.delete(chatId)
    this.bumpGeneration(chatId)
    this.notify(chatId)
  }

  async submit(chatId: string, content: string, onTitle?: (title: string) => void): Promise<void> {
    const trimmed = content.trim()
    if (!trimmed) return
    let cur = this.get(chatId)
    if (cur.live?.streaming) return

    // A finished previous turn still lives in `live` (finishStream only flips
    // the streaming flag). Replacing `live` would drop it from the UI, so first
    // archive it into `turns` by re-fetching from the server, where it is fully
    // persisted. Fetch BEFORE posting so the new turn's row isn't included yet.
    if (cur.live) {
      try {
        const res = await fetch(`/api/v1/chats/${chatId}`)
        if (res.ok) {
          const detail = (await res.json()) as { turns?: Turn[] }
          const s = this.get(chatId)
          if (!s.live?.streaming) this.write(chatId, { ...s, turns: detail.turns ?? s.turns })
        }
      } catch { /* keep local state; worst case the previous turn drops until refresh */ }
      cur = this.get(chatId)
    }

    const live: LiveTurn = { id: '', userText: trimmed, streaming: true, error: '' }
    this.write(chatId, { ...cur, live, error: '' })

    await this.runStream(
      chatId,
      signal => fetch(`/api/v1/chats/${chatId}/responses`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ content: trimmed }),
        signal,
      }),
      onTitle,
    )
  }

  stop(chatId: string): void {
    fetch(`/api/v1/chats/${chatId}/stream`, { method: 'DELETE' }).catch(() => {})
    this.controllers.get(chatId)?.abort()
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

      const updateNodeRuns = (nodeId: string | undefined, fn: (runs: AgentRun[]) => AgentRun[]) => {
        if (!nodeId) return
        const s = this.states.get(chatId)
        if (!s?.live?.dag) return
        const prev = s.live.dag.nodeRuns[nodeId] ?? []
        const dag = { ...s.live.dag, nodeRuns: { ...s.live.dag.nodeRuns, [nodeId]: fn(prev) } }
        this.write(chatId, { ...s, live: { ...s.live, dag } })
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

      const updateNodeState = (nodeId: string, patch: Partial<NodeState>) => {
        const s = this.states.get(chatId)
        if (!s?.live?.dag) return
        const prev = s.live.dag.nodeStates[nodeId] ?? { status: 'queued' as NodeStatus }
        const dag = { ...s.live.dag, nodeStates: { ...s.live.dag.nodeStates, [nodeId]: { ...prev, ...patch } } }
        const allDone = dag.nodes.every(n => {
          const st = dag.nodeStates[n.id]?.status
          return st === 'done' || st === 'failed'
        })
        if (allDone && !dag.finishedAt) dag.finishedAt = Date.now()
        this.write(chatId, { ...s, live: { ...s.live, dag } })
      }

      let streamError = ''
      await readAgentStream(res.body, {
        onAgentStart: d => updateNodeRuns(d.nodeId, r => startRun(r, { runId: d.runId, agent: d.agent, stage: d.stage, round: d.round, startedAt: Date.now() })),
        onAgentThinking: (runId, text, nid) => updateNodeRuns(nid, r => appendRunThinking(r, runId, text)),
        onAgentToolCall: (runId, callId, name, args, nid) => updateNodeRuns(nid, r => appendRunToolCall(r, runId, callId, name, args)),
        onAgentToolResult: (runId, callId, name, result, nid) => updateNodeRuns(nid, r => fillRunToolResult(r, runId, callId, name, result)),
        onAgentToken: (_runId, text, nid) => updateNodeAnswer(nid, text),
        onAgentComplete: d => updateNodeRuns(d.nodeId, r => completeRun(r, d.runId, {
          changed: d.changed, score: d.score, passed: d.passed, feedback: d.feedback,
          status: d.status, reason: d.reason, finishReason: d.finishReason, model: d.model, totalTokens: d.totalTokens,
        }, Date.now())),
        onChatTitle: title => onTitle?.(title),
        onError: msg => { streamError = msg },
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
            startedAt: Date.now(),
          }
          this.write(chatId, { ...s, live: { ...s.live, dag } })
        },
        onNodeQueued: nodeId => updateNodeState(nodeId, { status: 'queued' }),
        onNodeStart: nodeId => updateNodeState(nodeId, { status: 'running', startedAt: Date.now() }),
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
            selfRefined: meta.selfRefined,
            judgeRounds: meta.judgeRounds,
            judgeFinalScore: meta.judgeFinalScore,
            judgePassed: meta.judgePassed,
          })
        },
        onNodeFailed: (nodeId, error) => {
          updateNodeRuns(nodeId, r => freezeOpenRuns(r, Date.now()))
          updateNodeState(nodeId, { status: 'failed', finishedAt: Date.now(), error })
        },
      })
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

// dagFromOutputItem extracts DagOutputItem from a Turn's output array.
export function dagFromTurn(turn: Turn): DagOutputItem | undefined {
  for (const item of turn.output) {
    if (item.type === 'quack:dag') return item as DagOutputItem
  }
  return undefined
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
