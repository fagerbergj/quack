import { readAgentStream, type DagNodeDef, type DagEdgeDef } from './agentStream'
import {
  appendTextPart,
  appendThinkingPart,
  appendToolCall,
  fillToolResult,
  openAgent,
  closeAgent,
  openSelfRefine,
  closeSelfRefine,
  openJudgeVerdict,
  closeJudgeVerdict,
  appendRevise,
  appendJudgeUnavailable,
  type MessagePart,
} from '../components/AgentParts'

export interface Message {
  role: 'user' | 'assistant'
  // User messages and seeded history carry plain content; live assistant
  // messages accumulate ordered parts (text / thinking / tool_call).
  content?: string
  parts?: MessagePart[]
}

export type NodeStatus = 'queued' | 'running' | 'done' | 'failed'

export interface NodeState {
  status: NodeStatus
  outputPreview?: string
  error?: string
  startedAt?: number  // Date.now() when node_start received
  finishedAt?: number // Date.now() when node_done/node_failed received
  outputChars?: number // accumulated output character count (proxy for tokens)
}

export interface DagTurnState {
  planId: string
  nodes: DagNodeDef[]
  edges: DagEdgeDef[]
  nodeStates: Record<string, NodeState>
  nodeParts: Record<string, MessagePart[]>
  startedAt?: number  // Date.now() when dag_plan received
  finishedAt?: number // Date.now() when last node finishes
}

export interface ChatTurnState {
  messages: Message[]
  streaming: boolean
  error: string
  dag?: DagTurnState
}

type Listener = () => void

export const EMPTY_TURN: ChatTurnState = { messages: [], streaming: false, error: '' }

export class ChatStore {
  private states = new Map<string, ChatTurnState>()
  private listeners = new Map<string, Set<Listener>>()
  private controllers = new Map<string, AbortController>()
  private generations = new Map<string, number>()

