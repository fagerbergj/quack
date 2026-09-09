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
// terminated by `done` - every real completed run ends with one, and the
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

describe('ChatStore.submit - loading indicator gap (regression)', () => {
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
  // GET resolves - otherwise the spinner doesn't show until the first token.
  it('sets submitting/pendingUserText before the archive GET resolves', async () => {
    // First turn - complete it so a finished `live` lingers (the archive trigger).
    fetchMock.mockResolvedValueOnce(makeStream(''))
    await store.submit('chat-1', 'msg1')
    expect(store.get('chat-1').live?.streaming).toBe(false)

    // Second turn - make the archive GET hang so we can observe state mid-flight.
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
  // `submitting` phase - the live turn is created immediately.
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

// makeHangingStream is a `submit` response whose body never closes until the
// test calls `close()` - lets a test observe state while a run is still
// streaming, and then trigger its completion at will.
function makeHangingStream(): { response: Response; close: () => void } {
  const encoder = new TextEncoder()
  let controller!: ReadableStreamDefaultController<Uint8Array>
  const stream = new ReadableStream({ start(ctrl) { controller = ctrl } })
  return {
    response: new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }),
    close: () => {
      controller.enqueue(encoder.encode('event: done\ndata: {}\n\n'))
      controller.close()
    },
  }
}

describe('ChatStore - main-chat message queue', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  let store: ChatStore

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    store = new ChatStore()
    store.seed('chat-1', [])
  })

  it('queueing while streaming holds the message instead of starting a second run', async () => {
    const hang = makeHangingStream()
    fetchMock.mockResolvedValueOnce(hang.response)
    const p = store.submit('chat-1', 'msg1')
    expect(store.get('chat-1').live?.streaming).toBe(true)

    store.queueTurn('chat-1', 'follow-up')
    expect(store.get('chat-1').queue).toHaveLength(1)
    expect(store.get('chat-1').queue[0].text).toBe('follow-up')
    // Still streaming, and no second fetch (POST/GET) has fired for it.
    expect(store.get('chat-1').live?.userText).toBe('msg1')
    expect(fetchMock).toHaveBeenCalledTimes(1)

    hang.close()
    await p
  })

  it('submits the queued message once the run finishes', async () => {
    const hang = makeHangingStream()
    fetchMock.mockResolvedValueOnce(hang.response)
    const p = store.submit('chat-1', 'msg1')
    store.queueTurn('chat-1', 'follow-up')

    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ turns: [] }), { status: 200 })) // archive GET
    fetchMock.mockResolvedValueOnce(makeStream(''))                                               // follow-up's own run

    hang.close()
    await p

    await vi.waitFor(() => expect(store.get('chat-1').live?.userText).toBe('follow-up'))
    expect(store.get('chat-1').queue).toEqual([])
  })

  it('drains multiple queued messages in order, submitting the next only after the prior completes', async () => {
    const hang1 = makeHangingStream()
    fetchMock.mockResolvedValueOnce(hang1.response)
    const p1 = store.submit('chat-1', 'msg1')
    store.queueTurn('chat-1', 'follow-up-1')
    store.queueTurn('chat-1', 'follow-up-2')
    expect(store.get('chat-1').queue.map(q => q.text)).toEqual(['follow-up-1', 'follow-up-2'])

    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ turns: [] }), { status: 200 })) // archive GET
    const hang2 = makeHangingStream()
    fetchMock.mockResolvedValueOnce(hang2.response)                                               // follow-up-1's run

    hang1.close()
    await p1

    await vi.waitFor(() => expect(store.get('chat-1').live?.userText).toBe('follow-up-1'))
    // follow-up-2 stays queued until follow-up-1's own run finishes.
    expect(store.get('chat-1').queue.map(q => q.text)).toEqual(['follow-up-2'])
    expect(store.get('chat-1').live?.streaming).toBe(true)

    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ turns: [] }), { status: 200 })) // archive GET
    fetchMock.mockResolvedValueOnce(makeStream(''))                                               // follow-up-2's run
    hang2.close()

    await vi.waitFor(() => expect(store.get('chat-1').live?.userText).toBe('follow-up-2'))
    expect(store.get('chat-1').queue).toEqual([])
  })

  it('removes a queued message before it is sent', () => {
    store.queueTurn('chat-1', 'a')
    store.queueTurn('chat-1', 'b')
    const id = store.get('chat-1').queue[0].id
    store.unqueueTurn('chat-1', id)
    expect(store.get('chat-1').queue.map(q => q.text)).toEqual(['b'])
  })

  it('is a no-op for a blank message', () => {
    store.queueTurn('chat-1', '   ')
    expect(store.get('chat-1').queue).toEqual([])
  })
})

