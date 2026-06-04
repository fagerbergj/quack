import { api } from '../api'
import {
  appendConfirmation,
  appendTextPart,
  appendThinkingPart,
  appendToolCall,
  fillToolResult,
  markConfirmation,
  partsToText,
  type MessagePart,
} from '../components/AgentParts'
import { readAgentStream } from './agentStream'

// Message is the chat-page-level view of a chat message — same shape as the
// component used to manage locally before the stream state was hoisted into
// the store. Kept here so Chat.tsx can stay free of stream wiring.
export interface Message {
  role: 'user' | 'assistant'
  content: string
  parts?: MessagePart[]
}

// ChatTurnState is the public snapshot a component renders for a chatId.
// The AbortController and per-stream generation token are kept off this type
// in a parallel map so components only see pure data.
export interface ChatTurnState {
  messages: Message[]
  streaming: boolean
  error: string
}

type Listener = () => void

export const EMPTY_TURN: ChatTurnState = { messages: [], streaming: false, error: '' }

// ChatStore owns the per-chat stream state and the SSE read loop. Created
// once at app startup and provided via React context. Subscribers are
// scoped to a chatId so a token tick on chat A does not re-render a panel
// rendering chat B.
//
// In-memory only: state survives navigation within the SPA but a hard
// refresh / tab close starts from the server-persisted chat history (via
// the seed() entry point) and loses any in-flight stream. Persistent
// resume across refresh would require server-side persistence of tool
// calls and pending confirmations — explicit non-goal.
export class ChatStore {
  private states = new Map<string, ChatTurnState>()
  private listeners = new Map<string, Set<Listener>>()
  // Side-channel for the in-flight AbortController per chat — kept off the
  // public snapshot so subscribers see pure data.
  private controllers = new Map<string, AbortController>()
  // Generation counter incremented on every new turn / on clear(). consume()
  // captures its starting generation and bails out on each event if the
  // store has moved on (e.g. the chat was cleared mid-stream). Prevents a
  // late-arriving fold from resurrecting a deleted chat.
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

  /**
   * seed loads server-persisted messages into the store for the first
   * navigation to a chat. No-op if there's already a live stream OR if any
   * messages are already loaded — we never clobber live state with server
   * history, which is what makes background streams survive nav.
   */
  seed(chatId: string, messages: Message[]): void {
    const cur = this.states.get(chatId)
    if (cur && (cur.streaming || cur.messages.length > 0)) return
    this.write(chatId, { ...EMPTY_TURN, messages })
  }

  /** Removes a chat's state entirely (used when the chat is deleted). */
  clear(chatId: string): void {
    this.controllers.get(chatId)?.abort()
    this.controllers.delete(chatId)
    this.states.delete(chatId)
    this.bumpGeneration(chatId)
    this.notify(chatId)
  }

  /**
   * submit appends a user message and starts a streamed assistant reply.
   * Resolves when the stream ends (normally, via error, or via abort) —
   * the store is the source of truth; UI reads state via subscribe.
   */
  async submit(chatId: string, content: string): Promise<void> {
    const trimmed = content.trim()
    if (!trimmed) return
    const cur = this.get(chatId)
    if (cur.streaming) return

    const userMessage: Message = { role: 'user', content: trimmed }
    const assistantMessage: Message = { role: 'assistant', content: '', parts: [] }
    const assistantIdx = cur.messages.length + 1
    this.write(chatId, {
      messages: [...cur.messages, userMessage, assistantMessage],
      streaming: true,
      error: '',
    })

    await this.runStream(
      chatId,
      signal => api.sendMessage(chatId, trimmed, signal),
      assistantIdx,
      /*dropAssistantOnError=*/true,
    )
  }

  /**
   * decide approves or rejects a pending tool confirmation. On approve, the
   * server returns an SSE continuation that gets folded into the same
   * assistant message. On reject, no continuation is consumed.
   */
  async decide(chatId: string, callId: string, confirmed: boolean, content?: string): Promise<void> {
    const cur = this.get(chatId)
    const hostIdx = cur.messages.findIndex(m =>
      m.role === 'assistant' && m.parts?.some(p => p.kind === 'confirmation' && p.callId === callId)
    )
    if (hostIdx < 0) return

    this.write(chatId, {
      messages: cur.messages.map(m =>
        m.parts ? { ...m, parts: markConfirmation(m.parts, callId, confirmed ? 'approved' : 'rejected') } : m
      ),
      streaming: true,
      error: '',
    })

    await this.runStream(
      chatId,
      signal => api.decideConfirmation(chatId, callId, { confirmed, content }, signal),
      hostIdx,
      /*dropAssistantOnError=*/false,
      // Reject path: server returns a single done event with no folded
      // content. Drain the body but skip the SSE consume loop.
      /*foldBody=*/confirmed,
    )
  }

