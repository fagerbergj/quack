import { readAgentStream } from './agentStream'
import {
  appendTextPart,
  appendThinkingPart,
  appendToolCall,
  fillToolResult,
  type MessagePart,
} from '../components/AgentParts'

export interface Message {
  role: 'user' | 'assistant'
  // User messages and seeded history carry plain content; live assistant
  // messages accumulate ordered parts (text / thinking / tool_call).
  content?: string
  parts?: MessagePart[]
}

export interface ChatTurnState {
  messages: Message[]
  streaming: boolean
  error: string
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

  async submit(chatId: string, content: string): Promise<void> {
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
    )
  }

  stop(chatId: string): void {
    this.controllers.get(chatId)?.abort()
  }

  // runStream reads the Streamable-HTTP response (a streamed, SSE-framed body)
  // and folds each labeled event into the assistant message's parts.
  private async runStream(
    chatId: string,
    fetchFn: (signal: AbortSignal) => Promise<Response>,
    foldIdx: number,
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

      let streamError = ''
      await readAgentStream(res.body, {
        onToken: t => updateParts(p => appendTextPart(p, t)),
        onThinking: t => updateParts(p => appendThinkingPart(p, t)),
        onToolCall: (name, args) => updateParts(p => appendToolCall(p, name, args)),
        onToolResult: (name, result) => updateParts(p => fillToolResult(p, name, result)),
        onError: msg => { streamError = msg },
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