describe('ChatStore - mid-node steering', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  let store: ChatStore

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    store = new ChatStore()
    store.seed('c', [])
  })

  it('node_steered keeps the node running (not queued) and records the guidance', async () => {
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
    // Regression (#870): onNodeSteered used to set 'queued' - an illegal
    // running→queued transition per the backend's own state machine - and
    // nothing ever restored 'running', so the node rendered idle chrome
    // (no pulse, no spinner, canQueue gone) for the whole steered re-run.
    expect(ns?.status).toBe('running')
    expect(ns?.steers).toEqual(['focus on cost'])
  })

  // Full steer→resume sequence, observed as it streams (not just at the end):
  // the node must read 'running' (DagNode's running-derived UI -
  // pulse/spinner/canQueue) at every point between the steer and node_done,
  // never dropping to 'queued' or idle chrome while the resumed run streams
  // tokens.
  it('stays running across the full node_steered → agent_start → agent_token → node_done sequence', async () => {
    const encoder = new TextEncoder()
    let controller!: ReadableStreamDefaultController<Uint8Array>
    const stream = new ReadableStream({ start(ctrl) { controller = ctrl } })
    fetchMock.mockResolvedValueOnce(new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }))
    const p = store.submit('c', 'go')

    const send = (chunk: string) => controller.enqueue(encoder.encode(chunk))

    send('event: dag_plan\ndata: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}\n\n')
    send('event: node_start\ndata: {"node_id":"a","agent":"researcher"}\n\n')
    send('event: node_steered\ndata: {"node_id":"a","guidance":"focus on cost"}\n\n')
    await vi.waitFor(() => expect(store.get('c').live?.dag?.nodeStates['a']?.status).toBe('running'))

    // Resumed run: agent_start must restore/keep 'running' (the fix's (b) half).
    send('event: agent_start\ndata: {"node_id":"a","run_id":"worker-r1","agent":"researcher","stage":"worker"}\n\n')
    await vi.waitFor(() => expect(store.get('c').live?.dag?.nodeRuns['a']?.length).toBe(1))
    expect(store.get('c').live?.dag?.nodeStates['a']?.status).toBe('running')

    // agent_token must not disturb it.
    send('event: agent_token\ndata: {"node_id":"a","run_id":"worker-r1","text":"partial"}\n\n')
    await vi.waitFor(() => expect(store.get('c').live?.dag?.nodeAnswer['a']).toBe('partial'))
    expect(store.get('c').live?.dag?.nodeStates['a']?.status).toBe('running')

    send('event: node_done\ndata: {"node_id":"a"}\n\n')
    send('event: done\ndata: {}\n\n')
    controller.close()
    await p

    expect(store.get('c').live?.dag?.nodeStates['a']?.status).toBe('done')
  })

  it('node_paused marks the node paused, keeping its accumulated answer', async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: node_start',
      'data: {"node_id":"a","agent":"researcher"}',
      '',
      'event: agent_token',
      'data: {"node_id":"a","run_id":"worker-r0","text":"partial draft"}',
      '',
      'event: node_paused',
      'data: {"node_id":"a"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    const dag = store.get('c').live?.dag
    expect(dag?.nodeStates['a']?.status).toBe('paused')
    expect(dag?.nodeAnswer['a']).toBe('partial draft')
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

  it('delivery_result fills in traceId when node_start never carried one (reconnect case)', async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: node_start',
      'data: {"node_id":"a","agent":"researcher"}',
      '',
      'event: delivery_result',
      'data: {"node_id":"a","outcome":"delivered","trace_id":"trace-123"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    expect(store.get('c').live?.dag?.nodeStates['a']?.traceId).toBe('trace-123')
  })

  it("delivery_result's traceId does not clobber one node_start already set", async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: node_start',
      'data: {"node_id":"a","agent":"researcher","trace_id":"trace-original"}',
      '',
      'event: delivery_result',
      'data: {"node_id":"a","outcome":"delivered","trace_id":"trace-stale"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    expect(store.get('c').live?.dag?.nodeStates['a']?.traceId).toBe('trace-original')
  })

  it("a node's answer reflects only its LATEST worker/revise draft - judge commentary never leaks in, and a revision replaces (doesn't concatenate with) the draft it revised", async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      // Worker's first draft (an ask_advisor consult may have happened inside this
      // same run as an ordinary tool call - not a separate stage) - this becomes
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
      // Judge commentary runs between draft and revision - it must never reach the answer.
      'event: agent_start',
      'data: {"node_id":"a","run_id":"judge-r1","agent":"judge","stage":"judge","round":1}',
      '',
      'event: agent_token',
      'data: {"node_id":"a","run_id":"judge-r1","text":"Feedback: needs more sourcing."}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"judge-r1","stage":"judge","round":1}',
      '',
      // Judge fails it, triggering a revision - the revision REPLACES the draft.
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

    // #696: keeping judge text OUT of the answer must not throw it away - it
    // belongs in the judge's own card. judgePartEmitter only emits
    // agent_thinking for parts the model marks Thought, and local models
    // mostly don't, so dropping agent_token discarded nearly all of it.
    const judgeRun = store.get('c').live?.dag?.nodeRuns?.['a']?.find(r => r.runId === 'judge-r1')
    expect(judgeRun?.activity).toContainEqual({ kind: 'thinking', text: 'Feedback: needs more sourcing.' })
  })

  // The other half of #696: a revise run IS an answer stage, so its text goes to
  // the answer box and must NOT also be duplicated into its own card.
  it("a revise run's text goes to the answer box, not into its card's activity", async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"worker-r1","agent":"researcher","stage":"revise","round":1}',
      '',
      'event: agent_token',
      'data: {"node_id":"a","run_id":"worker-r1","text":"REVISED TEXT"}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"worker-r1","stage":"revise","round":1}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    expect(store.get('c').live?.dag?.nodeAnswer['a']).toBe('REVISED TEXT')
    const rev = store.get('c').live?.dag?.nodeRuns?.['a']?.find(r => r.runId === 'worker-r1')
    expect(rev?.activity).toEqual([])
  })

  // #387: narration a worker emits BEFORE a tool call ("I'll check X first...")
  // must not render as if it were the answer once the real answer streams in
  // after the call - mirrors internal/acp/translate.go's per-round reset
  // (#358), applied here to the live stream (a node's own worker/revise run,
  // not just the ACP-delivered final text).
  it("a tool call within a worker run discards narration emitted before it from the node's answer", async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"worker-r0","agent":"researcher","stage":"worker"}',
      '',
      'event: agent_token',
      'data: {"node_id":"a","run_id":"worker-r0","text":"Let me look that up first."}',
      '',
      'event: agent_tool_call',
      'data: {"node_id":"a","run_id":"worker-r0","call_id":"c1","name":"web_search","args":{"query":"x"}}',
      '',
      'event: agent_tool_result',
      'data: {"node_id":"a","run_id":"worker-r0","call_id":"c1","name":"web_search","result":{}}',
      '',
      'event: agent_token',
      'data: {"node_id":"a","run_id":"worker-r0","text":"the real answer"}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"worker-r0","stage":"worker"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    expect(store.get('c').live?.dag?.nodeAnswer['a']).toBe('the real answer')
  })

  // Same reset for the orchestrator's own top-level (no DAG node) reply.
  it('a top-level tool call discards narration emitted before it from the live text', async () => {
    const sse = [
      'event: agent_token',
      'data: {"text":"Let me check something first."}',
      '',
      'event: agent_tool_call',
      'data: {"run_id":"orchestrator","call_id":"c1","name":"get_user_choice","args":{}}',
      '',
      'event: agent_tool_result',
      'data: {"run_id":"orchestrator","call_id":"c1","name":"get_user_choice","result":{}}',
      '',
      'event: agent_token',
      'data: {"text":"the real answer"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    expect(store.get('c').live?.text).toBe('the real answer')
  })

  // #422: a second top-level run against the same live turn (e.g. the GitHub
  // dispatch driving the orchestrator twice when its first pass ran no plan)
  // must not concatenate its answer onto the first run's - the answer bubble
  // rendered the reply doubled before this reset existed.
  it('a second top-level run replaces the first run\'s live text instead of appending to it', async () => {
    const sse = [
      'event: agent_start',
      'data: {"run_id":"orchestrator-r1","agent":"orchestrator","stage":"worker"}',
      '',
      'event: agent_token',
      'data: {"run_id":"orchestrator-r1","text":"first attempt answer"}',
      '',
      'event: agent_complete',
      'data: {"run_id":"orchestrator-r1","stage":"worker"}',
      '',
      'event: agent_start',
      'data: {"run_id":"orchestrator-r2","agent":"orchestrator","stage":"worker"}',
      '',
      'event: agent_token',
      'data: {"run_id":"orchestrator-r2","text":"second attempt answer"}',
      '',
      'event: agent_complete',
      'data: {"run_id":"orchestrator-r2","stage":"worker"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    expect(store.get('c').live?.text).toBe('second attempt answer')
  })

  it('stopNode POSTs to the stop endpoint and pauseNode PUTs the status endpoint with a reason', () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 200 }))
    store.stopNode('c', 'a')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/chats/c/nodes/a/stop',
      expect.objectContaining({ method: 'POST' }),
    )
    store.pauseNode('c', 'a')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/chats/c/nodes/a/status',
      expect.objectContaining({ method: 'PUT', body: JSON.stringify({ status: 'paused', reason: undefined }) }),
    )
    store.pauseNode('c', 'a', 'shutdown')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/chats/c/nodes/a/status',
      expect.objectContaining({ method: 'PUT', body: JSON.stringify({ status: 'paused', reason: 'shutdown' }) }),
    )
  })

  it('startNode POSTs an answer to the start endpoint', async () => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null
    const sse = [
      `event: dag_plan\ndata: ${JSON.stringify({ plan_id: 'p', nodes: [{ id: 'a', agent: 'r', task: 't', depends_on: [] }], edges: [] })}\n\n`,
      `event: node_needs_input\ndata: {"node_id":"a","interrupt_id":"i1","message":"which region?"}\n\n`,
      `event: done\ndata: {}\n\n`,
    ].join('')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')

    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ status: 'queued' }), { status: 200 }))
    store.startNode('c', 'a', 'the answer')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/chats/c/nodes/a/start',
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ content: 'the answer' }) }),
    )
  })

  it('startNode mid-stream is refused with a node error note, not a silent no-op', async () => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null
    // Stream stays OPEN (no close, no done): the run is mid-flight when
    // startNode is called, so submit is deliberately not awaited.
    const sse = `event: dag_plan\ndata: ${JSON.stringify({ plan_id: 'p', nodes: [{ id: 'a', agent: 'r', task: 't', depends_on: [] }], edges: [] })}\n\nevent: node_paused\ndata: {"node_id":"a"}\n\n`
    const open = new ReadableStream({ start(ctrl) { ctrl.enqueue(new TextEncoder().encode(sse)) } })
    fetchMock.mockResolvedValueOnce(new Response(open, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }))
    void store.submit('c', 'go')
    await new Promise(r => setTimeout(r, 20))
    expect(store.get('c').live?.streaming).toBe(true)

    fetchMock.mockClear()
    store.startNode('c', 'a')
    expect(fetchMock).not.toHaveBeenCalled()
    expect(store.get('c').live?.dag?.nodeStates['a']?.error).toMatch(/still streaming/)
  })

  it('queueNodeMessage POSTs to the queue endpoint and ignores empty text', async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ id: 'q1', text: 'do X', status: 'queued', delivered: false, created_at: '2026-01-01T00:00:00Z' }), { status: 200 }))
    await store.queueNodeMessage('c', 'a', '  do X  ')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/chats/c/nodes/a/queue',
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ message: 'do X' }) }),
    )
    fetchMock.mockClear()
    await store.queueNodeMessage('c', 'a', '   ')
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('editNodeTask PATCHes the node and updates the local plan def on success', async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"original","depends_on":[]}],"edges":[]}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')

    fetchMock.mockResolvedValueOnce(new Response(null, { status: 200 }))
    const ok = await store.editNodeTask('c', 'a', 'revised task')
    expect(ok).toBe(true)
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/chats/c/nodes/a',
      expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ task: 'revised task' }) }),
    )
    const node = store.get('c').live?.dag?.nodes.find(n => n.id === 'a')
    expect(node?.task).toBe('revised task')
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
  // processing - so a reconnect/replay shows true elapsed instead of resetting.
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

  // Root cause of the sub-step (worker/judge/revise) timer resetting on page
  // refresh: agent_start carried no server timestamp, so a replayed run always
  // anchored to Date.now() at replay time. Mirrors the dag/node test above for
  // the run level.
  it('uses server started_at_ms for a sub-step (agent_start) timer, not replay-time Date.now()', async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"r","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: node_start',
      'data: {"node_id":"a","agent":"r"}',
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"judge-r1","agent":"judge","stage":"judge","started_at_ms":5000}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    const run = store.get('c').live?.dag?.nodeRuns['a']?.find(r => r.runId === 'judge-r1')
    expect(run?.startedAt).toBe(5000)
  })

  // A live run (no replay) carries no started_at_ms yet - it still anchors, this
  // time to Date.now() at arrival, so it starts ticking from ~0 immediately.
  it('falls back to Date.now() for a live agent_start with no started_at_ms', async () => {
    const before = Date.now()
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"r","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: node_start',
      'data: {"node_id":"a","agent":"r"}',
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"worker-r0","agent":"r","stage":"worker"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    const after = Date.now()
    const run = store.get('c').live?.dag?.nodeRuns['a']?.find(r => r.runId === 'worker-r0')
    expect(run?.startedAt).toBeGreaterThanOrEqual(before)
    expect(run?.startedAt).toBeLessThanOrEqual(after)
  })

  // Clock-skew: a server clock ahead of the client stores the RAW server
  // value, unclamped - clamping it to the client's now (a prior version did
  // this) is what corrupted a finished run's duration (finished_at_ms -
  // clamped start) whenever the client trailed the server. The "never show a
  // negative elapsed" guard lives in fmtMs (floors at 0) instead, so a
  // still-live timer never renders negative even while raw start > client now.
  it('stores a future server started_at_ms (clock skew) unclamped', async () => {
    const future = Date.now() + 60_000
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"r","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: node_start',
      'data: {"node_id":"a","agent":"r"}',
      '',
      'event: agent_start',
      `data: {"node_id":"a","run_id":"judge-r1","agent":"judge","stage":"judge","started_at_ms":${future}}`,
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    const run = store.get('c').live?.dag?.nodeRuns['a']?.find(r => r.runId === 'judge-r1')
    expect(run?.startedAt).toBe(future)
  })
})

