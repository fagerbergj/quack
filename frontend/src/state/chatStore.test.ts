import { describe, it, expect, vi, beforeEach } from 'vitest'
import { activityFromTurn, isTurnInProgress, ChatStore } from './chatStore'
import { pendingChoice } from '../components/messageParts'
import type { Turn } from '../generated'

function turnWith(...output: Turn['output']): Turn {
  return { id: 't1', created_at: '', input: { role: 'user', content: 'hi' }, output }
}

function dagTurn(status: 'in_progress' | 'completed'): Turn {
  return turnWith({
    type: 'quack:dag', id: 'p', status, plan_id: 'p',
    nodes: [{ id: 'a', agent: 'researcher', task: 't', depends_on: [] }],
    edges: [], node_states: {},
  })
}

describe('activityFromTurn', () => {
  it('reconstructs a synthetic orchestrator run from a quack:activity item', () => {
    const turn = turnWith({
      type: 'quack:activity', id: 't1:activity', status: 'completed',
      tool_calls: [
        { call_id: 'c1', name: 'web_search', args: { query: 'x' }, result: { hits: 2 } },
        { call_id: 'c2', name: 'get_user_choice', args: { options: ['A', 'B'] }, result: { status: 'pending' } },
      ],
    })
    const runs = activityFromTurn(turn)
    expect(runs).toHaveLength(1)
    expect(runs[0].runId).toBe('orchestrator')
    expect(runs[0].activity).toHaveLength(2)
    expect(runs[0].activity[0]).toEqual({
      kind: 'tool',
      tool: { callId: 'c1', name: 'web_search', args: { query: 'x' }, result: { hits: 2 }, done: true },
    })
    // The reconstructed run feeds pendingChoice the same way the live path does.
    expect(pendingChoice(runs)).toEqual({ callId: 'c2', question: '', options: ['A', 'B'] })
  })

  it('returns [] when the turn has no activity item', () => {
    expect(activityFromTurn(turnWith({ type: 'message', id: 'm', status: 'completed', content: [] }))).toEqual([])
  })
})

// Minimal SSE stream that closes immediately so the store doesn't hang.
function makeStream(body: string): Response {
  const encoder = new TextEncoder()
  const stream = new ReadableStream({
    start(ctrl) {
      ctrl.enqueue(encoder.encode(body))
      ctrl.close()
    },
  })
  return new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

describe('ChatStore.submit — loading indicator gap (regression)', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  let store: ChatStore

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    store = new ChatStore()
    store.seed('chat-1', [])
  })

  // On a follow-up message the previous finished turn lingers in `live` and must be
  // archived via a GET round-trip. The `submitting` indicator must appear BEFORE that
  // GET resolves — otherwise the spinner doesn't show until the first token.
  it('sets submitting/pendingUserText before the archive GET resolves', async () => {
    // First turn — complete it so a finished `live` lingers (the archive trigger).
    fetchMock.mockResolvedValueOnce(makeStream(''))
    await store.submit('chat-1', 'msg1')
    expect(store.get('chat-1').live?.streaming).toBe(false)

    // Second turn — make the archive GET hang so we can observe state mid-flight.
    let resolveArchive!: (r: Response) => void
    const archive = new Promise<Response>(res => { resolveArchive = res })
    fetchMock.mockReturnValueOnce(archive)            // GET /api/v1/chats/chat-1 (archive)
    fetchMock.mockResolvedValueOnce(makeStream(''))   // POST /responses (msg2)

    const p = store.submit('chat-1', 'msg2')

    // Synchronously after submit, before the GET resolves: the indicator is up.
    expect(store.get('chat-1').submitting).toBe(true)
    expect(store.get('chat-1').pendingUserText).toBe('msg2')

    resolveArchive(new Response(JSON.stringify({ turns: [] }), { status: 200 }))
    await p

    // Once streaming starts the indicator clears and the live turn carries the text.
    expect(store.get('chat-1').submitting).toBe(false)
    expect(store.get('chat-1').pendingUserText).toBeUndefined()
    expect(store.get('chat-1').live?.userText).toBe('msg2')
  })

  // First message of a chat has no previous `live`, so no archive GET and no
  // `submitting` phase — the live turn is created immediately.
  it('creates the live turn immediately on the first message (no archive)', async () => {
    fetchMock.mockResolvedValueOnce(makeStream(''))
    const p = store.submit('chat-1', 'hello')
    expect(store.get('chat-1').live?.userText).toBe('hello')
    expect(store.get('chat-1').submitting).toBeFalsy()
    await p
  })
})

