import { describe, it, expect, vi, beforeEach } from 'vitest'
import {
  activityFromTurn, isTurnInProgress, ChatStore,
  terminalNodeId, dagTotalTokens, dagAnswerAttribution, turnUsageTotal, plainReplyAttribution, pendingNodeQuestion,
  type DagTurnState,
} from './chatStore'
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

// Minimal SSE stream that closes immediately so the store doesn't hang. Always
// terminated by `done` — every real completed run ends with one, and the
// store now treats its absence as a dropped connection worth reconnecting
// over (see chatStore.test.ts's "reconnect on drop" tests below for that path).
function makeStream(body: string): Response {
  const encoder = new TextEncoder()
  const stream = new ReadableStream({
    start(ctrl) {
      ctrl.enqueue(encoder.encode(body + 'event: done\ndata: {}\n\n'))
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

  // Regression: the archive GET can race the server's own persistence of the
  // turn that just finished streaming. If the refetch's `turns` doesn't yet
  // contain that turn, the previous answer must survive (synthesized from the
  // in-memory `live`) instead of dropping until a manual refresh.
  it('keeps the previous answer when the archive refetch omits the just-finished turn', async () => {
    const sse = [
      'event: response_created',
      'data: {"response_id":"resp-1"}',
      '',
      'event: agent_token',
      'data: {"text":"first answer"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('chat-1', 'msg1')
    expect(store.get('chat-1').live?.id).toBe('resp-1')
    expect(store.get('chat-1').live?.text).toBe('first answer')

    // The archive GET comes back WITHOUT resp-1 (server hasn't persisted it yet).
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ turns: [] }), { status: 200 }))
    fetchMock.mockResolvedValueOnce(makeStream(''))
    await store.submit('chat-1', 'msg2')

    const turns = store.get('chat-1').turns
    expect(turns).toHaveLength(1)
    expect(turns[0].id).toBe('resp-1')
    expect(turns[0].input.content).toBe('msg1')
    const msg = turns[0].output.find(o => o.type === 'message')
    expect(msg && 'content' in msg ? msg.content[0] : undefined).toEqual({ type: 'output_text', text: 'first answer' })
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

  it("a node's answer reflects only its LATEST worker/revise draft — judge commentary never leaks in, and a revision replaces (doesn't concatenate with) the draft it revised", async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      // Worker's first draft (an ask_advisor consult may have happened inside this
      // same run as an ordinary tool call — not a separate stage) — this becomes
      // the answer.
      'event: agent_start',
      'data: {"node_id":"a","run_id":"worker-r0","agent":"researcher","stage":"worker"}',
      '',
      'event: agent_token',
      'data: {"node_id":"a","run_id":"worker-r0","text":"DRAFT ONE (unsourced)"}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"worker-r0","stage":"worker"}',
      '',
      // Judge commentary runs between draft and revision — it must never reach the answer.
      'event: agent_start',
      'data: {"node_id":"a","run_id":"judge-r1","agent":"judge","stage":"judge","round":1}',
      '',
      'event: agent_token',
      'data: {"node_id":"a","run_id":"judge-r1","text":"Feedback: needs more sourcing."}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"judge-r1","stage":"judge","round":1}',
      '',
      // Judge fails it, triggering a revision — the revision REPLACES the draft.
      'event: agent_start',
      'data: {"node_id":"a","run_id":"worker-r1","agent":"researcher","stage":"revise","round":1}',
      '',
      'event: agent_token',
      'data: {"node_id":"a","run_id":"worker-r1","text":"REVISED ANSWER (sourced)"}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"worker-r1","stage":"revise","round":1}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    const answer = store.get('c').live?.dag?.nodeAnswer['a']
    expect(answer).toBe('REVISED ANSWER (sourced)')
  })

  it('steerNode and cancelNode PUT the node status endpoint', () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 200 }))
    store.steerNode('c', 'a', '  do X  ')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/chats/c/nodes/a/status',
      expect.objectContaining({ method: 'PUT', body: JSON.stringify({ status: 'running', guidance: 'do X' }) }),
    )
    store.cancelNode('c', 'a')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/chats/c/nodes/a/status',
      expect.objectContaining({ method: 'PUT', body: JSON.stringify({ status: 'cancelled' }) }),
    )
  })

  it('steerNode ignores empty guidance', () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }))
    store.steerNode('c', 'a', '   ')
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('retryNode resets the target + descendants, PUTs the node status endpoint, then watches progress via GET /stream', async () => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null

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

    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ status: 'queued' }), { status: 200 }))
    store.retryNode('c', 'b', '  focus on X  ')

    // Synchronous reset: b (the target) cleared; a (upstream, not downstream of b) kept.
    const dag = store.get('c').live?.dag
    expect(dag?.nodeAnswer['b'] ?? '').toBe('')
    expect(dag?.nodeStates['b']?.status).toBe('queued')
    expect(dag?.nodeAnswer['a']).toContain('A-ANSWER')

    await new Promise(r => setTimeout(r, 0)) // let the PUT's .then() fire
    expect(fetchMock).toHaveBeenLastCalledWith(
      '/api/v1/chats/c/nodes/b/status',
      expect.objectContaining({ method: 'PUT', body: JSON.stringify({ status: 'queued', guidance: 'focus on X' }) }),
    )
    // The re-run's progress streams over GET /stream, not the PUT's own body.
    const es = FakeEventSource.last!
    expect(es.url).toBe('/api/v1/chats/c/stream')
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