// agent_complete/node_done/node_failed/node_cancelled now carry a server-clock
// finished_at_ms, so a finished run/node's duration comes from two server
// timestamps - never from Date.now() at whatever moment the client happens to
// process (or replay) the event.
describe('ChatStore - server-timestamped durations survive replay', () => {
  // One node running worker -> judge(reject) -> revise -> judge(pass) -> done,
  // every agent_start/agent_complete/node_done carrying explicit server
  // timestamps. runDagOnce replays this SAME event list into a fresh store.
  function dagSSE(): string {
    return [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"r","task":"t","depends_on":[]}],"edges":[],"started_at_ms":1000}',
      '',
      'event: node_start',
      'data: {"node_id":"a","agent":"r","started_at_ms":2000}',
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"worker-r0","agent":"r","stage":"worker","started_at_ms":2000}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"worker-r0","stage":"worker","finished_at_ms":8000}', // 6000ms
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"judge-r1","agent":"judge","stage":"judge","round":1,"started_at_ms":8000}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"judge-r1","stage":"judge","round":1,"passed":false,"finished_at_ms":13000}', // 5000ms
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"worker-r1","agent":"r","stage":"revise","round":1,"started_at_ms":13000}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"worker-r1","stage":"revise","round":1,"finished_at_ms":20000}', // 7000ms
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"judge-r2","agent":"judge","stage":"judge","round":2,"started_at_ms":20000}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"judge-r2","stage":"judge","round":2,"passed":true,"finished_at_ms":25000}', // 5000ms
      '',
      'event: node_done',
      'data: {"node_id":"a","duration_ms":23000,"finished_at_ms":25000}', // 25000 - 2000
      '',
    ].join('\n')
  }

  async function runDagOnce(fetchNowMs: number): Promise<DagTurnState> {
    const nowSpy = vi.spyOn(Date, 'now').mockReturnValue(fetchNowMs)
    try {
      const fetchMock = vi.fn().mockResolvedValueOnce(makeStream(dagSSE()))
      vi.stubGlobal('fetch', fetchMock)
      const store = new ChatStore()
      store.seed('c', [])
      await store.submit('c', 'go')
      return store.get('c').live!.dag!
    } finally {
      nowSpy.mockRestore()
    }
  }

  it('replaying the same event list at two different client Date.now() values yields identical run/node durations', async () => {
    const dagA = await runDagOnce(10_000_000_000) // "live": processed shortly after emission
    const dagB = await runDagOnce(20_000_000_000) // "replay": processed ~2.8 hours later

    const runsA = dagA.nodeRuns['a'].map(r => ({ runId: r.runId, durationMs: r.durationMs }))
    const runsB = dagB.nodeRuns['a'].map(r => ({ runId: r.runId, durationMs: r.durationMs }))
    expect(runsA).toEqual(runsB)
    expect(runsA.map(r => r.durationMs)).toEqual([6000, 5000, 7000, 5000])

    expect(dagA.nodeStates['a'].serverDurationMs).toBe(23000)
    expect(dagB.nodeStates['a'].serverDurationMs).toBe(23000)
    expect(dagA.nodeStates['a'].finishedAt).toBe(dagB.nodeStates['a'].finishedAt)
    expect(dagA.finishedAt).toBe(dagB.finishedAt)
  })

  it("a finished node's sub-run durations sum to no more than the node's own duration, each starting no earlier than the previous one finished", async () => {
    const dag = await runDagOnce(50_000_000_000)
    const runs = dag.nodeRuns['a']
    const sumOfRuns = runs.reduce((sum, r) => sum + (r.durationMs ?? 0), 0)
    expect(sumOfRuns).toBeLessThanOrEqual(dag.nodeStates['a'].serverDurationMs!)
    for (let i = 1; i < runs.length; i++) {
      const prevFinish = runs[i - 1].startedAt! + runs[i - 1].durationMs!
      expect(runs[i].startedAt).toBeGreaterThanOrEqual(prevFinish)
    }
  })

  // Regression: anchorTime used to clamp a run/node/plan's stored startedAt to
  // min(serverMs, Date.now()) - since finished_at_ms is never clamped, a
  // client clock trailing the server picked up its OWN (earlier) now as the
  // start, stretching every finished-start subtraction by the skew. Every
  // dagSSE() timestamp (1000-25000ms) sits well ahead of this mocked "now"
  // (500ms), so the old clamp fired on every single one of them.
  it('a client clock trailing the server does not inflate sub-run or plan durations', async () => {
    const dag = await runDagOnce(500)
    expect(dag.startedAt).toBe(1000)
    expect(dag.nodeStates['a'].startedAt).toBe(2000)
    expect(dag.nodeRuns['a'].map(r => r.durationMs)).toEqual([6000, 5000, 7000, 5000])
  })
})