describe('ChatStore — mid-node steering', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  let store: ChatStore

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    store = new ChatStore()
    store.seed('c', [])
  })

  it('node_steered re-queues the node and records the guidance', async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: node_start',
      'data: {"node_id":"a","agent":"researcher"}',
      '',
      'event: node_steered',
      'data: {"node_id":"a","guidance":"focus on cost"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    const ns = store.get('c').live?.dag?.nodeStates['a']
    expect(ns?.status).toBe('queued')
    expect(ns?.steers).toEqual(['focus on cost'])
  })

  it('node_needs_input marks the node waiting with its question', async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: node_start',
      'data: {"node_id":"a","agent":"researcher"}',
      '',
      'event: node_needs_input',
      'data: {"node_id":"a","interrupt_id":"hitl-a-r1","message":"which direction?"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    const ns = store.get('c').live?.dag?.nodeStates['a']
    expect(ns?.status).toBe('needs_input')
    expect(ns?.question).toBe('which direction?')
  })

  it('steerNode POSTs guidance; cancelNode DELETEs the node', () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }))
    store.steerNode('c', 'a', '  do X  ')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/chats/c/nodes/a/steer',
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ guidance: 'do X' }) }),
    )
    store.cancelNode('c', 'a')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/chats/c/nodes/a',
      expect.objectContaining({ method: 'DELETE' }),
    )
  })

  it('steerNode ignores empty guidance', () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }))
    store.steerNode('c', 'a', '   ')
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('retryNode resets the target + descendants, keeps the rest, and POSTs to /retry', async () => {
    // Seed a live DAG: a → b, a done (with an answer), b failed.
    const sse = [
      `event: dag_plan\ndata: ${JSON.stringify({ plan_id: 'p', nodes: [{ id: 'a', agent: 'r', task: 't', depends_on: [] }, { id: 'b', agent: 'r', task: 't', depends_on: ['a'] }], edges: [{ from: 'a', to: 'b' }] })}\n\n`,
      `event: agent_token\ndata: {"node_id":"a","run_id":"worker-r0","text":"A-ANSWER"}\n\n`,
      `event: node_done\ndata: {"node_id":"a"}\n\n`,
      `event: node_failed\ndata: {"node_id":"b","error":"produced no answer"}\n\n`,
      `event: done\ndata: {}\n\n`,
    ].join('')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'hello')
    expect(store.get('c').live?.dag?.nodeStates['b']?.status).toBe('failed')

    fetchMock.mockResolvedValueOnce(makeStream('event: done\ndata: {}\n\n'))
    store.retryNode('c', 'b', '  focus on X  ')

    // Synchronous reset: b (the target) cleared; a (upstream, not downstream of b) kept.
    const dag = store.get('c').live?.dag
    expect(dag?.nodeAnswer['b'] ?? '').toBe('')
    expect(dag?.nodeStates['b']?.status).toBe('queued')
    expect(dag?.nodeAnswer['a']).toContain('A-ANSWER')

    await new Promise(r => setTimeout(r, 0)) // let runStream fire the POST
    expect(fetchMock).toHaveBeenLastCalledWith(
      '/api/v1/chats/c/nodes/b/retry',
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ guidance: 'focus on X' }) }),
    )
  })

  // Timers anchor to the server's start time (epoch ms), not Date.now() at event
  // processing — so a reconnect/replay shows true elapsed instead of resetting.
  it('uses server started_at_ms for the dag and node timers', async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"r","task":"t","depends_on":[]}],"edges":[],"started_at_ms":1000}',
      '',
      'event: node_start',
      'data: {"node_id":"a","agent":"r","started_at_ms":2000}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    const dag = store.get('c').live?.dag
    expect(dag?.startedAt).toBe(1000)
    expect(dag?.nodeStates['a'].startedAt).toBe(2000)
  })
})

describe('isTurnInProgress — re-subscribe gate', () => {
  it('is true only for a turn whose DAG is in_progress', () => {
    expect(isTurnInProgress(dagTurn('in_progress'))).toBe(true)
    expect(isTurnInProgress(dagTurn('completed'))).toBe(false)
    expect(isTurnInProgress(turnWith())).toBe(false) // no DAG (e.g. failed nodes after restart)
    expect(isTurnInProgress(undefined)).toBe(false)
  })
})

// Minimal EventSource stand-in: jsdom has none. Captures listeners so a test can
// feed the same SSE vocabulary the hub replays, and records close().
class FakeEventSource {
  static last: FakeEventSource | null = null
  url: string
  onerror: (() => void) | null = null
  closed = false
  private listeners: Record<string, ((e: MessageEvent) => void)[]> = {}
  constructor(url: string) { this.url = url; FakeEventSource.last = this }
  addEventListener(name: string, cb: (e: MessageEvent) => void) { (this.listeners[name] ??= []).push(cb) }
  close() { this.closed = true }
  emit(name: string, data = '') { for (const cb of this.listeners[name] ?? []) cb({ data } as MessageEvent) }
}

describe('ChatStore.attach — reconnect to a live run', () => {
  let store: ChatStore
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null
    store = new ChatStore()
  })

  it('subscribes to /stream, rebuilds the DAG from replay, and ends on done', () => {
    store.seed('c', [dagTurn('in_progress')])
    store.attach('c')
    const es = FakeEventSource.last!
    expect(es.url).toBe('/api/v1/chats/c/stream')
    expect(store.get('c').live?.streaming).toBe(true)
    // The in-progress turn was lifted out of history into `live` (no double render).
    expect(store.get('c').turns).toHaveLength(0)
    expect(store.get('c').live?.userText).toBe('hi')

    es.emit('dag_plan', '{"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}')
    es.emit('node_start', '{"node_id":"a","agent":"researcher"}')
    expect(store.get('c').live?.dag?.nodeStates['a'].status).toBe('running')

    es.emit('done')
    expect(store.get('c').live?.streaming).toBe(false)
    expect(es.closed).toBe(true)
  })

  it('does not double-subscribe (second attach no-ops)', () => {
    store.seed('c', [dagTurn('in_progress')])
    store.attach('c')
    const first = FakeEventSource.last
    store.attach('c')
    expect(FakeEventSource.last).toBe(first) // no new EventSource opened
  })
})
