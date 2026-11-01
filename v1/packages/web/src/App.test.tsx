import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { App } from './App.tsx';

class FakeSocket {
  static all: FakeSocket[] = [];
  sent: unknown[] = [];
  readyState = 1;
  static OPEN = 1;
  onopen?: () => void;
  onclose?: () => void;
  onmessage?: (e: { data: string }) => void;
  url: string;
  constructor(url: string) {
    this.url = url;
    FakeSocket.all.push(this);
  }
  send(data: string) {
    this.sent.push(JSON.parse(data));
  }
  close() {}
  open() {
    act(() => this.onopen?.());
  }
  frame(f: object) {
    act(() => this.onmessage?.({ data: JSON.stringify(f) }));
  }
}

const usage = {};
const entry = (id: string, model: object[]) => ({ id, conversationId: 'c', kind: 'x', model });
const snapshot = (over: object = {}) => ({ type: 'snapshot', entries: [], tools: [], nestedTools: [], compactions: [], inbox: [], agent: {}, usage, ...over });
const assistant = (content: object[]) => ({ role: 'assistant', content, api: 'a', provider: 'p', model: 'm', usage, stopReason: 'stop', timestamp: 1 });

// Desktop puts the session list in the sidebar beside the main column; a phone shows one at a time.
const stubViewport = (desktop: boolean) =>
  vi.stubGlobal('matchMedia', () => ({ matches: desktop, addEventListener() {}, removeEventListener() {} }));

let routes: Record<string, unknown>;
const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
  const hit = routes[`${init?.method ?? 'GET'} /${url}`];
  return new Response(JSON.stringify(hit ?? { error: 'not found' }), { status: hit ? 200 : 404 });
});

beforeEach(() => {
  FakeSocket.all = [];
  routes = { 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /sessions/ui%3Au1/artifacts': { artifacts: [] }, 'GET /sessions/ui%3Au1/approvals': { approvals: [] } };
  fetchMock.mockClear();
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal('fetch', fetchMock);
  vi.stubGlobal('crypto', { randomUUID: () => 'uuid-1' });
  stubViewport(true);
  location.hash = '';
});
afterEach(() => vi.unstubAllGlobals());

async function openSession() {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0];
  ws.open();
  return ws;
}

const sess = (key: string, over: object = {}) => ({ key, agent: 'reviewer', running: false, active: false, lastActivity: '2026-11-01T01:00:00.000Z', ...over });

test('groups sessions: a running session with a pending approval lands in Needs you', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { running: true, active: true, title: 'Fix the flake' }), sess('gh:2', { running: true, active: true }), sess('gh:3', { title: 'Old work' })] };
  routes['GET /sessions/gh%3A1/approvals'] = { approvals: [{ id: 'a', tool: 'push', args: {}, requestedAt: 1 }] };
  routes['GET /sessions/gh%3A2/approvals'] = { approvals: [] };
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByRole('link', { name: /Fix the flake/ })).toHaveAttribute('href', '#/s/gh%3A1');
  expect(within(needs).getByText('Approve push')).toBeInTheDocument();
  expect(within(screen.getByRole('region', { name: 'Running' })).getByRole('link', { name: /gh:2/ })).toBeInTheDocument();
  expect(within(screen.getByRole('region', { name: 'Recent' })).getByRole('link', { name: /Old work/ })).toHaveTextContent('reviewer');
  expect(screen.getByText('1 need you · 1 running')).toBeInTheDocument();
});