describe('ChatStore - finished_at_ms: remaining lifecycle paths', () => {
  it('a run still open at replay time (no terminal event yet) gets its duration from server timestamps once it completes live', () => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null
    const store = new ChatStore()
    store.seed('c', [dagTurn('in_progress')])
    store.attach('c')
    const es = FakeEventSource.last!

    // The replay/reconnect itself lands long after the run actually started -
    // this must never leak into the eventual duration.
    const nowSpy = vi.spyOn(Date, 'now').mockReturnValue(90_000)
    try {
      es.emit('dag_plan', '{"plan_id":"p","nodes":[{"id":"a","agent":"r","task":"t","depends_on":[]}],"edges":[],"started_at_ms":1000}')
      es.emit('node_start', '{"node_id":"a","agent":"r","started_at_ms":2000}')
      es.emit('agent_start', '{"node_id":"a","run_id":"worker-r0","agent":"r","stage":"worker","started_at_ms":2000}')
      expect(store.get('c').live?.dag?.nodeStates['a'].status).toBe('running')

      // The run then finishes LIVE on this same held-open stream.
      es.emit('agent_complete', '{"node_id":"a","run_id":"worker-r0","stage":"worker","finished_at_ms":8000}')
      es.emit('node_done', '{"node_id":"a","finished_at_ms":8000}')
    } finally {
      nowSpy.mockRestore()
    }

    const run = store.get('c').live?.dag?.nodeRuns['a']?.find(r => r.runId === 'worker-r0')
    expect(run?.durationMs).toBe(6000) // 8000 - 2000, not stretched by the 90_000 replay-processing clock
    expect(store.get('c').live?.dag?.nodeStates['a'].finishedAt).toBe(8000)
  })

  async function runOneNode(event: string, data: string, nowMs: number) {
    const nowSpy = vi.spyOn(Date, 'now').mockReturnValue(nowMs)
    try {
      const sse = [
        'event: dag_plan',
        'data: {"plan_id":"p","nodes":[{"id":"a","agent":"r","task":"t","depends_on":[]}],"edges":[],"started_at_ms":1000}',
        '',
        'event: node_start',
        'data: {"node_id":"a","agent":"r","started_at_ms":2000}',
        '',
        `event: ${event}`,
        `data: ${data}`,
        '',
      ].join('\n')
      const fetchMock = vi.fn().mockResolvedValueOnce(makeStream(sse))
      vi.stubGlobal('fetch', fetchMock)
      const store = new ChatStore()
      store.seed('c', [])
      await store.submit('c', 'go')
      return store.get('c').live!.dag!
    } finally {
      nowSpy.mockRestore()
    }
  }

  it('node_failed carries a server finished_at_ms that survives replay at a different client clock', async () => {
    const dagA = await runOneNode('node_failed', '{"node_id":"a","error":"boom","finished_at_ms":9000}', 10_000_000_000)
    const dagB = await runOneNode('node_failed', '{"node_id":"a","error":"boom","finished_at_ms":9000}', 20_000_000_000)
    expect(dagA.nodeStates['a'].status).toBe('failed')
    expect(dagA.nodeStates['a'].finishedAt).toBe(9000)
    expect(dagA.nodeStates['a'].finishedAt).toBe(dagB.nodeStates['a'].finishedAt)
  })

  it('node_cancelled carries a server finished_at_ms that survives replay at a different client clock', async () => {
    const dagA = await runOneNode('node_cancelled', '{"node_id":"a","finished_at_ms":9000}', 10_000_000_000)
    const dagB = await runOneNode('node_cancelled', '{"node_id":"a","finished_at_ms":9000}', 20_000_000_000)
    expect(dagA.nodeStates['a'].status).toBe('cancelled')
    expect(dagA.nodeStates['a'].finishedAt).toBe(9000)
    expect(dagA.nodeStates['a'].finishedAt).toBe(dagB.nodeStates['a'].finishedAt)
  })

  it('falls back to Date.now() for a node_done from an old server with no finished_at_ms', async () => {
    const before = Date.now()
    const dag = await runOneNode('node_done', '{"node_id":"a"}', Date.now())
    const after = Date.now()
    expect(dag.nodeStates['a'].finishedAt).toBeGreaterThanOrEqual(before)
    expect(dag.nodeStates['a'].finishedAt).toBeLessThanOrEqual(after)
  })
})