  get(chatId: string): ChatTurnState {
    return this.states.get(chatId) ?? EMPTY_TURN
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

  seed(chatId: string, messages: Message[]): void {
    const cur = this.states.get(chatId)
    if (cur && (cur.streaming || cur.messages.length > 0)) return
    this.write(chatId, { ...EMPTY_TURN, messages })
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
    const cur = this.get(chatId)
    if (cur.streaming) return

    const userMessage: Message = { role: 'user', content: trimmed }
    const assistantMessage: Message = { role: 'assistant', parts: [] }
    const assistantIdx = cur.messages.length + 1
    this.write(chatId, {
      messages: [...cur.messages, userMessage, assistantMessage],
      streaming: true,
      error: '',
    })

    await this.runStream(
      chatId,
      signal => fetch(`/api/v1/chats/${chatId}/messages`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ content: trimmed }),
        signal,
      }),
      assistantIdx,
      onTitle,
    )
  }

  stop(chatId: string): void {
    // Cancel the server-side run first so inference stops, then drop the connection.
    fetch(`/api/v1/chats/${chatId}/stream`, { method: 'DELETE' }).catch(() => {})
    this.controllers.get(chatId)?.abort()
  }

  // runStream reads the Streamable-HTTP response (a streamed, SSE-framed body)
  // and folds each labeled event into the assistant message's parts.
  private async runStream(
    chatId: string,
    fetchFn: (signal: AbortSignal) => Promise<Response>,
    foldIdx: number,
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

      const updateParts = (fn: (parts: MessagePart[]) => MessagePart[]) => {
        const s = this.states.get(chatId)
        if (!s) return
        this.write(chatId, {
          ...s,
          messages: s.messages.map((m, i) =>
            i === foldIdx ? { ...m, parts: fn(m.parts ?? []) } : m,
          ),
        })
      }

      const updateNodeParts = (nodeId: string, fn: (parts: MessagePart[]) => MessagePart[], extraChars?: number) => {
        const s = this.states.get(chatId)
        if (!s?.dag) return
        const prev = s.dag.nodeParts[nodeId] ?? []
        const dag = { ...s.dag, nodeParts: { ...s.dag.nodeParts, [nodeId]: fn(prev) } }
        if (extraChars) {
          const ns = dag.nodeStates[nodeId] ?? { status: 'queued' as NodeStatus }
          dag.nodeStates = { ...dag.nodeStates, [nodeId]: { ...ns, outputChars: (ns.outputChars ?? 0) + extraChars } }
        }
        this.write(chatId, { ...s, dag })
      }

      const updateNodeState = (nodeId: string, patch: Partial<NodeState>) => {
        const s = this.states.get(chatId)
        if (!s?.dag) return
        const prev = s.dag.nodeStates[nodeId] ?? { status: 'queued' as NodeStatus }
        const dag = { ...s.dag, nodeStates: { ...s.dag.nodeStates, [nodeId]: { ...prev, ...patch } } }
        // Track DAG finishedAt when all nodes are done/failed
        const allDone = dag.nodes.every(n => {
          const st = dag.nodeStates[n.id]?.status
          return st === 'done' || st === 'failed'
        })
        if (allDone && !dag.finishedAt) dag.finishedAt = Date.now()
        this.write(chatId, { ...s, dag })
      }

      // Route activity event to a node's parts list (if nodeId) or top-level parts.
      const route = (nodeId: string | undefined, fn: (parts: MessagePart[]) => MessagePart[]) => {
        if (nodeId) {
          updateNodeParts(nodeId, fn)
        } else {
          updateParts(fn)
        }
      }

      let streamError = ''
      await readAgentStream(res.body, {
        onToken: (t, nid) => {
          if (nid) updateNodeParts(nid, p => appendTextPart(p, t), t.length)
          else updateParts(p => appendTextPart(p, t))
        },
        onThinking: (t, nid) => route(nid, p => appendThinkingPart(p, t)),
        onToolCall: (name, args, nid) => route(nid, p => appendToolCall(p, name, args)),
        onToolResult: (name, result, nid) => route(nid, p => fillToolResult(p, name, result)),
        onAgentStart: (agent, nid) => route(nid, p => openAgent(p, agent)),
        onAgentEnd: (agent, nid) => route(nid, p => closeAgent(p, agent)),
        onSelfRefineStart: nid => route(nid, p => openSelfRefine(p, Date.now())),
        onSelfRefine: (changed, nid) => route(nid, p => closeSelfRefine(p, changed, Date.now())),
        onJudgeStart: (round, nid) => route(nid, p => openJudgeVerdict(p, round, Date.now())),
        onRevise: (round, nid) => route(nid, p => appendRevise(p, round)),
        onJudgeVerdict: (v, nid) => route(nid, p => closeJudgeVerdict(p, v.round, v.score, v.passed, v.feedback, Date.now())),
        onJudgeUnavailable: (round, reason, nid) => route(nid, p => appendJudgeUnavailable(p, round, reason)),
        onChatTitle: title => onTitle?.(title),
        onError: msg => { streamError = msg },
        // DAG lifecycle handlers
        onDagPlan: plan => {
          const s = this.states.get(chatId)
          if (!s) return
          const nodeStates: Record<string, NodeState> = {}
          for (const n of plan.nodes) nodeStates[n.id] = { status: 'queued' }
          this.write(chatId, {
            ...s,
            dag: { planId: plan.plan_id, nodes: plan.nodes, edges: plan.edges, nodeStates, nodeParts: {}, startedAt: Date.now() },
          })
        },
        onNodeQueued: nodeId => updateNodeState(nodeId, { status: 'queued' }),
        onNodeStart: (nodeId) => updateNodeState(nodeId, { status: 'running', startedAt: Date.now() }),
        onNodeDone: (nodeId, preview) => updateNodeState(nodeId, { status: 'done', finishedAt: Date.now(), outputPreview: preview }),
        onNodeFailed: (nodeId, error) => updateNodeState(nodeId, { status: 'failed', finishedAt: Date.now(), error }),
      })
      if (streamError) throw new Error(streamError)
    } catch (err: unknown) {
      this.handleStreamError(chatId, err, foldIdx)
    } finally {
      if (this.controllers.get(chatId) === controller) {
        this.controllers.delete(chatId)
      }
      this.finishStream(chatId, generation)
    }
  }

  private handleStreamError(chatId: string, err: unknown, assistantIdx: number): void {
    if ((err as Error)?.name === 'AbortError') return
    const msg = (err as Error)?.message || 'Request failed'
    const s = this.states.get(chatId)
    if (!s) return
    let messages = s.messages
    const last = messages[assistantIdx]
    const empty = last?.role === 'assistant' && !(last.parts?.length) && !last.content
    if (empty) {
      messages = messages.slice(0, assistantIdx)
    }
    this.write(chatId, { ...s, messages, error: msg })
  }

  private finishStream(chatId: string, generation: number): void {
    if (this.generations.get(chatId) !== generation) return
    const s = this.states.get(chatId)
    if (!s) return
    this.write(chatId, { ...s, streaming: false })
  }

  private bumpGeneration(chatId: string): number {
    const next = (this.generations.get(chatId) ?? 0) + 1
    this.generations.set(chatId, next)
    return next
  }

  private write(chatId: string, next: ChatTurnState): void {
    this.states.set(chatId, next)
    this.notify(chatId)
  }

  private notify(chatId: string): void {
    const set = this.listeners.get(chatId)
    if (!set) return
    for (const l of set) l()
  }
}