test('a GitHub key shows as #n with its title, and the full key stays as the tooltip', async () => {
  routes['GET /sessions'] = { sessions: [sess('github:fagerbergj/quack#1885', { title: 'Review the shell' })] };
  render(<App />);
  const link = await screen.findByRole('link', { name: /#1885 Review the shell/ });
  expect(link).toHaveAttribute('title', 'Review the shell\ngithub:fagerbergj/quack#1885');
});

test('review rows show their repo only when the list holds reviews from two repos', async () => {
  routes['GET /sessions'] = { sessions: [sess('github:a/one#1', { title: 'fix: first' }), sess('github:a/one#2', { title: 'fix: second' })] };
  const view = render(<App />);
  expect(await screen.findByRole('link', { name: /#1 First/ })).not.toHaveTextContent('a/one');
  view.unmount();
  routes['GET /sessions'] = { sessions: [sess('github:a/one#1', { title: 'fix: first' }), sess('github:b/two#2', { title: 'fix: second' })] };
  render(<App />);
  expect(await screen.findByRole('link', { name: /b\/two #2 Second/ })).toBeInTheDocument();
});

const indexed = (session: string, kind: string, passed: boolean | null, version = 1, sealedAt = 1) => ({ session, kind, version, verdict: passed === null ? null : { passed }, sealedAt });

test('each kind of session shows its own title and state, and search finds it by kind', async () => {
  routes['GET /sessions'] = { sessions: [
    sess('github:o/r#7', { title: 'Fix the cache', running: true, active: true }),
    sess('github:o/r#8', { title: 'Add retries' }),
    sess('github:o/r#9', { title: 'Drop the flag' }),
    sess('ui:r1', { agent: 'researcher', title: 'What changed in v4?' }),
    sess('sleeper:123:8:lineup', { agent: 'sleeper-lineup' }),
    sess('remarkable:doc1', { agent: 'remarkable-ingest', title: 'Campaign Notes' }),
    sess('ui:x', { agent: 'coder' }),
    sess('github:o/r#11', { title: 'Tie on version' }),
    sess('github:o/r#10', { running: true, title: 'Review pull request o/r#10 (label quack:review). Title: feat(v1): Sleeper waivers job' }),
  ] };
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  routes['GET /index/artifacts'] = { artifacts: [
    indexed('github:o/r#8', 'github.review', false, 1), indexed('github:o/r#8', 'github.review', true, 2),
    indexed('github:o/r#9', 'github.review', false),
    indexed('github:o/r#10', 'github.review', true),
    indexed('github:o/r#11', 'github.review', false, 1, 1), indexed('github:o/r#11', 'github.review', true, 1, 2),
    indexed('ui:r1', 'research.report', true),
    indexed('sleeper:123:8:lineup', 'sleeper.cards', true),
    indexed('remarkable:doc1', 'remarkable.transcript', false),
  ] };
  render(<App />);
  const row = async (name: RegExp) => within(await screen.findByRole('link', { name }));
  expect((await row(/#7 Fix the cache/)).getByText('Reviewing')).toBeInTheDocument();
  expect((await row(/#8 Add retries/)).getByText('Approved ✓')).toBeInTheDocument();
  expect((await row(/#9 Drop the flag/)).getByText('Checks failed')).toBeInTheDocument();
  expect((await row(/What changed/)).getByText('Report ✓')).toBeInTheDocument();
  expect((await row(/Lineup · wk 8/)).getByText('Card ready')).toBeInTheDocument();
  expect((await row(/Campaign Notes/)).getByText(/^Failed/)).toBeInTheDocument();
  expect((await row(/#11 Tie on version/)).getByText('Approved ✓')).toBeInTheDocument();
  const alive = await row(/#10 Sleeper waivers job/);
  expect(alive.getByText('Approved ✓')).toBeInTheDocument();
  expect(within(screen.getByRole('region', { name: 'Recent' })).getByRole('link', { name: /#10 Sleeper/ })).toBeInTheDocument();
  expect((await row(/ui:x/)).getByText('coder')).toBeInTheDocument();
  await userEvent.type(screen.getByRole('searchbox', { name: 'Search sessions' }), 'lineup');
  expect(screen.queryByRole('link', { name: /Campaign Notes/ })).toBeNull();
  expect(screen.getByRole('link', { name: /Lineup · wk 8/ })).toBeInTheDocument();
});

test('search and the agent filter narrow the list', async () => {
  routes['GET /sessions'] = { sessions: [sess('github:1', { title: 'Fix the flake' }), sess('ui:2', { agent: 'researcher', title: 'Compare sandboxes' })] };
  render(<App />);
  await screen.findByRole('link', { name: /Fix the flake/ });
  await userEvent.type(screen.getByRole('searchbox', { name: 'Search sessions' }), 'sandbox');
  expect(screen.queryByRole('link', { name: /Fix the flake/ })).toBeNull();
  expect(screen.getByRole('link', { name: /Compare sandboxes/ })).toBeInTheDocument();
  await userEvent.clear(screen.getByRole('searchbox', { name: 'Search sessions' }));
  await userEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Started by' }), 'GitHub');
  expect(screen.getByRole('link', { name: /Fix the flake/ })).toBeInTheDocument();
  expect(screen.queryByRole('link', { name: /Compare sandboxes/ })).toBeNull();
});

test('selecting a session opens it in the main pane and marks its row', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1', { title: 'Fix the flake' })] };
  render(<App />);
  await userEvent.click(await screen.findByRole('link', { name: /Fix the flake/ }));
  expect(await screen.findByRole('heading', { name: 'Fix the flake' })).toBeInTheDocument();
  expect(FakeSocket.all[0].url).toMatch(/\/sessions\/ui%3Au1\/ws$/);
  expect(screen.getByRole('link', { name: /Fix the flake/ })).toHaveAttribute('aria-current', 'true');
});

async function startFromLanding(text: string) {
  routes['GET /agents'] = { agents: [{ name: 'reviewer', version: 2 }] };
  render(<App />);
  await userEvent.type(await screen.findByRole('textbox', { name: 'Question or task' }), text);
  await userEvent.click(screen.getByRole('button', { name: 'Start' }));
  await waitFor(() => expect(FakeSocket.all[0]?.url).toMatch(/\/sessions\/ui%3Auuid-1\/ws\?agent=reviewer$/));
}

test('starting a session opens a ui: key with the chosen agent and sends the untouched first message once on open', async () => {
  await startFromLanding('review it');
  const sock = FakeSocket.all[0];
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('review it');
  expect(sock.sent).toEqual([]);
  sock.open();
  await waitFor(() => expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue(''));
  expect(sock.sent).toEqual([{ type: 'input', input: 'review it' }]);
});

test('a seed the user edited is not sent on open, and a socket that never opens leaves the text', async () => {
  await startFromLanding('review it');
  const sock = FakeSocket.all[0];
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('review it');
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), ' carefully');
  sock.open();
  expect(sock.sent).toEqual([]);
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('review it carefully');
});

test('renders the snapshot transcript, then streamed events', async () => {
  const ws = await openSession();
  ws.frame(snapshot({ entries: [entry('e1', [{ role: 'user', content: 'hi', timestamp: 1 }])] }));
  expect(screen.getByRole('article', { name: 'You' })).toHaveTextContent('hi');
  expect(screen.getByRole('status')).toHaveTextContent('Idle');

  ws.frame({ type: 'events', events: [{ type: 'run_start', inputs: [] }, { type: 'message_start', message: assistant([{ type: 'text', text: 'He' }]) }] });
  expect(screen.getByRole('status')).toHaveTextContent('Running');
  ws.frame({ type: 'events', events: [{ type: 'message_update', usage, changes: [{ type: 'text_delta', contentIndex: 0, delta: 'llo' }] }] });
  expect(screen.getByRole('article', { name: 'Assistant' })).toHaveTextContent('Hello');

  const call = { type: 'toolCall', id: 't1', name: 'bash', arguments: { cmd: 'ls' } };
  const final = entry('e2', [assistant([{ type: 'text', text: 'Hello' }, call])]);
  const result = entry('e3', [{ role: 'toolResult', toolCallId: 't1', toolName: 'bash', content: [{ type: 'text', text: 'a.txt' }], isError: false, timestamp: 2 }]);
  ws.frame({ type: 'events', events: [{ type: 'message_end', entry: final }, { type: 'entry_appended', entry: final }, { type: 'entry_appended', entry: result }, { type: 'run_end', inputs: [] }] });
  expect(screen.getAllByRole('article', { name: 'Assistant' })).toHaveLength(1);
  expect(screen.getByText('Tool call: bash').closest('details')).not.toHaveAttribute('open');
  expect(screen.getByText('Tool result: bash')).toBeInTheDocument();
  expect(screen.getByRole('status')).toHaveTextContent('Idle');
});

test('tool calls show a summary, expand to arguments and output, truncate long output, and mark errors', async () => {
  const ws = await openSession();
  const call = (id: string, name: string, args: object) => ({ type: 'toolCall', id, name, arguments: args });
  const result = (id: string, name: string, text: string, isError = false) =>
    entry(`r${id}`, [{ role: 'toolResult', toolCallId: id, toolName: name, content: [{ type: 'text', text }], isError, timestamp: 2 }]);
  ws.frame(snapshot({
    entries: [
      entry('a', [assistant([{ type: 'thinking', thinking: 'weighing options' }, { type: 'thinking', thinking: '' }, call('t1', 'web_search', { query: 'quack ducks', limit: 3 }), call('t2', 'bash', { cmd: 'false' })])]),
      result('t1', 'web_search', 'x'.repeat(2500)),
      result('t2', 'bash', 'exit 1\nboom', true),
    ],
  }));
  expect(screen.getAllByText('Thinking')).toHaveLength(1);
  expect(screen.getByText('weighing options').closest('details')).not.toHaveAttribute('open');

  const search = screen.getByText('Tool call: web_search').closest('details')!;
  expect(within(search.querySelector('summary')!).getByText('quack ducks')).toBeInTheDocument();
  expect(search).not.toHaveAttribute('open');
  await userEvent.click(within(search).getByText('Tool call: web_search'));
  expect(search).toHaveAttribute('open');
  expect(search).toHaveTextContent('"limit": 3');

  const out = screen.getByText('Tool result: web_search').closest('details')!;
  expect(within(out.querySelector('summary')!).getByText('2500 characters')).toBeInTheDocument();
  expect(out.querySelector('pre')!.textContent).toHaveLength(2003);
  await userEvent.click(within(out).getByRole('button', { name: 'Show all (2500 characters)' }));
  expect(out.querySelector('pre')!.textContent).toHaveLength(2500);

  const bad = screen.getByText('Tool result: bash (error)').closest('details')!;
  expect(within(bad.querySelector('summary')!).getByText('exit 1')).toBeInTheDocument();
  expect(bad.querySelector('pre')).toHaveTextContent('boom');
});

test('a call whose arguments were not valid JSON shows the parse error', async () => {
  const ws = await openSession();
  const call = { type: 'toolCall', id: 't9', name: 'bash', arguments: {}, argumentsError: 'Unexpected token } at 4' };
  ws.frame(snapshot({ entries: [entry('a', [assistant([call])])] }));
  const bad = screen.getByText('Tool call: bash (bad arguments)').closest('details')!;
  expect(bad.querySelector('pre')).toHaveTextContent('Unexpected token } at 4');
});

test('an idle send has no whenBusy; a busy send carries the chosen mode', async () => {
  const ws = await openSession();
  ws.frame(snapshot());
  expect(screen.queryByRole('group', { name: 'While the agent is working' })).toBeNull();
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'first');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  expect(ws.sent[0]).toEqual({ type: 'input', input: 'first' });

  ws.frame({ type: 'events', events: [{ type: 'run_start', inputs: [] }] });
  const group = screen.getByRole('group', { name: 'While the agent is working' });
  expect(within(group).getByRole('button', { name: 'Steer now' })).toHaveAttribute('aria-pressed', 'true');
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'turn left');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  await userEvent.click(within(group).getByRole('button', { name: 'Queue next' }));
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'then right');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  expect(ws.sent.slice(1)).toEqual([
    { type: 'input', input: 'turn left', whenBusy: 'steer' },
    { type: 'input', input: 'then right', whenBusy: 'followUp' },
  ]);
});

test('shows a server error frame', async () => {
  const ws = await openSession();
  ws.frame({ type: 'error', error: 'input is too long' });
  expect(screen.getByRole('alert')).toHaveTextContent('input is too long');
});

test('approving posts the verdict and clears the item', async () => {
  const pending = { id: 'a/1', tool: 'bash', args: { cmd: 'rm x' }, requestedAt: 1 };
  routes['GET /sessions/ui%3Au1/approvals'] = { approvals: [pending] };
  routes['POST /sessions/ui%3Au1/approvals/a%2F1'] = { resolved: true };
  const ws = await openSession();
  ws.frame(snapshot());
  const approve = await screen.findByRole('button', { name: 'Approve bash' });
  routes['GET /sessions/ui%3Au1/approvals'] = { approvals: [] };
  await userEvent.click(approve);
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Approve bash' })).toBeNull());
  const post = fetchMock.mock.calls.find(([, init]) => init?.method === 'POST')!;
  expect(JSON.parse(post[1]!.body as string)).toEqual({ decision: 'approve', by: 'web' });
});

test('an approval that appears mid-run shows up when tool activity arrives', async () => {
  const ws = await openSession();
  ws.frame(snapshot());
  routes['GET /sessions/ui%3Au1/approvals'] = { approvals: [{ id: 'b', tool: 'push', args: {}, requestedAt: 1 }] };
  ws.frame({ type: 'events', events: [{ type: 'tool_execution_start', toolCallId: 't', toolName: 'push', args: {} }] });
  expect(await screen.findByRole('button', { name: 'Reject push' })).toBeInTheDocument();
});

test('a malformed escape in the hash shows the sessions list instead of crashing', async () => {
  routes['GET /sessions'] = { sessions: [] };
  routes['GET /agents'] = { agents: [] };
  stubViewport(false);
  location.hash = '#/s/%zz';
  render(<App />);
  expect(await screen.findByRole('heading', { name: 'Sessions' })).toBeInTheDocument();
});

test('an assistant entry with only an errorMessage renders the error', async () => {
  const ws = await openSession();
  const err = entry('e9', [{ ...assistant([]), stopReason: 'error', errorMessage: 'Provider is not configured' }]);
  ws.frame(snapshot({ entries: [err] }));
  expect(screen.getByRole('article', { name: 'Assistant' })).toHaveTextContent('Error: Provider is not configured');
});

test('a send on a closed socket keeps the input and says so', async () => {
  routes['GET /sessions/ui%3Au1/approvals'] = { approvals: [] };
  const ws = await openSession();
  ws.frame(snapshot());
  ws.readyState = 3;
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'lost?');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  expect(ws.sent).toEqual([]);
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('lost?');
  expect(screen.getByRole('alert')).toHaveTextContent('Disconnected');
});

test('each list fetch reports its own failure', async () => {
  routes = {};
  render(<App />);
  expect(await screen.findByText(/Sessions: /)).toBeInTheDocument();
  expect(screen.getByText(/Agents: /)).toBeInTheDocument();
});

test('Abort shows only while a run is in progress and posts to the abort route', async () => {
  routes['POST /sessions/ui%3Au1/abort'] = { aborted: true };
  const ws = await openSession();
  ws.frame(snapshot());
  expect(screen.queryByRole('button', { name: 'Abort' })).toBeNull();
  ws.frame({ type: 'events', events: [{ type: 'run_start', inputs: [] }] });
  await userEvent.click(screen.getByRole('button', { name: 'Abort' }));
  expect(fetchMock).toHaveBeenCalledWith('sessions/ui%3Au1/abort', { method: 'POST' });
  ws.frame({ type: 'events', events: [{ type: 'run_end', inputs: [] }] });
  expect(screen.queryByRole('button', { name: 'Abort' })).toBeNull();
});

test('a failed abort is reported', async () => {
  const ws = await openSession();
  ws.frame(snapshot({ run: {} }));
  await userEvent.click(screen.getByRole('button', { name: 'Abort' }));
  expect(await screen.findByRole('alert')).toHaveTextContent('not found');
});

test('a dropped socket reconnects with backoff and the new snapshot replaces the transcript', () => {
  vi.useFakeTimers();
  try {
    location.hash = '#/s/ui%3Au1';
    render(<App />);
    const first = FakeSocket.all[0];
    first.open();
    first.frame(snapshot({ entries: [entry('e1', [{ role: 'user', content: 'hi', timestamp: 1 }])] }));
    act(() => first.onclose?.());
    expect(screen.getByRole('status')).toHaveTextContent('Reconnecting');

    act(() => void vi.advanceTimersByTime(999));
    expect(FakeSocket.all).toHaveLength(1);
    act(() => void vi.advanceTimersByTime(1));
    expect(FakeSocket.all).toHaveLength(2);

    // The second attempt fails too, so the delay doubles.
    act(() => FakeSocket.all[1].onclose?.());
    act(() => void vi.advanceTimersByTime(1999));
    expect(FakeSocket.all).toHaveLength(2);
    act(() => void vi.advanceTimersByTime(1));
    expect(FakeSocket.all).toHaveLength(3);

    const third = FakeSocket.all[2];
    third.open();
    third.frame(snapshot({ entries: [entry('e1', [{ role: 'user', content: 'hi', timestamp: 1 }]), entry('e2', [assistant([{ type: 'text', text: 'there' }])])] }));
    expect(screen.getByRole('status')).toHaveTextContent('Idle');
    expect(screen.getAllByRole('article', { name: 'You' })).toHaveLength(1);
    expect(screen.getAllByRole('article', { name: 'Assistant' })).toHaveLength(1);
  } finally {
    vi.useRealTimers();
  }
});

test('lists artifacts with their check status and failing criteria', async () => {
  const check = (criterion: string, id: string, met: number | null) => ({ criterion, id, met, notes: [] });
  routes['GET /sessions/ui%3Au1/artifacts'] = {
    artifacts: [
      { kind: 'github.review', version: 1, hash: 'h1', content: 'draft one', sealedAt: 1, verdict: { passed: false, gatesFailed: ['cites.lines'], score: 0.4, checks: [check('cites', 'lines', 0), check('tone', 'polite', 1), check('misc', 'na', null)] } },
      { kind: 'github.review', version: 2, hash: 'h2', content: 'final review', sealedAt: 2, verdict: { passed: true, gatesFailed: [], score: 1, checks: [check('cites', 'lines', 1)] } },
    ],
  };
  const ws = await openSession();
  ws.frame(snapshot());
  const section = await screen.findByRole('region', { name: 'Artifacts' });
  const items = within(section).getAllByRole('listitem');
  expect(items[0]).toHaveTextContent('github.review v1 sealed · checks failed');
  expect(items[0]).toHaveTextContent('Failing: cites.lines');
  expect(items[1]).toHaveTextContent('github.review v2 sealed · checked');
  expect(items[1]).not.toHaveTextContent('Failing');
  expect(screen.queryByRole('dialog')).toBeNull();
  await userEvent.click(within(items[1]).getByRole('button', { name: 'Open github.review v2' }));
  expect(within(screen.getByRole('dialog', { name: 'Work' })).getByText('final review')).toBeInTheDocument();
});
test('api and ws urls resolve under a /v1/ page path', async () => {
  const { socketUrl, listArtifacts, abortRun } = await import('./api.ts');
  history.replaceState(null, '', '/v1/');
  try {
    routes['GET /sessions/ui%3Au1/artifacts'] = { artifacts: [] };
    routes['POST /sessions/ui%3Au1/abort'] = {};
    await listArtifacts('ui:u1');
    await abortRun('ui:u1');
    const resolved = fetchMock.mock.calls.map(([url]) => new URL(url, location.href).pathname);
    expect(resolved).toEqual(['/v1/sessions/ui%3Au1/artifacts', '/v1/sessions/ui%3Au1/abort']);
    expect(socketUrl('ui:u1', 'reviewer')).toBe(`ws://${location.host}/v1/sessions/ui%3Au1/ws?agent=reviewer`);
  } finally {
    history.replaceState(null, '', '/');
  }
});

test('the rail links every area and marks the current one', async () => {
  routes['GET /sessions'] = { sessions: [] };
  routes['GET /agents'] = { agents: [] };
  render(<App />);
  const rail = screen.getAllByRole('navigation', { name: 'Main' })[0];
  expect(within(rail).getAllByRole('link').map((l) => l.getAttribute('href'))).toEqual(['#/', '#/inbox', '#/artifacts', '#/sleeper', '#/remarkable', '#/memory', '#/agents', '#/evals', '#/schedules']);
  expect(within(rail).getByRole('link', { name: 'Sessions' })).toHaveAttribute('aria-current', 'page');
  await userEvent.click(within(rail).getByRole('link', { name: 'Inbox' }));
  expect(await screen.findByRole('heading', { name: 'Inbox' })).toBeInTheDocument();
  expect(within(rail).getByRole('link', { name: 'Inbox' })).toHaveAttribute('aria-current', 'page');
  expect(within(rail).getByRole('link', { name: 'Sessions' })).not.toHaveAttribute('aria-current');
});

test('a session stays inside the Sessions rail entry', async () => {
  await openSession();
  const rail = screen.getAllByRole('navigation', { name: 'Main' })[0];
  expect(within(rail).getByRole('link', { name: 'Sessions' })).toHaveAttribute('aria-current', 'page');
});

test('the theme toggle flips the document theme', async () => {
  routes['GET /sessions'] = { sessions: [] };
  routes['GET /agents'] = { agents: [] };
  document.documentElement.dataset.theme = 'light';
  localStorage.clear();
  render(<App />);
  const toggle = () => screen.getAllByRole('button', { name: 'Day / night' })[0];
  expect(toggle()).toHaveAttribute('aria-pressed', 'false');
  await userEvent.click(toggle());
  expect(document.documentElement.dataset.theme).toBe('dark');
  expect(localStorage.getItem('quack:theme')).toBe('dark');
  expect(toggle()).toHaveAttribute('aria-pressed', 'true');
  await userEvent.click(toggle());
  expect(document.documentElement.dataset.theme).toBe('light');
});

test('assistant text renders as markdown with a table, highlighted code and citation chips; calls collapse into one card', async () => {
  const ws = await openSession();
  const md = '## Result\n\n| Server | Gain |\n| --- | --- |\n| vLLM | 1.9x |\n\nSee [S9] and `TP=4`.\n\n```ts\nconst x = 1;\n```';
  const call = (id: string, name: string) => ({ type: 'toolCall', id, name, arguments: { q: id } });
  const res = (id: string, name: string) => entry(`r${id}`, [{ role: 'toolResult', toolCallId: id, toolName: name, content: [{ type: 'text', text: 'ok' }], isError: false, timestamp: 4000 }]);
  ws.frame(snapshot({
    entries: [
      entry('u', [{ role: 'user', content: 'compare', timestamp: 1 }]),
      entry('a', [assistant([{ type: 'text', text: md }, call('c1', 'web_search'), call('c2', 'web_fetch'), call('c3', 'web_fetch')])]),
      res('c1', 'web_search'), res('c2', 'web_fetch'), res('c3', 'web_fetch'),
    ],
  }));
  const reply = screen.getByRole('article', { name: 'Assistant' });
  expect(within(reply).getByRole('heading', { name: 'Result' })).toBeInTheDocument();
  expect(within(reply).getByRole('table')).toHaveTextContent('vLLM');
  expect(reply.querySelector('.cite')).toHaveTextContent('S9');
  expect(reply.querySelector('pre code.hljs .hljs-keyword')).toHaveTextContent('const');
  expect(within(reply).getByText('3 calls · web_search ×1, web_fetch ×2')).toBeInTheDocument();
  expect(within(reply).getAllByText('4 s')).toHaveLength(1);
});

test('the Work drawer opens from the status line, shows report then checks, traps focus and closes on Esc', async () => {
  routes['GET /sessions/ui%3Au1/artifacts'] = {
    artifacts: [
      { kind: 'research.report', version: 2, hash: 'h2', content: '## Short answer\n\nGain is 1.9x [S9].', sealedAt: 2, verdict: { passed: false, gatesFailed: [], score: 0.5, checks: [{ criterion: 'citations', id: 'support', met: 0, notes: ['S7 does not mention TP'] }, { criterion: 'dated', id: 'claims', met: 1, notes: [] }] } },
    ],
  };
  const ws = await openSession();
  ws.frame(snapshot());
  expect(screen.queryByRole('dialog')).toBeNull();
  const open = await screen.findByRole('button', { name: 'Open research.report v2' });
  await userEvent.click(open);
  const work = screen.getByRole('dialog', { name: 'Work' });
  expect(within(work).getByRole('heading', { name: 'Short answer' })).toBeInTheDocument();
  expect(work).toHaveTextContent('v2');
  expect(within(work).getByRole('button', { name: 'Close' })).toHaveFocus();
  await userEvent.click(within(work).getByRole('tab', { name: 'Checks' }));
  expect(work).toHaveTextContent('citations.support');
  expect(work).toHaveTextContent('S7 does not mention TP');
  expect(within(work).getByText('Fail')).toBeInTheDocument();
  expect(within(work).getByText('Pass')).toBeInTheDocument();
  await userEvent.tab({ shift: true });
  await userEvent.tab({ shift: true });
  expect(work).toContainElement(document.activeElement as HTMLElement);
  await userEvent.keyboard('{Escape}');
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(open).toHaveFocus();
});

test('the Work tabs are wired for assistive tech and switch with the arrow keys', async () => {
  routes['GET /sessions/ui%3Au1/artifacts'] = { artifacts: [{ kind: 'research.report', version: 1, hash: 'h', content: 'body', sealedAt: 1, verdict: null }] };
  const ws = await openSession();
  ws.frame(snapshot());
  await userEvent.click(await screen.findByRole('button', { name: 'Open research.report v1' }));
  const report = screen.getByRole('tab', { name: 'Report' });
  const checks = screen.getByRole('tab', { name: 'Checks' });
  expect(report).toHaveAttribute('tabindex', '0');
  expect(checks).toHaveAttribute('tabindex', '-1');
  expect(screen.getByRole('tabpanel')).toHaveAttribute('aria-labelledby', report.id);
  expect(report).toHaveAttribute('aria-controls', screen.getByRole('tabpanel').id);
  report.focus();
  await userEvent.keyboard('{ArrowRight}');
  expect(checks).toHaveAttribute('aria-selected', 'true');
  expect(checks).toHaveFocus();
  expect(screen.getByRole('tabpanel')).toHaveAttribute('aria-labelledby', checks.id);
  await userEvent.keyboard('{ArrowLeft}');
  expect(report).toHaveFocus();
});

test('a malformed socket frame is skipped and later frames still apply', async () => {
  const ws = await openSession();
  ws.frame(snapshot());
  act(() => ws.onmessage?.({ data: '{not json' }));
  ws.frame({ type: 'events', events: [{ type: 'run_start', inputs: [] }] });
  expect(screen.getByRole('status')).toHaveTextContent('Running');
});

test('a trailing slash on #/new/ shows the landing page on phone and desktop alike', async () => {
  for (const desktop of [false, true]) {
    stubViewport(desktop);
    location.hash = '#/new/';
    const { unmount } = render(<App />);
    expect(await screen.findByRole('heading', { name: 'What should quack work on?' })).toBeInTheDocument();
    unmount();
  }
});

const pendingIndex = { approvals: [{ session: 'gh:3', id: 'a/1', tool: 'github_post_review', state: 'pending', decision: null, by: null, at: 1 }] };
const goTo = (hash: string) => {
  location.hash = hash;
  act(() => void dispatchEvent(new HashChangeEvent('hashchange')));
};

test('with the index on, an idle session waiting on approval is in Needs you and the Inbox', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Old work' })] };
  routes['GET /index/approvals?state=pending'] = pendingIndex;
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByText('Approve github_post_review')).toBeInTheDocument();
  expect(fetchMock.mock.calls.some(([u]) => u === 'sessions/gh%3A3/approvals')).toBe(false);
  goTo('#/inbox');
  expect(await screen.findByRole('heading', { name: 'Inbox' })).toBeInTheDocument();
  expect(screen.getByText('1 waiting for your approval')).toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'Old work' })).toHaveAttribute('href', '#/s/gh%3A3');
});

test('the Inbox posts approve to the owning session, and reject with its reason', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3')] };
  routes['GET /index/approvals?state=pending'] = pendingIndex;
  routes['POST /sessions/gh%3A3/approvals/a%2F1'] = { resolved: true };
  location.hash = '#/inbox';
  render(<App />);
  const post = () => JSON.parse(fetchMock.mock.calls.filter(([, i]) => i?.method === 'POST').at(-1)![1]!.body as string);
  await userEvent.click(await screen.findByRole('button', { name: 'Approve github_post_review' }));
  expect(post()).toEqual({ decision: 'approve', by: 'web' });

  await userEvent.click(screen.getByRole('button', { name: 'Reject github_post_review' }));
  await userEvent.type(screen.getByRole('textbox', { name: 'Reason' }), 'wrong repo');
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  await userEvent.click(screen.getByRole('button', { name: 'Confirm reject github_post_review' }));
  expect(post()).toEqual({ decision: 'reject', by: 'web', reason: 'wrong repo' });
  await screen.findByText('Nothing is waiting on you');
});

const pendingDecision = { decisions: [{ session: 'gh:3', step: 'type', question: 'What kind of document is this?', options: ['meeting', 'idea'], evidence: 'Agenda: standup', state: 'pending', choice: null, by: null, at: 1 }] };

test('the Inbox shows a workflow question with its options, and an option answers it', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Campaign Notes' })] };
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  routes['GET /index/decisions?state=pending'] = pendingDecision;
  routes['POST /sessions/gh%3A3/decisions/type'] = { answered: true };
  location.hash = '#/inbox';
  render(<App />);
  expect(await screen.findByText('What kind of document is this?')).toBeInTheDocument();
  expect(screen.getByText('1 waiting for your answer')).toBeInTheDocument();
  expect(screen.getByText('Agenda: standup')).not.toBeVisible();
  expect(screen.getByRole('link', { name: 'Campaign Notes' })).toHaveAttribute('href', '#/s/gh%3A3');
  routes['GET /index/decisions?state=pending'] = { decisions: [] };
  await userEvent.click(screen.getByRole('button', { name: 'meeting' }));
  expect(JSON.parse(fetchMock.mock.calls.filter(([, i]) => i?.method === 'POST').at(-1)![1]!.body as string)).toEqual({ choice: 'meeting', by: 'web' });
  await screen.findByText('Nothing is waiting on you');
});

test('a session waiting on a question is in Needs you, and its view shows the question card', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1')] };
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  routes['GET /index/decisions?state=pending'] = { decisions: pendingDecision.decisions.map((d) => ({ ...d, session: 'ui:u1' })) };
  routes['GET /sessions/ui%3Au1/decisions'] = { decisions: [{ step: 'type', question: 'What kind of document is this?', options: ['meeting', 'idea'], evidence: null, askedAt: 1 }] };
  await openSession();
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByText('Answer: What kind of document is this?')).toBeInTheDocument();
  const card = await screen.findByRole('region', { name: 'Decision' });
  expect(within(card).getByText('NEEDS YOU')).toBeInTheDocument();
  expect(within(card).getByRole('button', { name: 'idea' })).toBeInTheDocument();
});

test('with the index off, the Inbox falls back to the running sessions own approvals', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { running: true, active: true })] };
  routes['GET /sessions/gh%3A1/approvals'] = { approvals: [{ id: 'a', tool: 'push', args: { branch: 'main' }, requestedAt: 1 }] };
  location.hash = '#/inbox';
  render(<App />);
  expect(await screen.findByText('{"branch":"main"}')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Approve push' })).toBeInTheDocument();
});

test('Artifacts lists sealed versions from the index and opens one in the Work drawer', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Old work' })] };
  routes['GET /index/artifacts'] = { artifacts: [
    { session: 'gh:3', kind: 'github.review', version: 2, hash: 'h', verdict: { passed: true }, sealedAt: 2 },
    { session: 'gh:3', kind: 'github.review', version: 1, hash: 'g', verdict: null, sealedAt: 1 },
  ] };
  routes['GET /sessions/gh%3A3/artifacts'] = { artifacts: [
    { kind: 'github.review', version: 1, hash: 'g', content: 'first draft', sealedAt: 1, verdict: null },
    { kind: 'github.review', version: 2, hash: 'h', content: 'final review', sealedAt: 2, verdict: { passed: true, gatesFailed: [], score: 1, checks: [] } },
  ] };
  location.hash = '#/artifacts';
  render(<App />);
  const open = await screen.findByRole('button', { name: 'Open github.review v2 of Old work' });
  expect(open).toHaveTextContent('Passed');
  expect(open).toHaveTextContent('2 versions');
  expect(screen.getAllByRole('button', { name: /^Open / })).toHaveLength(1);
  await userEvent.click(open);
  expect(await screen.findByText('final review')).toBeInTheDocument();
  await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Version' }), '1');
  expect(screen.getByText('first draft')).toBeInTheDocument();
  expect(screen.getByRole('dialog', { name: 'Work' })).toBeInTheDocument();
});

test('Artifacts says so when the index is off', async () => {
  location.hash = '#/artifacts';
  render(<App />);
  expect(await screen.findByText(/The index is off/)).toBeInTheDocument();
});

test('a failed load shows its error in the Inbox instead of an empty list', async () => {
  delete routes['GET /sessions'];
  location.hash = '#/inbox';
  render(<App />);
  expect(await screen.findAllByRole('alert')).not.toHaveLength(0);
  expect(screen.queryByText('Nothing is waiting on you')).toBeNull();
});

test('a double-click on Approve posts once', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3')] };
  routes['GET /index/approvals?state=pending'] = pendingIndex;
  location.hash = '#/inbox';
  render(<App />);
  const approve = await screen.findByRole('button', { name: 'Approve github_post_review' });
  // The decision stays in flight, as it does while the session wakes.
  fetchMock.mockImplementationOnce(() => new Promise<Response>(() => {}));
  await userEvent.dblClick(approve);
  expect(fetchMock.mock.calls.filter(([, i]) => i?.method === 'POST')).toHaveLength(1);
});

const KEY = 'sleeper:L1:8:lineup';
const player = (name: string, over: object = {}) => ({ id: name, name, pos: 'WR', team: 'DET', inj: null, ...over });
const row = (name: string, over: object = {}) => ({ slot: 'WR1', player: player(name), proj: 12.34, action: 'keep', locked: false, replaces: null, game: { opponent: 'MIN', home: true, kickoff: '2026-11-01T18:00:00.000Z', locked: false, status: 'pre_game' }, why: `Why ${name}`, news: [], ...over });
const lineup = (over: object = {}) => ({ kind: 'lineup', league: 'L1', week: 8, team: 'Substation', opponent: 'Shore69', my_proj: 118.8, opp_proj: 130.5, summary: 'Start Allen over Concepcion.', bench: [{ player: player('Benchy'), proj: 3 }],
  starters: [row('Jefferson'), row('Allen', { slot: 'FLEX', action: 'start', replaces: player('Concepcion'), news: [{ title: 'Practice report', url: 'https://nfl.com/x', date: '2026-10-30', player: 'Concepcion' }] })], ...over });
const sealed = (card: object, verdict: object | null, version = 1) => ({ kind: 'sleeper.cards', version, hash: 'h', content: JSON.stringify(card), sealedAt: 1, verdict });

async function openSleeper(artifacts: object[], sessionOver: object = {}) {
  routes['GET /apps/sleeper'] = { league: 'L1' };
  routes['GET /sessions'] = { sessions: [sess(KEY, { agent: 'sleeper-lineup', ...sessionOver })] };
  routes[`GET /sessions/${encodeURIComponent(KEY)}/artifacts`] = { artifacts };
  routes[`GET /sessions/${encodeURIComponent(KEY)}/approvals`] = { approvals: [] };
  location.hash = '#/sleeper';
  render(<App />);
}

test('Sleeper leads with the week strip and renders a passed lineup card', async () => {
  await openSleeper([sealed(lineup(), { passed: true, gatesFailed: [], score: 1, checks: [] })]);
  const strip = await screen.findByRole('region', { name: 'This week' });
  expect(within(strip).getByText(/Lineup · week 8 · locks Sun/)).toBeInTheDocument();
  expect(within(strip).getByText('checked')).toBeInTheDocument();
  expect(within(screen.getByRole('article', { name: 'Week 8 lineup' })).getByText('v1 · checked')).toBeInTheDocument();
  const card = screen.getByRole('article', { name: 'Week 8 lineup' });
  expect(within(card).getByText('Allen')).toBeInTheDocument();
  expect(within(card).getByText('START')).toBeInTheDocument();
  expect(within(card).getByText('Why Allen')).toBeInTheDocument();
  expect(within(card).getByText(/replaces Concepcion/)).toBeInTheDocument();
  const news = within(card).getByText('News (1)').closest('details')!;
  expect(within(news).getByRole('link', { name: /Practice report/ })).toHaveAttribute('href', 'https://nfl.com/x');
  expect(news).toHaveTextContent('nfl.com · 2026-10-30');
  expect(within(card).getByRole('link', { name: 'Ask about this card' })).toHaveAttribute('href', `#/s/${encodeURIComponent(KEY)}`);
});

test('a failed lineup card names its failing checks', async () => {
  const failed = { passed: false, gatesFailed: [], score: 0.4, checks: [{ criterion: 'gates', id: 'summary_names_changes', met: 0, notes: ['Summary says nothing about the swap'] }] };
  await openSleeper([sealed(lineup(), null, 1), sealed(lineup(), failed, 2)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(within(card).getByText('v2 · checks failed')).toBeInTheDocument();
  const check = within(card).getByText('Summary names changes');
  expect(check).toHaveAttribute('title', 'gates.summary_names_changes');
  expect(check.closest('details')).toHaveTextContent('Summary says nothing about the swap');
});

test('a card with no changes shows the calm banner, not the change summary', async () => {
  await openSleeper([sealed(lineup({ summary: 'No changes.', starters: [row('Jefferson')] }), null)]);
  expect(await screen.findAllByText('No changes: your lineup stands')).toHaveLength(2);
});

test('a failed verdict with no failing check shows the judge tally', async () => {
  const checks = [{ criterion: 'gates', id: 'a', met: 1, notes: [] }, { criterion: 'pitfalls', id: 'b', met: 1, notes: [] }, { criterion: 'x', id: 'c', met: null, notes: [] }];
  await openSleeper([sealed(lineup(), { passed: false, gatesFailed: [], score: 0.7, checks })]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(within(card).getByText('Judge: 2 of 2 checks passed')).toBeInTheDocument();
});

test('a failing check with no judge note says so', async () => {
  const failed = { passed: false, gatesFailed: [], score: 0.4, checks: [{ criterion: 'gates', id: 'summary_names_changes', met: 0, notes: [] }] };
  await openSleeper([sealed(lineup(), failed)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(within(card).getByText('Summary names changes').closest('details')).toHaveTextContent('No note from the judge.');
});

test('an unchecked card is labelled not checked', async () => {
  await openSleeper([sealed(lineup(), null)]);
  expect(await screen.findAllByText('v1 · not checked')).not.toHaveLength(0);
});

test('Run now posts to the distributor route, which carries no token', async () => {
  await openSleeper([]);
  routes['POST /apps/sleeper/lineup'] = { sessionKey: KEY };
  await userEvent.click(await screen.findByRole('button', { name: 'Run now' }));
  expect(await screen.findByRole('status')).toHaveTextContent('Started');
  const post = fetchMock.mock.calls.find(([u, i]) => u === 'apps/sleeper/lineup' && i?.method === 'POST');
  expect(post?.[1]?.headers).toBeUndefined();
});

test('Run now is off while the job runs', async () => {
  await openSleeper([], { running: true });
  expect(await screen.findByRole('button', { name: 'Run now' })).toBeDisabled();
  expect(screen.getByText('Running now')).toBeInTheDocument();
});

test('a refused Run now shows the reason', async () => {
  await openSleeper([]);
  await userEvent.click(await screen.findByRole('button', { name: 'Run now' }));
  expect(await screen.findByRole('status')).toHaveTextContent('Could not start: not found');
});

test('without the Sleeper route the page says it is not set up, whatever sessions exist', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1'), sess('github:a/b#1')] };
  location.hash = '#/sleeper';
  render(<App />);
  expect(await screen.findByText('Sleeper is not set up on this server.')).toBeInTheDocument();
});