describe('ChatStore - context meter + compaction', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  let store: ChatStore

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    store = new ChatStore()
    store.seed('c', [])
  })

  it("a worker agent_complete's context_tokens becomes the node's live contextTokens reading", async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[],"context_window":262144}],"edges":[]}',
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"worker-r0","agent":"researcher","stage":"worker"}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"worker-r0","stage":"worker","context_tokens":156000}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    const state = store.get('c').live?.dag
    expect(state?.nodeStates['a'].contextTokens).toBe(156000)
    expect(state?.nodes[0].context_window).toBe(262144)
  })

  // A judge round's own context usage has nothing to do with the worker's
  // window - it must not overwrite the meter's reading.
  it('ignores context_tokens from a judge-stage agent_complete', async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"worker-r0","agent":"researcher","stage":"worker"}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"worker-r0","stage":"worker","context_tokens":100000}',
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"judge-r1","agent":"judge","stage":"judge","round":1}',
      '',
      'event: agent_complete',
      'data: {"node_id":"a","run_id":"judge-r1","stage":"judge","round":1,"context_tokens":9999}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    expect(store.get('c').live?.dag?.nodeStates['a'].contextTokens).toBe(100000)
  })

  it('records a compaction event on the run matching its exact run_id', async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: agent_start',
      'data: {"node_id":"a","run_id":"worker-r0","agent":"researcher","stage":"worker"}',
      '',
      'event: compaction',
      'data: {"node_id":"a","run_id":"worker-r0","summary_input_tokens":210000,"summary_output_tokens":1800}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    const run = store.get('c').live?.dag?.nodeRuns['a']?.find(r => r.runId === 'worker-r0')
    expect(run?.activity).toContainEqual({ kind: 'compaction', summaryInputTokens: 210000, summaryOutputTokens: 1800 })
  })
})