  /** Aborts the in-flight stream for this chat. State is preserved. */
  stop(chatId: string): void {
    this.controllers.get(chatId)?.abort()
  }

  // ── internals ──────────────────────────────────────────────────────────────

  /**
   * runStream is the shared shape of submit + decide: create an
   * AbortController, fire the fetch, read the SSE body into the message at
   * `foldIdx`, and finalize streaming=false on the chat. Errors that aren't
   * AbortErrors land in `state.error`.
   *
   * Set foldBody=false when the response is expected to be a single done
   * frame with nothing to fold (e.g. the reject branch of `decide`); the
   * body is drained but the SSE parser is skipped.
   */
  private async runStream(
    chatId: string,
    fetchFn: (signal: AbortSignal) => Promise<Response>,
    foldIdx: number,
    dropAssistantOnError: boolean,
    foldBody: boolean = true,
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
      if (!foldBody) {
        await res.body?.cancel().catch(() => {})
        return
      }
      await this.consumeStream(chatId, generation, res.body!, foldIdx)
    } catch (err: unknown) {
      this.handleStreamError(chatId, err, foldIdx, dropAssistantOnError)
    } finally {
      if (this.controllers.get(chatId) === controller) {
        this.controllers.delete(chatId)
      }
      this.finishStream(chatId, generation)
    }
  }

  private async consumeStream(
    chatId: string,
    generation: number,
    body: ReadableStream<Uint8Array>,
    foldIdx: number,
  ): Promise<void> {
    const updateMessage = (mut: (parts: MessagePart[]) => MessagePart[]) => {
      // Bail if the chat was cleared or another turn started since we began
      // — prevents a late event from resurrecting deleted state.
      if (this.generations.get(chatId) !== generation) return
      const s = this.states.get(chatId)
      if (!s) return
      this.write(chatId, {
        ...s,
        messages: s.messages.map((m, i) =>
          i === foldIdx ? withParts(m, mut(m.parts ?? [])) : m
        ),
      })
    }

    await readAgentStream(body, {
      onToken: text => updateMessage(parts => appendTextPart(parts, text)),
      onThinking: text => updateMessage(parts => appendThinkingPart(parts, text)),
      onToolCall: (name, args) => updateMessage(parts => appendToolCall(parts, name, args)),
      onToolResult: (name, result) => updateMessage(parts => fillToolResult(parts, name, result)),
      onConfirmationRequest: req => updateMessage(parts => appendConfirmation(parts, {
        callId: req.callId,
        toolName: req.toolName,
        hint: req.hint,
        field: (req.payload.field as string) ?? '',
        stage: (req.payload.stage as string) ?? '',
        before: (req.payload.before as string) ?? '',
        after: (req.payload.after as string) ?? '',
      })),
      onError: msg => {
        if (this.generations.get(chatId) !== generation) return
        const s = this.states.get(chatId)
        if (!s) return
        this.write(chatId, { ...s, error: msg })
      },
    })
  }

  private handleStreamError(chatId: string, err: unknown, assistantIdx: number, dropAssistant: boolean): void {
    // AbortError fires on explicit stop() or store clear() — both are user
    // intent, not a failure to surface.
    if ((err as Error)?.name === 'AbortError') return
    const msg = (err as Error)?.message || 'Request failed'
    const s = this.states.get(chatId)
    if (!s) return
    let messages = s.messages
    if (dropAssistant) {
      const last = messages[assistantIdx]
      if (last?.role === 'assistant' && !last.content) {
        messages = messages.slice(0, assistantIdx)
      }
    }
    this.write(chatId, { ...s, messages, error: msg })
  }

  // finishStream flips streaming=false, but only if this stream is still the
  // active one for the chat (compare generation). If clear() ran between
  // start and finish, we don't want to resurrect cleared state.
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

// Mirrors message.content with the plain-text projection of its text parts
// so copy/download buttons keep working without re-deriving on every render.
function withParts(m: Message, parts: MessagePart[]): Message {
  return { ...m, parts, content: partsToText(parts) }
}