// dag builds a minimal DagTurnState: two nodes, b depends on a (a is not
// terminal, b is), so answer attribution + total tokens have a real "which node
// is terminal" question to answer.
function dag(nodeStates: DagTurnState['nodeStates']): DagTurnState {
  return {
    planId: 'p',
    nodes: [
      { id: 'a', agent: 'web-researcher', task: 't', depends_on: [] },
      { id: 'b', agent: 'synthesizer', task: 't', depends_on: ['a'] },
    ],
    edges: [{ from: 'a', to: 'b' }],
    nodeStates,
    nodeRuns: {},
    nodeAnswer: {},
  }
}

describe('answer-bubble attribution helpers', () => {
  it('terminalNodeId finds the node with no successor', () => {
    expect(terminalNodeId(dag({}).nodes)).toBe('b')
  })

  it('terminalNodeId returns undefined for an empty DAG', () => {
    expect(terminalNodeId([])).toBeUndefined()
  })

  it('dagTotalTokens sums total_tokens across every node', () => {
    const d = dag({ a: { status: 'done', totalTokens: 100 }, b: { status: 'done', totalTokens: 50 } })
    expect(dagTotalTokens(d)).toBe(150)
  })

  it('dagTotalTokens is 0 when no node reports usage', () => {
    expect(dagTotalTokens(dag({}))).toBe(0)
  })

  it("dagAnswerAttribution credits the terminal node's agent + its own model/tokens", () => {
    const d = dag({
      a: { status: 'done', model: 'qwen3-30b-a3b', totalTokens: 999 },
      b: { status: 'done', model: 'gpt-oss-120b', totalTokens: 500 },
    })
    expect(dagAnswerAttribution(d)).toEqual({ agent: 'synthesizer', model: 'gpt-oss-120b', tokens: 500 })
  })

  it('dagAnswerAttribution omits model/tokens when the terminal node has none yet', () => {
    const d = dag({ b: { status: 'running' } })
    expect(dagAnswerAttribution(d)).toEqual({ agent: 'synthesizer', model: undefined, tokens: undefined })
  })

  it('turnUsageTotal sums input+output tokens from a persisted Turn', () => {
    const turn: Turn = { id: 't', created_at: '', input: { role: 'user', content: 'hi' }, output: [], usage: { input_tokens: 40, output_tokens: 17 } }
    expect(turnUsageTotal(turn)).toBe(57)
  })

  it('turnUsageTotal is undefined when the turn carries no usage (e.g. a DAG-only turn)', () => {
    const turn: Turn = { id: 't', created_at: '', input: { role: 'user', content: 'hi' }, output: [] }
    expect(turnUsageTotal(turn)).toBeUndefined()
  })

  it('plainReplyAttribution credits the orchestrator with the turn-persisted model + tokens', () => {
    const turn: Turn = {
      id: 't', created_at: '', input: { role: 'user', content: 'hi' }, output: [],
      model: 'gpt-oss-120b', usage: { input_tokens: 40, output_tokens: 17 },
    }
    expect(plainReplyAttribution(turn)).toEqual({ agent: 'orchestrator', model: 'gpt-oss-120b', tokens: 57 })
  })

  it('plainReplyAttribution omits model/tokens when the turn carries neither', () => {
    const turn: Turn = { id: 't', created_at: '', input: { role: 'user', content: 'hi' }, output: [] }
    expect(plainReplyAttribution(turn)).toEqual({ agent: 'orchestrator', model: undefined, tokens: undefined })
  })

  it('pendingNodeQuestion finds a paused node awaiting an answer, credited to its own agent', () => {
    const d = dag({ a: { status: 'done' }, b: { status: 'needs_input', question: 'Which time zone?' } })
    expect(pendingNodeQuestion(d)).toEqual({ nodeId: 'b', agent: 'synthesizer', question: 'Which time zone?' })
  })

  it('pendingNodeQuestion is undefined when no node is waiting', () => {
    expect(pendingNodeQuestion(dag({ a: { status: 'done' }, b: { status: 'running' } }))).toBeUndefined()
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
// feed the same SSE vocabulary the hub replays, and records close(). emit's
// optional `id` mirrors the SSE `id:` field — EventSource surfaces it on the
// MessageEvent as `lastEventId`, which is what a reconnect resumes past.
class FakeEventSource {
  static last: FakeEventSource | null = null
  url: string
  onerror: (() => void) | null = null
  closed = false
  private listeners: Record<string, ((e: MessageEvent) => void)[]> = {}
  constructor(url: string) { this.url = url; FakeEventSource.last = this }
  addEventListener(name: string, cb: (e: MessageEvent) => void) { (this.listeners[name] ??= []).push(cb) }
  close() { this.closed = true }
  emit(name: string, data = '', id?: number) {
    const event = { data, lastEventId: id != null ? String(id) : '' } as MessageEvent
    for (const cb of this.listeners[name] ?? []) cb(event)
  }
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

// Issue #383: a dropped SSE connection must be retried automatically —
// resuming via Last-Event-ID — instead of tearing the run down and forcing a
// manual page refresh.
describe('ChatStore — reconnect on a dropped stream (#383)', () => {
  let store: ChatStore
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null
    store = new ChatStore()
  })

  it('an EventSource drop mid-run reconnects with Last-Event-ID and resumes without losing or duplicating events', () => {
    vi.useFakeTimers()
    try {
      store.seed('c', [dagTurn('in_progress')])
      store.attach('c')
      const es1 = FakeEventSource.last!
      expect(es1.url).toBe('/api/v1/chats/c/stream')

      es1.emit('dag_plan', '{"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}', 1)
      es1.emit('node_start', '{"node_id":"a","agent":"researcher"}', 2)
      expect(store.get('c').live?.dag?.nodeStates['a'].status).toBe('running')

      // Connection drops mid-run — no `done` was seen, so this must NOT tear
      // the turn down (unlike the server's expected post-`done` close).
      es1.onerror?.()
      expect(es1.closed).toBe(true)
      expect(store.get('c').live?.streaming).toBe(true)

      // Bounded backoff, not an immediate hammer: no reconnect until the delay elapses.
      expect(FakeEventSource.last).toBe(es1)
      vi.advanceTimersByTime(999)
      expect(FakeEventSource.last).toBe(es1)
      vi.advanceTimersByTime(1)

      const es2 = FakeEventSource.last!
      expect(es2).not.toBe(es1)
      expect(es2.url).toBe('/api/v1/chats/c/stream?last_event_id=2') // resumes past the last event actually seen

      // The node's prior state (from es1) is untouched — resuming replays only
      // what's new, so nothing already applied is lost or re-applied.
      expect(store.get('c').live?.dag?.nodeStates['a'].status).toBe('running')
      es2.emit('node_done', '{"node_id":"a"}', 3)
      expect(store.get('c').live?.dag?.nodeStates['a'].status).toBe('done')

      es2.emit('done', '{}', 4)
      expect(store.get('c').live?.streaming).toBe(false)
      expect(es2.closed).toBe(true) // onDone tore it down cleanly
    } finally {
      vi.useRealTimers()
    }
  })

  it('a clean close after `done` does not reconnect', () => {
    store.seed('c', [dagTurn('in_progress')])
    store.attach('c')
    const es = FakeEventSource.last!
    es.emit('done', '{}', 1)
    expect(store.get('c').live?.streaming).toBe(false)
    // EventSource fires `error` too once the server closes the connection —
    // must not be mistaken for a drop and reopen a new stream.
    es.onerror?.()
    expect(FakeEventSource.last).toBe(es)
  })

  it('gives up and surfaces an error after repeated reconnect failures, without retrying forever', () => {
    vi.useFakeTimers()
    try {
      store.seed('c', [dagTurn('in_progress')])
      store.attach('c')

      let last = FakeEventSource.last!
      let attempts = 0
      while (store.get('c').live?.streaming && attempts < 20) {
        last.onerror?.()
        vi.advanceTimersByTime(30_000) // more than the max backoff delay
        last = FakeEventSource.last!
        attempts++
      }

      expect(attempts).toBeLessThan(20) // it gave up rather than retrying forever
      expect(store.get('c').live?.streaming).toBe(false)
      expect(store.get('c').error).toMatch(/lost connection/i)
    } finally {
      vi.useRealTimers()
    }
  })

  it('a POST stream that drops (no `done`, body just ends) hands off to the resumable GET stream, resetting local state first so the replay is not duplicated', async () => {
    const dropped = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: agent_token',
      'data: {"node_id":"a","run_id":"worker-r0","text":"partial answer"}',
      '',
      // no `done` — the body simply ends here, simulating a mid-run drop.
    ].join('\n')
    const encoder = new TextEncoder()
    const stream = new ReadableStream({ start(ctrl) { ctrl.enqueue(encoder.encode(dropped)); ctrl.close() } })
    const res = new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(res))

    await store.submit('c', 'go')

    // The turn is still live — handed off to the GET stream, not failed.
    expect(store.get('c').live?.streaming).toBe(true)
    const es = FakeEventSource.last!
    expect(es.url).toBe('/api/v1/chats/c/stream') // no id to resume past on this path — full replay
    // Local state was cleared before the handoff, so the replay below isn't duplication.
    expect(store.get('c').live?.dag).toBeUndefined()
    expect(store.get('c').live?.text).toBe('')

    es.emit('dag_plan', '{"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}', 1)
    es.emit('node_done', '{"node_id":"a","output_preview":"final"}', 2)
    expect(store.get('c').live?.dag?.nodeStates['a'].status).toBe('done')

    es.emit('done', '{}', 3)
    expect(store.get('c').live?.streaming).toBe(false)
  })
})