// #1114: artifact_revision/artifact_judge_round populate ChatState.artifactEvents
// and fan out through the existing subscribe() seam, so a late-mounted
// component (ArtifactPanel) not otherwise wired into chatStore can follow them.
describe('ChatStore - artifact live events (#1114)', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  let store: ChatStore

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    store = new ChatStore()
    store.seed('c', [])
  })

  it('records artifact_revision and artifact_judge_round, incrementing seq each time', async () => {
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: artifact_revision',
      'data: {"id":"text:plan","revision":1,"kind":"text","node_id":"a","round":1}',
      '',
      'event: artifact_judge_round',
      'data: {"id":"judge_round:t-a-1","passed":false,"score":0.4,"scored":[{"artifact_id":"text:plan","revision":1}]}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')

    const events = store.get('c').artifactEvents
    expect(events?.revision).toEqual({ id: 'text:plan', revision: 1, kind: 'text', nodeId: 'a', round: 1 })
    expect(events?.judgeRound).toEqual({ id: 'judge_round:t-a-1', passed: false, score: 0.4, scored: [{ artifactId: 'text:plan', revision: 1 }] })
    expect(events?.seq).toBe(2)
  })

  it('fans artifact events out through subscribe() to a listener not otherwise wired to the store', async () => {
    const listener = vi.fn()
    const unsubscribe = store.subscribe('c', listener)
    const sse = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: artifact_revision',
      'data: {"id":"text:plan","revision":1,"kind":"text","node_id":"a","round":1}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sse))
    await store.submit('c', 'go')
    await new Promise(r => setTimeout(r, 20)) // notify() coalesces to one rAF/setTimeout(16) tick
    expect(listener).toHaveBeenCalled()
    unsubscribe()
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

  it('pendingNodeQuestion matches the wire-normalized paused/awaiting_input spelling (post-reload)', () => {
    const d = dag({ a: { status: 'done' }, b: { status: 'paused', pauseReason: 'awaiting_input', question: 'Which time zone?' } })
    expect(pendingNodeQuestion(d)).toEqual({ nodeId: 'b', agent: 'synthesizer', question: 'Which time zone?' })
  })

  it('pendingNodeQuestion is undefined when no node is waiting', () => {
    expect(pendingNodeQuestion(dag({ a: { status: 'done' }, b: { status: 'running' } }))).toBeUndefined()
  })
})

describe('isTurnInProgress - re-subscribe gate', () => {
  it('is true only for a turn whose DAG is in_progress', () => {
    expect(isTurnInProgress(dagTurn('in_progress'))).toBe(true)
    expect(isTurnInProgress(dagTurn('completed'))).toBe(false)
    expect(isTurnInProgress(turnWith())).toBe(false) // no DAG (e.g. failed nodes after restart)
    expect(isTurnInProgress(undefined)).toBe(false)
  })
})

// Minimal EventSource stand-in: jsdom has none. Captures listeners so a test can
// feed the same SSE vocabulary the hub replays, and records close(). emit's
// optional `id` mirrors the SSE `id:` field - EventSource surfaces it on the
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

describe('ChatStore.attach - reconnect to a live run', () => {
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

  // #282: opening a chat while its node is actively running must show LIVE
  // activity (tool calls, streamed tokens) as it lands on the held-open
  // stream - not just the terminal node_start/done bookends.
  it('activity events (tool_call, token) landing on the held-open stream update the store live, no reload', () => {
    store.seed('c', [dagTurn('in_progress')])
    store.attach('c')
    const es = FakeEventSource.last!

    es.emit('dag_plan', '{"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}')
    es.emit('node_start', '{"node_id":"a","agent":"researcher"}')
    es.emit('agent_start', '{"node_id":"a","run_id":"r1","agent":"researcher","stage":"worker"}')

    es.emit('agent_tool_call', '{"node_id":"a","run_id":"r1","call_id":"tc1","name":"web_search","args":{"q":"x"}}')
    es.emit('agent_token', '{"node_id":"a","run_id":"r1","text":"partial answer text"}')

    const dag = store.get('c').live?.dag
    expect(dag?.nodeRuns['a']?.some(r => r.runId === 'r1')).toBe(true)
    expect(dag?.nodeAnswer['a']).toContain('partial answer text')
    expect(es.closed).toBe(false) // still streaming - no reload needed to see this
  })
})

// Issue #383: a dropped SSE connection must be retried automatically -
// resuming via Last-Event-ID - instead of tearing the run down and forcing a
// manual page refresh.
describe('ChatStore - reconnect on a dropped stream (#383)', () => {
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

      // Connection drops mid-run - no `done` was seen, so this must NOT tear
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

      // The node's prior state (from es1) is untouched - resuming replays only
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
    // EventSource fires `error` too once the server closes the connection -
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

  it('a POST stream that drops (no `done`, body just ends, no `id:` seen) hands off to the resumable GET stream at event 0 - nothing to resume past', async () => {
    const dropped = [
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'event: agent_token',
      'data: {"node_id":"a","run_id":"worker-r0","text":"partial answer"}',
      '',
      // no `done` - the body simply ends here, simulating a mid-run drop.
    ].join('\n')
    const encoder = new TextEncoder()
    const stream = new ReadableStream({ start(ctrl) { ctrl.enqueue(encoder.encode(dropped)); ctrl.close() } })
    const res = new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(res))

    await store.submit('c', 'go')

    // The turn is still live - handed off to the GET stream, not failed.
    expect(store.get('c').live?.streaming).toBe(true)
    const es = FakeEventSource.last!
    expect(es.url).toBe('/api/v1/chats/c/stream') // no `id:` line was seen on this path - full replay

    // The replayed dag_plan (a fresh plan) resets the stale top-level text
    // from the dropped body, same as any fresh dag_plan (#463) - no separate
    // pre-handoff reset is needed.
    es.emit('dag_plan', '{"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}', 1)
    expect(store.get('c').live?.text).toBe('')
    es.emit('node_done', '{"node_id":"a","output_preview":"final"}', 2)
    expect(store.get('c').live?.dag?.nodeStates['a'].status).toBe('done')

    es.emit('done', '{}', 3)
    expect(store.get('c').live?.streaming).toBe(false)
  })

  // #1090 perf audit item 4: the POST body carries `id:` lines on the wire
  // (same sseWriter as the GET stream) - a drop mid-run should resume past
  // whatever was already applied, not replay the run from 0.
  it('a POST stream that drops after seeing `id:` lines hands off to the GET stream past the last id applied', async () => {
    const dropped = [
      'id: 1',
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'id: 2',
      'event: node_start',
      'data: {"node_id":"a","agent":"researcher"}',
      '',
      // dropped before node_done/done arrive
    ].join('\n')
    const encoder = new TextEncoder()
    const stream = new ReadableStream({ start(ctrl) { ctrl.enqueue(encoder.encode(dropped)); ctrl.close() } })
    const res = new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(res))

    await store.submit('c', 'go')

    const es = FakeEventSource.last!
    expect(es.url).toBe('/api/v1/chats/c/stream?last_event_id=2')
    expect(store.get('c').live?.dag?.nodeStates['a'].status).toBe('running') // already applied, not lost

    es.emit('node_done', '{"node_id":"a","output_preview":"final"}', 3)
    expect(store.get('c').live?.dag?.nodeStates['a'].status).toBe('done')
    es.emit('done', '{}', 4)
    expect(store.get('c').live?.streaming).toBe(false)
  })

  // A drop can land between an `id:` line and its `data:` line - a real TCP
  // boundary, not a corner case. Resuming past an id whose event was never
  // actually dispatched would silently drop that event from the UI.
  it('a POST stream that drops right after an `id:` line (before its `data:` arrives) does not resume past the undelivered event', async () => {
    const dropped = [
      'id: 1',
      'event: dag_plan',
      'data: {"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}',
      '',
      'id: 2',
      'event: node_start',
      // dropped here - id 2's `data:` line never arrives.
    ].join('\n')
    const encoder = new TextEncoder()
    const stream = new ReadableStream({ start(ctrl) { ctrl.enqueue(encoder.encode(dropped)); ctrl.close() } })
    const res = new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(res))

    await store.submit('c', 'go')

    const es = FakeEventSource.last!
    expect(es.url).toBe('/api/v1/chats/c/stream?last_event_id=1') // not 2 - that event was never applied
  })
})

// #1090 perf audit item 4: a fresh attach still replays from 0 - a real page
// reload starts a new ChatStore with no memory of what this client already
// applied, so it has no cursor to resume from (a durable per-chat cursor
// needs a backend field; not implemented here). The POST-drop handoff
// (above) is the one case where this client DOES already know how far it
// got - see the two tests just above this comment.
describe('ChatStore.attach - a fresh attach has no cursor, replays from 0 (#1090)', () => {
  it('attach on a chat this client has never streamed opens /stream with no last_event_id', () => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null
    const store = new ChatStore()
    store.seed('c', [dagTurn('in_progress')])
    store.attach('c')
    expect(FakeEventSource.last!.url).toBe('/api/v1/chats/c/stream')
  })
})

// Issue #463: when a fresh dag_plan arrives on a LiveTurn that has accumulated
// top-level content (from pre-DAG orchestrator narration or replays into an old
// turn), the stale text/runs bleed into the new DAG scope.  The fix: onDagPlan
// also resets live.text and live.runs when creating a fresh DAG.
describe('ChatStore - fresh dag_plan resets top-level accumulators (#463)', () => {
  let store: ChatStore
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null
    store = new ChatStore()
  })

  it('a fresh dag_plan emitted into a live turn that has accumulated stale text + runs clears them', () => {
    store.seed('c', [dagTurn('in_progress')])
    store.attach('c')
    const es = FakeEventSource.last!

    // Pre-planning phase: orchestrator emits top-level text (no DAG yet).
    es.emit('agent_start', '{"run_id":"orchestrator","stage":"worker"}')
    es.emit('agent_token', '{"text":"PRE-DAG NARRATION"}')
    expect(store.get('c').live?.text).toBe('PRE-DAG NARRATION')
    expect(store.get('c').live?.runs).toHaveLength(1)

    // FINALLY a dag_plan arrives - signals a fresh DAG for a new run.
    es.emit('dag_plan', '{"plan_id":"p-new","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}')

    // #463: stale top-level content must be purged when replacing with a fresh DAG.
    expect(store.get('c').live?.text).toBe('')
    expect(store.get('c').live?.runs).toEqual([])
  })
})

describe('ChatStore - attach on idle chat fires live turn (#463)', () => {
  let store: ChatStore
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null
    store = new ChatStore()
  })

  // #463 (part 2): when a run goes active on an already-open chat, the
  // Chat.tsx useEffect fires attach - lifting any history turns into `live`
  // and opening the /stream subscribe so events start flowing.  Without this
  // path the chat box stays blank while the Running badge shows.
  it('attach called on idle chat lifts history into live and starts streaming', () => {
    store.seed('c', [dagTurn('in_progress')])
    expect(store.get('c').live).toBeUndefined()

    store.attach('c')
    const es = FakeEventSource.last!
    expect(es.url).toBe('/api/v1/chats/c/stream')
    expect(store.get('c').live?.streaming).toBe(true)
    // The in-progress turn was lifted out of history into `live`.
    expect(store.get('c').turns).toHaveLength(0)
    expect(store.get('c').live?.userText).toBe('hi')
  })

  it('a fresh dag_plan on this live stream creates a visible DAG with queued nodes', () => {
    store.seed('c', [dagTurn('in_progress')])
    store.attach('c')
    const es = FakeEventSource.last!

    es.emit('dag_plan', '{"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[]}')

    const dag = store.get('c').live?.dag
    expect(dag).toBeDefined()
    expect(dag?.nodeStates['a']?.status).toBe('queued')
    expect(es.closed).toBe(false) // still streaming
  })
})

// Issue #463 (part 3, live repro): the hub only publishes NEW events, so a client
// attaching after a run's events already fired gets no replay at all. attach()
// used to lift the in-progress turn into a BLANK `live` on that assumption -
// with nothing ever arriving to fill it, the whole pane rendered empty (not
// just that turn: earlier history stayed intact, but had nothing to show for
// the run everyone could see was "Running"). The fix: seed `live` from what
// GET /chats/{id} already persisted for that turn, so it renders immediately.
describe('ChatStore.attach - seeds live from persisted output when the hub replays nothing (#463)', () => {
  let store: ChatStore
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null
    store = new ChatStore()
  })

  it('renders the earlier completed turn and the in-progress turn\'s own DAG snapshot, with zero stream events emitted', () => {
    const done: Turn = {
      id: 't1', created_at: '', input: { role: 'user', content: 'first' },
      output: [{ id: 'm1', type: 'message', status: 'completed', content: [{ type: 'output_text', text: 'first answer' }] }],
    }
    const running: Turn = {
      id: 't2', created_at: '', input: { role: 'user', content: 'second' },
      output: [{
        type: 'quack:dag', id: 'p2', status: 'in_progress', plan_id: 'p2',
        nodes: [
          { id: 'a', agent: 'researcher', task: 't', depends_on: [] },
          { id: 'b', agent: 'synthesizer', task: 't2', depends_on: ['a'] },
        ],
        edges: [{ from: 'a', to: 'b' }],
        node_states: {
          a: { status: 'done', output_preview: 'node a result' },
          b: { status: 'running' },
        },
      }],
    }

    store.seed('c', [done, running])
    store.attach('c')  // no es.emit(...) at all - nothing is replayed

    const s = store.get('c')
    expect(s.turns).toEqual([done])           // earlier history is untouched
    expect(s.live?.streaming).toBe(true)
    expect(s.live?.dag?.nodeStates['a']).toMatchObject({ status: 'done', outputPreview: 'node a result' })
    expect(s.live?.dag?.nodeStates['b']?.status).toBe('running')
  })
})

// #1290: a page reload rebuilt a finished chat's DAG bubble from the persisted
// node_states rollup alone (dagTurnStateFromItem), which has no per-run
// breakdown - the judge/revise sub-run cards a live view showed vanished.
// attach() itself never gated on turn status; the caller (Chat.tsx's getChat
// effect) did. These guard the store half of the fix: attaching to a chat
// whose LAST turn is a completed (not just in_progress) DAG still replays
// chat_events and rebuilds nodeRuns through the same handlers the live path
// uses - one parser, no separate "history" reconstruction.
describe('ChatStore.attach - rebuilds a finished turn\'s sub-run cards from replay (#1290)', () => {
  let store: ChatStore
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    FakeEventSource.last = null
    store = new ChatStore()
  })

  // One judge/revise round, every event server-timestamped, replayed onto a
  // COMPLETED dag turn (not the in_progress fixture the other attach() tests use).
  function replayFinishedRound(es: FakeEventSource): void {
    es.emit('dag_plan', '{"plan_id":"p","nodes":[{"id":"a","agent":"researcher","task":"t","depends_on":[]}],"edges":[],"started_at_ms":1000}')
    es.emit('node_start', '{"node_id":"a","agent":"researcher","started_at_ms":2000}')
    es.emit('agent_start', '{"node_id":"a","run_id":"worker-r0","agent":"researcher","stage":"worker","started_at_ms":2000}')
    es.emit('agent_complete', '{"node_id":"a","run_id":"worker-r0","stage":"worker","finished_at_ms":8000}')
    es.emit('agent_start', '{"node_id":"a","run_id":"judge-r1","agent":"judge","stage":"judge","round":1,"started_at_ms":8000}')
    es.emit('agent_complete', '{"node_id":"a","run_id":"judge-r1","stage":"judge","round":1,"passed":false,"finished_at_ms":13000}')
    es.emit('agent_start', '{"node_id":"a","run_id":"worker-r1","agent":"researcher","stage":"revise","round":1,"started_at_ms":13000}')
    es.emit('agent_complete', '{"node_id":"a","run_id":"worker-r1","stage":"revise","round":1,"finished_at_ms":20000}')
    es.emit('agent_start', '{"node_id":"a","run_id":"judge-r2","agent":"judge","stage":"judge","round":2,"started_at_ms":20000}')
    es.emit('agent_complete', '{"node_id":"a","run_id":"judge-r2","stage":"judge","round":2,"passed":true,"finished_at_ms":25000}')
    es.emit('node_done', '{"node_id":"a","duration_ms":23000,"finished_at_ms":25000}')
    es.emit('done')
  }

  it('attach()-ing to a completed DAG turn replays and rebuilds worker/judge/revise cards', () => {
    store.seed('c', [dagTurn('completed')])
    store.attach('c')
    const es = FakeEventSource.last!
    expect(es.url).toBe('/api/v1/chats/c/stream') // fresh replay from seq 0, same as the in-progress path

    replayFinishedRound(es)

    const s = store.get('c')
    expect(s.live?.streaming).toBe(false) // done fired - it's history again, not a live run
    const runs = s.live?.dag?.nodeRuns['a'] ?? []
    expect(runs.map(r => r.stage)).toEqual(['worker', 'judge', 'revise', 'judge'])
    expect(runs.every(r => r.done)).toBe(true)
  })

  it('replaying the same finished round at two different client Date.now() values yields identical run durations (no timer drift across loads)', () => {
    function loadOnce(fetchNowMs: number): DagTurnState {
      const nowSpy = vi.spyOn(Date, 'now').mockReturnValue(fetchNowMs)
      try {
        vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
        FakeEventSource.last = null
        const s = new ChatStore()
        s.seed('c', [dagTurn('completed')])
        s.attach('c')
        replayFinishedRound(FakeEventSource.last!)
        return s.get('c').live!.dag!
      } finally {
        nowSpy.mockRestore()
      }
    }

    const dagA = loadOnce(10_000_000_000) // first page open
    const dagB = loadOnce(20_000_000_000) // reload, hours later

    const runsA = dagA.nodeRuns['a'].map(r => ({ runId: r.runId, durationMs: r.durationMs }))
    const runsB = dagB.nodeRuns['a'].map(r => ({ runId: r.runId, durationMs: r.durationMs }))
    expect(runsA).toEqual(runsB)
    expect(runsA.map(r => r.durationMs)).toEqual([6000, 5000, 7000, 5000])
    expect(dagA.nodeStates['a'].finishedAt).toBe(dagB.nodeStates['a'].finishedAt)
  })
})

// Issue #463 (part 2): confirm sequential submits already get clean state via archive path.
describe('ChatStore - submit already produces clean turns (#463)', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  let store: ChatStore

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    store = new ChatStore()
    store.seed('c', [])
  })

  it('creates fresh LiveTurn for each submit - prev text does not leak into next turn', async () => {
    const sseOld = [
      'event: agent_token',
      'data: {"text":"OLD ANSWER"}',
      '',
      'event: done\ndata: {}\n\n',
    ].join('\n')

    fetchMock.mockResolvedValueOnce(makeStream(sseOld))
    await store.submit('c', 'msg1')
    expect(store.get('c').live?.text).toBe('OLD ANSWER')
    expect(store.get('c').live?.streaming).toBe(false)

    // Next submit: archive GET + fresh LiveTurn.  Stale text must not carry forward.
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ turns: [] }), { status: 200 }))
    const sseNew = [
      'event: agent_token',
      'data: {"text":"NEW ANSWER"}',
      '',
    ].join('\n')
    fetchMock.mockResolvedValueOnce(makeStream(sseNew))
    await store.submit('c', 'msg2')

    expect(store.get('c').live?.text).toBe('NEW ANSWER')
  })
})
