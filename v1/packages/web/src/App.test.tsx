import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { App } from './App.tsx';
import { statusOf } from './ArtifactList.tsx';
import { forgetVersion } from './api.ts';

const requestOf = (sock: { sent: unknown[] }, i: number) => (sock.sent.at(i) as { requestId: string }).requestId;
const accept = (sock: FakeSocket, i = -1) => sock.frame({ type: 'accepted', requestId: requestOf(sock, i) });
const reject = (sock: FakeSocket, error: string, i = -1) => sock.frame({ type: 'error', error, requestId: requestOf(sock, i) });

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
const item = (e: object) => ({ kind: 'entry', entry: e });
// The conversation view as the socket sends it; omitted `entries` leave the transcript the client holds as it is.
const view = (o: { entries?: object[]; run?: boolean | object; message?: object; tools?: object[]; usage?: object } = {}) => ({
  type: 'view',
  items: o.entries?.map(item),
  live: { ...(o.run ? { run: { taskId: 1, inputs: [], turns: 0 } } : {}), ...(o.message ? { generation: { attempt: 1, message: o.message } } : {}), ...(o.tools ? { tools: o.tools } : {}) },
  usage: o.usage ?? usage,
});
const snapshot = (o: Parameters<typeof view>[0] = {}) => view({ entries: [], ...o });
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
  forgetVersion();
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

const sess = (key: string, over: object = {}) => ({ key, agent: 'reviewer', running: false, active: false, createdAt: '2026-11-01T01:00:00.000Z', lastActivity: '2026-11-01T01:00:00.000Z', ...over });

test('the list keeps the server order and labels rows by creation when an older session becomes active', async () => {
  const older = sess('gh:old', { title: 'Older', running: true, active: true, createdAt: '2026-11-01T01:00:00.000Z', lastActivity: '2026-11-01T09:00:00.000Z' });
  routes['GET /sessions'] = { sessions: [sess('gh:a', { title: 'Alpha', running: true, active: true, createdAt: '2026-11-01T03:00:00.000Z' }), sess('gh:new', { title: 'Newer', createdAt: '2026-11-01T02:00:00.000Z' }), older, sess('gh:z', { title: 'Zed', createdAt: '2026-11-01T00:00:00.000Z' })] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const running = await screen.findByRole('region', { name: 'Running' });
  expect(within(running).getAllByRole('link').map((l) => l.getAttribute('href'))).toEqual(['#/s/gh%3Aa', '#/s/gh%3Aold']);
  expect(within(screen.getByRole('region', { name: 'Recent' })).getAllByRole('link').map((l) => l.getAttribute('href'))).toEqual(['#/s/gh%3Anew', '#/s/gh%3Az']);
  expect(within(running).getByRole('link', { name: /Older/ }).querySelector('time')).toHaveAttribute('datetime', '2026-11-01T01:00:00.000Z');
});

test('groups sessions: a running session with a pending approval lands in Needs you', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { running: true, active: true, title: 'Fix the flake' }), sess('gh:2', { running: true, active: true }), sess('gh:3', { title: 'Old work' })] };
  routes['GET /sessions/gh%3A1/approvals'] = { approvals: [{ id: 'a', tool: 'push', args: {}, requestedAt: 1 }] };
  routes['GET /sessions/gh%3A2/approvals'] = { approvals: [] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByRole('link', { name: /Fix the flake/ })).toHaveAttribute('href', '#/s/gh%3A1');
  expect(within(needs).getByText('Waiting for your approval')).toBeInTheDocument();
  expect(within(screen.getByRole('region', { name: 'Running' })).getByRole('link', { name: /gh:2/ })).toBeInTheDocument();
  expect(within(screen.getByRole('region', { name: 'Recent' })).getByRole('link', { name: /Old work/ })).toHaveTextContent('reviewer');
});

test('a GitHub key shows as #n with its title, and the full key stays as the tooltip', async () => {
  routes['GET /sessions'] = { sessions: [sess('github:fagerbergj/quack#1885', { title: 'Review the shell' })] };
  render(<App />);
  const link = await screen.findByRole('link', { name: /#1885 Review the shell/ });
  expect(link).toHaveAttribute('title', 'Review the shell\ngithub:fagerbergj/quack#1885');
});

test('review rows carry their PR state from one lookup, and read plainly when the service is not connected', async () => {
  const keys = encodeURIComponent('github:a/one#1,github:a/one#2,github:a/one#3');
  routes['GET /sessions'] = { sessions: [sess('github:a/one#1', { title: 'fix: first' }), sess('github:a/one#2', { title: 'fix: second' }), sess('github:a/one#3', { title: 'fix: third' }), sess('gh:9', { title: 'Plain' })] };
  routes[`GET /apps/github/pulls?keys=${keys}`] = { pulls: { 'a/one#1': { state: 'open', at: '2026-11-01T00:00:00Z' }, 'a/one#2': { state: 'merged', at: '2026-11-02T00:00:00Z' } } };
  const view = render(<App />);
  const row = async (name: RegExp) => await screen.findByRole('link', { name });
  await waitFor(async () => expect(await row(/#2 Second/)).toHaveTextContent('Merged'));
  expect(await row(/#1 First/)).toHaveTextContent('Open');
  expect(await row(/#3 Third/)).not.toHaveTextContent(/Open|Merged|Closed/);
  expect(await row(/Plain/)).not.toHaveTextContent(/Open|Merged|Closed/);
  expect(fetchMock.mock.calls.filter(([u]) => String(u).startsWith('apps/github/pulls'))).toHaveLength(1);
  view.unmount();
  delete routes[`GET /apps/github/pulls?keys=${keys}`];
  render(<App />);
  expect(await row(/#1 First/)).not.toHaveTextContent(/Open|Merged|Closed/);
});

test('a closed PR reads Closed on its row', async () => {
  routes['GET /sessions'] = { sessions: [sess('github:a/one#1', { title: 'fix: first' })] };
  routes[`GET /apps/github/pulls?keys=${encodeURIComponent('github:a/one#1')}`] = { pulls: { 'a/one#1': { state: 'closed', at: '2026-11-01T00:00:00Z' } } };
  render(<App />);
  await waitFor(async () => expect(await screen.findByRole('link', { name: /#1 First/ })).toHaveTextContent('Closed'));
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

const indexed = (session: string, kind: string, passed: boolean | null, version = 1, sealedAt = 1, extra: { outcome?: string; posted?: boolean } = {}) =>
  ({ session, kind, version, verdict: passed === null ? null : { passed }, sealedAt, outcome: extra.outcome, links: extra.posted ? [{ label: 'Posted', url: `https://github.com/${session}/review-${version}` }] : [] });

test('each kind of session shows its own title and state, and search finds it by kind', async () => {
  routes['GET /sessions'] = { sessions: [
    sess('github:o/r#7', { title: 'Fix the cache', running: true, active: true }),
    sess('github:o/r#8', { title: 'Add retries' }),
    sess('github:o/r#9', { title: 'Drop the flag' }),
    sess('ui:r1', { agent: 'researcher', title: 'What changed in v4?' }),
    sess('sleeper:123:8:lineup', { agent: 'sleeper-lineup' }),
    sess('sleeper:123:8:waivers', { agent: 'sleeper-waivers' }),
    sess('sleeper:123:8:history', { agent: 'sleeper-x' }),
    sess('sleeper:123:8:trends', { agent: 'sleeper-x' }),
    sess('sleeper:123:8:trade-finder', { agent: 'sleeper-x' }),
    sess('sleeper:123:8:trade:4:12+13:99', { agent: 'sleeper-x' }),
    sess('sleeper:123:8:digest', { agent: 'sleeper-x' }),
    sess('sleeper:123:8:retro', { agent: 'sleeper-x' }),
    sess('remarkable:doc1', { agent: 'remarkable-ingest', title: 'Campaign Notes' }),
    sess('ui:x', { agent: 'coder' }),
    sess('github:o/r#11', { title: 'Tie on version' }),
    sess('github:o/r#10', { running: true, title: 'Review pull request o/r#10 (label quack:review). Title: feat(v1): Sleeper waivers job' }),
  ] };
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  routes['GET /index/artifacts'] = { artifacts: [
    indexed('github:o/r#8', 'github.review', false, 1), indexed('github:o/r#8', 'github.review', true, 2, 2, { outcome: 'approve', posted: true }),
    indexed('github:o/r#9', 'github.review', false),
    indexed('github:o/r#10', 'github.review', true, 1, 1, { outcome: 'approve', posted: true }),
    indexed('github:o/r#11', 'github.review', false, 1, 1), indexed('github:o/r#11', 'github.review', true, 1, 2, { outcome: 'approve', posted: true }),
    indexed('ui:r1', 'research.report', true),
    indexed('sleeper:123:8:lineup', 'sleeper.cards', true),
    indexed('sleeper:123:8:waivers', 'sleeper.cards', true),
    indexed('sleeper:123:8:history', 'sleeper.cards', true),
    indexed('sleeper:123:8:trends', 'sleeper.cards', true),
    indexed('sleeper:123:8:trade-finder', 'sleeper.cards', true),
    indexed('sleeper:123:8:trade:4:12+13:99', 'sleeper.cards', true),
    indexed('sleeper:123:8:digest', 'sleeper.cards', true),
    indexed('sleeper:123:8:retro', 'sleeper.cards', true),
    indexed('remarkable:doc1', 'remarkable.transcript', false),
  ] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const row = async (name: RegExp) => within(await screen.findByRole('link', { name }));
  expect((await row(/#7 Fix the cache/)).getByText('Reviewing')).toBeInTheDocument();
  expect((await row(/#8 Add retries/)).getByText('Approved ✓')).toBeInTheDocument();
  expect((await row(/#9 Drop the flag/)).getByText('No review posted')).toBeInTheDocument();
  expect((await row(/What changed/)).getByText('Report ✓')).toBeInTheDocument();
  expect((await row(/Lineup · wk 8/)).getByText('Lineup picks ready')).toBeInTheDocument();
  expect((await row(/Waivers · wk 8/)).getByText('Waiver claims ready')).toBeInTheDocument();
  expect((await row(/^History/)).getByText('League history ready')).toBeInTheDocument();
  expect((await row(/^Trends · wk 8/)).getByText('Trends ready')).toBeInTheDocument();
  expect((await row(/^Trade finder · wk 8/)).getByText('Trade ideas ready')).toBeInTheDocument();
  expect((await row(/^Trade · wk 8/)).getByText('Trade ideas ready')).toBeInTheDocument();
  expect((await row(/^Recap · wk 8/)).getByText('Week 8 recap ready')).toBeInTheDocument();
  expect((await row(/^Retro · wk 8/)).getByText('Week 8 retro ready')).toBeInTheDocument();
  expect((await row(/Campaign Notes/)).getByText(/^Didn't pass its checks/)).toBeInTheDocument();
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

async function startFromHome(text: string) {
  render(<App />);
  await userEvent.type(await screen.findByRole('textbox', { name: 'What do you need?' }), text);
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  await waitFor(() => expect(FakeSocket.all[0]?.url).toMatch(/\/sessions\/home%3Amain\/ws\?agent=quack$/));
}

test('starting a session opens a home: key for quack and sends the untouched first message once on open', async () => {
  await startFromHome('review it');
  const sock = FakeSocket.all[0];
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('review it');
  expect(sock.sent).toEqual([]);
  sock.open();
  await waitFor(() => expect(sock.sent).toHaveLength(1));
  accept(sock);
  await waitFor(() => expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue(''));
  expect(sock.sent).toEqual([{ type: 'input', requestId: expect.any(String), input: 'review it', whenBusy: 'followUp' }]);
});

test('two Home sends go to the same home:main session, queued as follow-ups', async () => {
  await startFromHome('first');
  FakeSocket.all[0].open();
  await waitFor(() => expect(FakeSocket.all[0].sent).toEqual([{ type: 'input', requestId: expect.any(String), input: 'first', whenBusy: 'followUp' }]));
  goTo('#/');
  await userEvent.type(await screen.findByRole('textbox', { name: 'What do you need?' }), 'second');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  await waitFor(() => expect(FakeSocket.all).toHaveLength(2));
  const urls = FakeSocket.all.map((w) => w.url.replace(/\?.*/, ''));
  expect(new Set(urls).size).toBe(1);
  expect(urls[0]).toMatch(/home%3Amain\/ws$/);
});

test('a seed the user edited is not sent on open, and a socket that never opens leaves the text', async () => {
  await startFromHome('review it');
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

  ws.frame(view({ run: true, message: assistant([{ type: 'text', text: 'He' }]) }));
  expect(screen.getByRole('status')).toHaveTextContent('Running');
  ws.frame(view({ run: true, message: assistant([{ type: 'text', text: 'Hello' }]) }));
  expect(screen.getByRole('article', { name: 'Assistant' })).toHaveTextContent('Hello');

  const call = { type: 'toolCall', id: 't1', name: 'bash', arguments: { cmd: 'ls' } };
  const final = entry('e2', [assistant([{ type: 'text', text: 'Hello' }, call])]);
  const result = entry('e3', [{ role: 'toolResult', toolCallId: 't1', toolName: 'bash', content: [{ type: 'text', text: 'a.txt' }], isError: false, timestamp: 2 }]);
  ws.frame(view({ entries: [entry('e1', [{ role: 'user', content: 'hi', timestamp: 1 }]), final, result] }));
  expect(screen.getAllByRole('article', { name: 'Assistant' })).toHaveLength(1);
  expect(screen.getByText('Worked · 1 tool · bash').closest('details')).not.toHaveAttribute('open');
  expect(screen.getByRole('status')).toHaveTextContent('Idle');
});

test('a running tool call shows as running with its arguments, and its step in plain words', async () => {
  const ws = await openSession();
  const call = { type: 'toolCall', id: 't1', name: 'read', arguments: { path: 'a/b/notes.md' } };
  ws.frame(view({ entries: [entry('a', [assistant([call])])], run: true, tools: [{ callId: 't1', name: 'read', status: 'running' }] }));
  expect(screen.getByText('Reading notes.md')).toBeInTheDocument();
  const row = screen.getByText('read').closest('details')!;
  expect(within(row.querySelector('summary')!).getByText('running')).toBeInTheDocument();
  expect(row).toHaveTextContent('patha/b/notes.md');
});

test('a transcript longer than the window offers earlier and missing messages, and asks the server for them', async () => {
  const ws = await openSession();
  ws.frame({ ...view(), items: [{ kind: 'earlier' }, item(entry('e5', [{ role: 'user', content: 'five', timestamp: 5 }])), { kind: 'gap', after: 5, upTo: 9 }, item(entry('e9', [{ role: 'user', content: 'nine', timestamp: 9 }]))] });
  await userEvent.click(screen.getByRole('button', { name: 'Load earlier messages' }));
  await userEvent.click(screen.getByRole('button', { name: 'Load missing messages' }));
  expect(ws.sent).toEqual([{ type: 'earlier' }, { type: 'gap', after: 5, upTo: 9 }]);
  expect(screen.getAllByRole('article', { name: 'You' })).toHaveLength(2);
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
  const group = screen.getByText('Worked · 2 tools · web_search, bash').closest('details')!;
  expect(group).not.toHaveAttribute('open');
  expect(within(group).getByText('weighing options')).toBeInTheDocument();

  const search = within(group).getByText('web_search').closest('details')!;
  expect(within(search.querySelector('summary')!).getByText('quack ducks')).toBeInTheDocument();
  expect(within(search.querySelector('summary')!).getByText('3 k chars')).toBeInTheDocument();
  expect(search).not.toHaveAttribute('open');
  await userEvent.click(within(search).getByText('web_search'));
  expect(search).toHaveAttribute('open');
  expect(search).toHaveTextContent('limit3');
  const out = search.querySelectorAll('pre')[0]!;
  expect(out.textContent).toHaveLength(2003);
  await userEvent.click(within(search).getByRole('button', { name: 'Show all (2500 characters)' }));
  expect(search.querySelectorAll('pre')[0]!.textContent).toHaveLength(2500);

  const bad = within(group).getByText('bash').closest('details')!;
  expect(within(bad.querySelector('summary')!).getByText('error')).toBeInTheDocument();
  expect(bad).toHaveTextContent('boom');
});

test('a call whose arguments were not valid JSON shows the parse error', async () => {
  const ws = await openSession();
  const call = { type: 'toolCall', id: 't9', name: 'bash', arguments: {}, argumentsError: 'Unexpected token } at 4' };
  ws.frame(snapshot({ entries: [entry('a', [assistant([call])])] }));
  expect(screen.getByText('bash').closest('details')!.querySelector('pre')).toHaveTextContent('Unexpected token } at 4');
});

test('a compaction entry is a muted collapsed line with its summary rendered, not a user bubble', async () => {
  const ws = await openSession();
  const summary = 'The conversation history before this point was compacted.\n\n## Goal\n\n- [x] ship it';
  ws.frame(snapshot({ entries: [{ ...entry('c', [{ role: 'user', content: summary, timestamp: 5 }]), kind: 'pi.compaction' }] }));
  expect(screen.queryByRole('article', { name: 'You' })).toBeNull();
  const line = screen.getByText('Earlier conversation summarized').closest('details')!;
  expect(line).not.toHaveAttribute('open');
  expect(within(line).getByRole('heading', { name: 'Goal' })).toBeInTheDocument();
  expect(line.querySelector('time')).toHaveAttribute('datetime', new Date(5).toISOString());
});

test('user messages render markdown inside the bubble', async () => {
  const ws = await openSession();
  ws.frame(snapshot({ entries: [entry('u', [{ role: 'user', content: '## Review\n\nsee [the PR](https://github.com/o/r/pull/1)', timestamp: 1 }])] }));
  const bubble = screen.getByRole('article', { name: 'You' });
  expect(within(bubble).getByRole('heading', { name: 'Review' })).toBeInTheDocument();
  const link = within(bubble).getByRole('link', { name: 'the PR' });
  expect(link).toHaveAttribute('target', '_blank');
  expect(link).toHaveAttribute('rel', expect.stringContaining('noreferrer'));
});

test('markdown tool output renders with a toggle to raw; the delivery link is not read from its tool text', async () => {
  const ws = await openSession();
  const call = (id: string, name: string) => ({ type: 'toolCall', id, name, arguments: { version: 1 } });
  const res = (id: string, name: string, text: string) => entry(`r${id}`, [{ role: 'toolResult', toolCallId: id, toolName: name, content: [{ type: 'text', text }], isError: false, timestamp: 2 }]);
  ws.frame(snapshot({
    entries: [
      entry('a', [assistant([call('s', 'submit_review')])]),
      res('s', 'submit_review', '## Takeaway\n\n- fine\nPosted as APPROVE: https://github.com/o/r/pull/1#pullrequestreview-9'),
    ],
  }));
  const submit = screen.getByText('submit_review').closest('details')!;
  expect(within(submit).getByRole('heading', { name: 'Takeaway' })).toBeInTheDocument();
  await userEvent.click(within(submit).getByRole('button', { name: 'Raw' }));
  expect(submit.querySelectorAll('pre')[0]).toHaveTextContent('## Takeaway');
  // Delivery links come from the artifact's links (next test), never from a tool's result text.
  expect(screen.queryByRole('link', { name: 'Posted ↗' })).toBeNull();
});

test('an artifact line renders the links stored with the version, with no stream entries to read', async () => {
  routes['GET /sessions/ui%3Au1/artifacts'] = { artifacts: [{ kind: 'github.review', version: 1, hash: 'h', content: 'x', sealedAt: 1, verdict: { passed: true, gatesFailed: [], score: 1, checks: [] }, links: [{ label: 'Posted', url: 'https://github.com/o/r/pull/1#pullrequestreview-9' }] }] };
  const ws = await openSession();
  ws.frame(snapshot());
  const link = await screen.findByRole('link', { name: 'Posted ↗' });
  expect(link).toHaveAttribute('href', 'https://github.com/o/r/pull/1#pullrequestreview-9');
  expect(link).toHaveAttribute('rel', expect.stringContaining('noreferrer'));
});

test('a Sleeper card in a session links back to that session, in the list and the Work drawer', async () => {
  const lineup = (await import('./extensions/sleeper/fixtures/8-lineup.json')).default as { content: string };
  routes['GET /sessions/ui%3Au1/artifacts'] = { artifacts: [{ kind: 'sleeper.cards', version: 1, hash: 'h', content: lineup.content, sealedAt: 1, verdict: null }] };
  const ws = await openSession();
  ws.frame(snapshot());
  const asks = await screen.findAllByRole('link', { name: 'Ask about this' });
  for (const a of asks) expect(a).toHaveAttribute('href', '#/s/ui%3Au1');
});

test('the header shows the token total and Waiting on you for a pending approval', async () => {
  routes['GET /sessions/ui%3Au1/approvals'] = { approvals: [{ id: 'a', tool: 'bash', args: {}, requestedAt: 1 }] };
  const ws = await openSession();
  ws.frame(snapshot({ usage: { models: { 'p/m': { input: 1200, output: 300 } }, tools: {} } }));
  expect(await screen.findByRole('status')).toHaveTextContent('Waiting on you');
  expect(screen.getByText('2k tokens')).toBeInTheDocument();
});

test('a GitHub session header links to the pull request', async () => {
  routes['GET /sessions/github%3Ao%2Fr%231/approvals'] = { approvals: [] };
  routes['GET /sessions/github%3Ao%2Fr%231/artifacts'] = { artifacts: [] };
  location.hash = '#/s/github%3Ao%2Fr%231';
  render(<App />);
  expect(await screen.findByRole('link', { name: 'View on GitHub' })).toHaveAttribute('href', 'https://github.com/o/r/pull/1');
});

test('a send while a run works is a server-side follow-up shown as a chip; Send now steers it and the cross withdraws it', async () => {
  const ws = await openSession();
  ws.frame(snapshot());
  const box = () => screen.getByRole('textbox', { name: 'Message' });
  const send = async (t: string) => {
    await userEvent.type(box(), t);
    await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  };
  await send('first');
  expect(ws.sent).toEqual([{ type: 'input', requestId: expect.any(String), input: 'first' }]);
  accept(ws);

  ws.frame(view({ run: true }));
  expect(screen.queryByRole('group', { name: 'While the agent is working' })).toBeNull();
  expect(box()).toHaveAttribute('placeholder', 'Message · sends after this step');
  await send('turn left');
  expect(ws.sent[1]).toEqual({ type: 'input', requestId: expect.any(String), input: 'turn left', whenBusy: 'followUp' });
  accept(ws);
  await waitFor(() => expect(box()).toHaveValue(''));
  expect(screen.queryByText('Queued')).toBeNull();

  // The chip is the server's queue: it shows when the session reports it, so a reload shows it too.
  ws.frame({ type: 'queued', items: [{ id: 's1', text: 'turn left' }, { id: 's2', text: 'never mind' }] });
  expect(screen.getByText('turn left')).toBeInTheDocument();
  await userEvent.click(screen.getAllByRole('button', { name: 'Send now' })[0]);
  expect(ws.sent[2]).toEqual({ type: 'steer', id: 's1' });
  await userEvent.click(screen.getAllByRole('button', { name: 'Remove queued message' })[1]);
  expect(ws.sent[3]).toEqual({ type: 'withdraw', id: 's2' });
  ws.frame({ type: 'queued', items: [] });
  expect(screen.queryByText('Queued')).toBeNull();
});

test('the first message of a review session is the PR card, with the files and checks the service attached', async () => {
  const key = 'github%3Ao%2Fr%231';
  const text = 'Review pull request o/r#1 (label quack:review).\nTitle: Cost per run\nHead: 9f43b50aaaa on feat/x; base: v1\n\nWhat changed';
  routes[`GET /sessions/${key}/artifacts`] = { artifacts: [] };
  routes[`GET /sessions/${key}/approvals`] = { approvals: [] };
  routes[`GET /sessions/${key}/meta`] = { meta: [{ id: '9f43b50aaaa', files: [{ path: 'a.ts', additions: 3, deletions: 1 }] }] };
  location.hash = `#/s/${key}`;
  render(<App />);
  const ws = FakeSocket.all[0];
  ws.open();
  ws.frame(snapshot({ entries: [entry('u', [{ role: 'user', content: text, timestamp: 1 }])] }));
  const card = await screen.findByRole('region', { name: 'Pull request' });
  expect(within(card).getByText('Cost per run')).toBeInTheDocument();
  expect(await within(card).findByText('1 file')).toBeInTheDocument();
  expect(screen.queryByText(/Review pull request/)).toBeNull();
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
  expect(JSON.parse(post[1]!.body as string)).toEqual({ decision: 'approve' });
});

test('an approval that appears mid-run shows up when tool activity arrives', async () => {
  const ws = await openSession();
  ws.frame(snapshot());
  routes['GET /sessions/ui%3Au1/approvals'] = { approvals: [{ id: 'b', tool: 'push', args: {}, requestedAt: 1 }] };
  ws.frame(view({ run: true, tools: [{ callId: 't', name: 'push', status: 'running' }] }));
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
  expect(screen.getByText('Error: Provider is not configured')).toBeInTheDocument();
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

test('a failed session fetch shows its error on the list', async () => {
  routes = {};
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  expect(await screen.findByRole('alert')).toBeInTheDocument();
});

test('Abort shows only while a run is in progress and posts to the abort route', async () => {
  routes['POST /sessions/ui%3Au1/abort'] = { aborted: true };
  const ws = await openSession();
  ws.frame(snapshot());
  expect(screen.queryByRole('button', { name: 'Abort' })).toBeNull();
  ws.frame(view({ run: true }));
  await userEvent.click(screen.getByRole('button', { name: 'More actions' }));
  await userEvent.click(screen.getByRole('menuitem', { name: 'Stop' }));
  expect(fetchMock).toHaveBeenCalledWith('sessions/ui%3Au1/abort', { method: 'POST' });
  ws.frame(view({}));
  expect(screen.queryByRole('button', { name: 'Abort' })).toBeNull();
});

test('a failed abort is reported', async () => {
  const ws = await openSession();
  ws.frame(snapshot({ run: {} }));
  await userEvent.click(screen.getByRole('button', { name: 'More actions' }));
  await userEvent.click(screen.getByRole('menuitem', { name: 'Stop' }));
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
  const check = (criterion: string, id: string, met: number | null) => ({ criterion, id, label: `Label ${id}`, weight: 2, met, notes: [] });
  routes['GET /sessions/ui%3Au1/artifacts'] = {
    artifacts: [
      { kind: 'github.review', version: 1, hash: 'h1', content: 'draft one', sealedAt: 1, verdict: { passed: false, gatesFailed: ['cites.lines'], score: 0.4, checks: [check('cites', 'lines', 0), check('tone', 'polite', 1), check('misc', 'na', null)] } },
      { kind: 'github.review', version: 2, hash: 'h2', content: 'final review', sealedAt: 2, verdict: { passed: true, gatesFailed: [], score: 1, checks: [check('cites', 'lines', 1)] } },
    ],
  };
  const ws = await openSession();
  ws.frame(snapshot());
  const section = await screen.findByRole('region', { name: 'Artifacts' });
  expect(within(section).getAllByRole('listitem')).toHaveLength(1);
  await userEvent.click(within(section).getByRole('button', { name: '1 earlier version' }));
  const items = within(section).getAllByRole('listitem');
  expect(items[0]).toHaveTextContent("Review didn't pass its checks");
  expect(items[1]).toHaveTextContent(/Review ready/);
  expect(items[1]).not.toHaveTextContent(/Cards|github\.review|v2|sealed/);
  expect(screen.queryByRole('dialog')).toBeNull();
  await userEvent.click(within(items[1]).getByRole('button', { name: 'Open Review version 2' }));
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
  expect(within(rail).getAllByRole('link').map((l) => l.getAttribute('href'))).toEqual(['#/', '#/sessions', '#/artifacts', '#/sleeper', '#/agents', '#/remarkable', '#/schedules', '#/contracts', '#/settings']);
  expect(within(rail).getByRole('link', { name: 'Home' })).toHaveAttribute('aria-current', 'page');
  await userEvent.click(within(rail).getByRole('link', { name: 'Artifacts' }));
  expect(within(rail).getByRole('link', { name: 'Artifacts' })).toHaveAttribute('aria-current', 'page');
  expect(within(rail).getByRole('link', { name: 'Home' })).not.toHaveAttribute('aria-current');
});

test('a session stays inside the Sessions rail entry', async () => {
  await openSession();
  const rail = screen.getAllByRole('navigation', { name: 'Main' })[0];
  expect(within(rail).getByRole('link', { name: 'Sessions' })).toHaveAttribute('aria-current', 'page');
});

test('the Settings page picks the theme: Dark persists, System clears it', async () => {
  document.documentElement.dataset.theme = 'light';
  localStorage.clear();
  location.hash = '#/settings';
  render(<App />);
  expect(await screen.findByRole('group', { name: 'Appearance' })).toBeInTheDocument();
  expect(screen.getByRole('radio', { name: 'System' })).toBeChecked();
  await userEvent.click(screen.getByRole('radio', { name: 'Dark' }));
  expect(document.documentElement.dataset.theme).toBe('dark');
  expect(localStorage.getItem('quack:theme')).toBe('dark');
  await userEvent.click(screen.getByRole('radio', { name: 'System' }));
  expect(document.documentElement.dataset.theme).toBeUndefined();
  expect(localStorage.getItem('quack:theme')).toBeNull();
});

const BUILD = { commit: 'abc1234def5678', builtAt: '2026-11-03T12:00:00Z' };

test('About shows the short commit linked to GitHub and the build time', async () => {
  routes['GET /version'] = BUILD;
  location.hash = '#/settings';
  render(<App />);
  const about = within(await screen.findByRole('region', { name: 'About' }));
  expect(about.getByRole('link', { name: 'abc1234' })).toHaveAttribute('href', 'https://github.com/fagerbergj/quack/commit/abc1234def5678');
  expect(screen.getByText(/^Built /)).toBeInTheDocument();
});

test('About reads development build when the image carries no commit', async () => {
  routes['GET /version'] = { commit: null, builtAt: null };
  location.hash = '#/settings';
  render(<App />);
  expect(await screen.findByText('quack v1 · development build')).toBeInTheDocument();
  expect(screen.queryByText(/^Built /)).toBeNull();
});

test('the sidebar footer shows the short commit and links to Settings', async () => {
  routes['GET /version'] = BUILD;
  render(<App />);
  const rail = screen.getAllByRole('complementary')[0];
  expect(await within(rail).findByRole('link', { name: 'Version abc1234' })).toHaveAttribute('href', '#/settings');
  expect(fetchMock.mock.calls.filter(([u]) => u === 'version')).toHaveLength(1);
});

test('More lists the short commit on a phone', async () => {
  routes['GET /version'] = BUILD;
  stubViewport(false);
  location.hash = '#/more';
  render(<App />);
  expect((await screen.findAllByRole('link', { name: 'Version abc1234' })).length).toBeGreaterThan(0);
});

test('a failed version fetch shows nothing', async () => {
  location.hash = '#/settings';
  render(<App />);
  expect(await screen.findByRole('group', { name: 'Appearance' })).toBeInTheDocument();
  await waitFor(() => expect(fetchMock.mock.calls.some(([u]) => u === 'version')).toBe(true));
  expect(screen.queryByText(/quack v1/)).toBeNull();
  expect(screen.queryByRole('link', { name: /^[0-9a-f]{7}$/ })).toBeNull();
});

test('More lists Settings and no longer carries the theme toggle', async () => {
  stubViewport(false);
  location.hash = '#/more';
  render(<App />);
  expect((await screen.findAllByRole('link', { name: 'Settings' })).length).toBeGreaterThan(0);
  expect(screen.queryByRole('button', { name: 'Day / night' })).toBeNull();
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
  expect(screen.getByText('Worked 4 s · 3 tools · web_search, web_fetch ×2')).toBeInTheDocument();
});

test('the Work drawer opens from the status line, shows report then checks, traps focus and closes on Esc', async () => {
  routes['GET /sessions/ui%3Au1/artifacts'] = {
    artifacts: [
      { kind: 'research.report', version: 2, hash: 'h2', content: '## Short answer\n\nGain is 1.9x [S9].', sealedAt: 2, verdict: { passed: false, gatesFailed: [], score: 0.5, checks: [{ criterion: 'citations', id: 'support', label: 'Cites a source that does not say it', weight: 3, met: 0, notes: ['S7 does not mention TP'] }, { criterion: 'dated', id: 'claims', label: 'A claim has no date', weight: 2, met: 1, notes: [] }] } },
    ],
  };
  const ws = await openSession();
  ws.frame(snapshot());
  expect(screen.queryByRole('dialog')).toBeNull();
  const open = await screen.findByRole('button', { name: 'Open Report version 2' });
  await userEvent.click(open);
  const work = screen.getByRole('dialog', { name: 'Work' });
  expect(within(work).getByRole('heading', { name: 'Short answer' })).toBeInTheDocument();
  expect(within(work).getByRole('button', { name: 'Close' })).toHaveFocus();
  await userEvent.click(within(work).getByRole('tab', { name: 'Checks' }));
  expect(work).toHaveTextContent('Cites a source that does not say it');
  expect(work).not.toHaveTextContent('citations.support');
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
  await userEvent.click(await screen.findByRole('button', { name: 'Open Report' }));
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
  ws.frame(view({ run: true }));
  expect(screen.getByRole('status')).toHaveTextContent('Running');
});

test('#/new lands on Home with the box focused, on phone and desktop alike', async () => {
  for (const desktop of [false, true]) {
    stubViewport(desktop);
    location.hash = '#/new';
    const { unmount } = render(<App />);
    const box = await screen.findByRole('textbox', { name: 'What do you need?' });
    await waitFor(() => expect(box).toHaveFocus());
    expect(location.hash).toBe('#/');
    unmount();
  }
});

const pendingIndex = { approvals: [{ session: 'gh:3', id: 'a/1', tool: 'github_post_review', state: 'pending', decision: null, by: null, at: 1 }] };
const goTo = (hash: string) => {
  location.hash = hash;
  act(() => void dispatchEvent(new HashChangeEvent('hashchange')));
};

test('with the index on, an idle session waiting on approval is in Needs you on the list and on Home', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Old work' })] };
  routes['GET /index/approvals?state=pending'] = pendingIndex;
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByText('Waiting for your approval')).toBeInTheDocument();
  expect(fetchMock.mock.calls.some(([u]) => u === 'sessions/gh%3A3/approvals')).toBe(false);
  goTo('#/');
  const home = await screen.findByRole('region', { name: 'Approval' });
  expect(within(home).getByRole('link', { name: 'Old work wants to continue' })).toHaveAttribute('href', '#/s/gh%3A3');
  expect(within(home).getByText('github_post_review', { exact: false })).not.toBeVisible();
});

test('Home posts approve to the owning session, and reject with its reason', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3')] };
  routes['GET /index/approvals?state=pending'] = pendingIndex;
  routes['POST /sessions/gh%3A3/approvals/a%2F1'] = { resolved: true };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  const post = () => JSON.parse(fetchMock.mock.calls.filter(([, i]) => i?.method === 'POST').at(-1)![1]!.body as string);
  await userEvent.click(await screen.findByRole('button', { name: 'Approve gh:3' }));
  expect(post()).toEqual({ decision: 'approve' });

  await userEvent.click(screen.getByRole('button', { name: 'Reject gh:3' }));
  await userEvent.type(screen.getByRole('textbox', { name: 'Reason' }), 'wrong repo');
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  await userEvent.click(screen.getByRole('button', { name: 'Confirm reject gh:3' }));
  expect(post()).toEqual({ decision: 'reject', reason: 'wrong repo' });
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Approval' })).toBeNull());
});

const pendingDecision = { questions: [{ session: 'gh:3', id: '7:type', question: 'What kind of document is this?', options: ['meeting', 'idea'], evidence: 'Agenda: standup', state: 'pending', choice: null, by: null, at: 1 }] };

test('Home shows a workflow question with its options, and an option answers it', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Campaign Notes' })] };
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  routes['GET /index/questions?state=pending'] = pendingDecision;
  routes['POST /sessions/gh%3A3/questions/7%3Atype'] = { answered: true };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  expect(await screen.findByText('What kind of document is this?')).toBeInTheDocument();
  expect(screen.getByText('Agenda: standup')).not.toBeVisible();
  expect(screen.getByRole('link', { name: 'Campaign Notes' })).toHaveAttribute('href', '#/s/gh%3A3');
  routes['GET /index/questions?state=pending'] = pendingDecision;
  await userEvent.click(screen.getByRole('button', { name: 'meeting' }));
  expect(JSON.parse(fetchMock.mock.calls.filter(([, i]) => i?.method === 'POST').at(-1)![1]!.body as string)).toEqual({ choice: 'meeting' });
  expect(screen.queryByRole('region', { name: 'Question' })).toBeNull();
});

test('a re-asked question with a newer askedAt shows again after the first was answered', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3')] };
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  routes['GET /index/questions?state=pending'] = pendingDecision;
  routes['POST /sessions/gh%3A3/questions/7%3Atype'] = { answered: true };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  const first = await screen.findByRole('button', { name: 'meeting' });
  routes['GET /index/questions?state=pending'] = { questions: pendingDecision.questions.map((d) => ({ ...d, at: 2 })) };
  await userEvent.click(first);
  expect(await screen.findByRole('region', { name: 'Question' })).toBeInTheDocument();
});

test('a failed answer keeps the question card and shows the error', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3')] };
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  routes['GET /index/questions?state=pending'] = pendingDecision;
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  await userEvent.click(await screen.findByRole('button', { name: 'meeting' }));
  expect(await screen.findByRole('alert')).toBeInTheDocument();
  expect(screen.getByRole('region', { name: 'Question' })).toBeInTheDocument();
});

test('a session waiting on a question is in Needs you, and its view shows the question card', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1')] };
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  routes['GET /index/questions?state=pending'] = { questions: pendingDecision.questions.map((d) => ({ ...d, session: 'ui:u1' })) };
  const ws = await openSession();
  ws.frame({ type: 'standing', questions: [{ id: '7:type', question: 'What kind of document is this?', options: ['meeting', 'idea'], evidence: null, askedAt: 1 }], failures: [] });
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByText('What kind of document is this?')).toBeInTheDocument();
  const card = await screen.findByRole('region', { name: 'Question' });
  expect(within(card).getByText('NEEDS YOU')).toBeInTheDocument();
  expect(within(card).getByRole('button', { name: 'idea' })).toBeInTheDocument();
});

const failure = { step: 'decide', error: 'Validation failed for tool "decide":\n- summary: must not have more than 300 characters' };

test('a failed run shows Failed in the header and a card with the step, the first error line and Run again', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1')] };
  const ws = await openSession();
  ws.frame({ type: 'standing', questions: [], failures: [failure] });
  const card = await screen.findByRole('region', { name: 'Failed run' });
  expect(within(card).getByText('decide')).toBeInTheDocument();
  expect(within(card).getByText('Validation failed for tool "decide":')).toBeInTheDocument();
  await userEvent.click(within(card).getByText('Full error'));
  expect(within(card).getByText(/must not have more than 300 characters/)).toBeVisible();
  expect(screen.getByRole('status')).toHaveTextContent('Failed · decide');
  routes['POST /sessions/ui%3Au1/resume'] = { submissionId: '2', status: 'placed' };
  await userEvent.click(within(card).getByRole('button', { name: 'Run again' }));
  expect(fetchMock).toHaveBeenCalledWith('sessions/ui%3Au1/resume', { method: 'POST' });
  ws.frame({ type: 'standing', questions: [], failures: [] });
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Failed run' })).toBeNull());
  expect(screen.getByRole('status')).toHaveTextContent('Idle');
});

test('a failed run that cannot resume shows why, and Copy error copies the whole error', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1')] };
  const ws = await openSession();
  ws.frame({ type: 'standing', questions: [], failures: [failure] });
  const writeText = vi.fn(async () => undefined);
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
  const card = await screen.findByRole('region', { name: 'Failed run' });
  await userEvent.click(within(card).getByRole('button', { name: 'Copy error' }));
  expect(writeText).toHaveBeenCalledWith(failure.error);
  await userEvent.click(within(card).getByRole('button', { name: 'Run again' }));
  expect(await within(card).findByRole('alert')).toBeInTheDocument();
});

test('a session whose run failed is under Needs you with its step and the error first line', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Trade finder' })] };
  routes['GET /index/failures'] = { failures: [{ session: 'gh:3', ...failure, at: 1 }] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByText('Run failed at decide · Validation failed for tool "decide":')).toBeInTheDocument();
});

test('Home pond goes still when a session has failed', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Trade finder' })] };
  routes['GET /index/failures'] = { failures: [{ session: 'gh:3', ...failure, at: 1 }] };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  await screen.findByText('Run failed at decide · Validation failed for tool "decide":');
  expect(screen.getByText('What do you need?').closest('.pond')).toHaveClass('still');
});

test('Home pond keeps swimming when nothing has failed', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Trade finder' })] };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  const pond = (await screen.findByText('What do you need?')).closest('.pond');
  expect(pond).not.toHaveClass('still');
  expect(pond?.querySelector('.swimmer')).not.toBeNull();
});

test('with the index off, Home falls back to the running sessions own approvals, details behind a disclosure', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { running: true, active: true })] };
  routes['GET /sessions/gh%3A1/approvals'] = { approvals: [{ id: 'a', tool: 'push', args: { branch: 'main' }, requestedAt: 1 }] };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  expect(await screen.findByRole('button', { name: 'Approve gh:1' })).toBeInTheDocument();
  expect(screen.getByText('gh:1 wants to continue')).toBeInTheDocument();
  expect(screen.getByText('Details').closest('details')).toHaveTextContent('branchmain');
  expect(screen.getByText('push')).not.toBeVisible();
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
  const open = await screen.findByRole('button', { name: 'Open Old work' });
  expect(within(open).getByRole('img', { name: 'Passed' })).toBeInTheDocument();
  expect(open).not.toHaveTextContent(/versions|Double-checked/);
  expect(screen.getAllByRole('button', { name: /^Open / })).toHaveLength(1);
  await userEvent.click(open);
  expect(await screen.findByText('final review')).toBeInTheDocument();
  await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Version' }), '1');
  expect(screen.getByText('first draft')).toBeInTheDocument();
  expect(screen.getByRole('dialog', { name: 'Work' })).toBeInTheDocument();
});

test('Artifacts rows say what each is and what it concluded, and chips filter by kind', async () => {
  routes['GET /sessions'] = { sessions: [sess('github:o/r#7', { title: 'feat: Declare requires' }), sess('ui:r1', { agent: 'researcher', title: 'Why is it slow?', running: true, active: true })] };
  routes['GET /index/artifacts'] = { artifacts: [
    { session: 'github:o/r#7', kind: 'github.review', version: 1, hash: 'h', verdict: { passed: true }, summary: 'Approve · 1 nit', sealedAt: 2 },
    { session: 'ui:r1', kind: 'research.report', version: 1, hash: 'g', verdict: { passed: false }, summary: 'Cold caches dominate', sealedAt: 1 },
    { session: 'ui:old', kind: 'research.report', version: 1, hash: 'o', verdict: null, sealedAt: 1 },
  ] };
  location.hash = '#/artifacts';
  render(<App />);
  const review = await screen.findByRole('button', { name: 'Open #7 Declare requires' });
  expect(review).toHaveTextContent('Review · Approve · 1 nit');
  const research = screen.getByRole('button', { name: 'Open Why is it slow?' });
  expect(research).toHaveTextContent('Research report · Cold caches dominate');
  expect(within(research).getByRole('img', { name: 'Revising' })).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Open ui:old' })).not.toHaveTextContent('·');
  await userEvent.click(screen.getByRole('button', { name: 'Reviews' }));
  expect(screen.getAllByRole('button', { name: /^Open / })).toHaveLength(1);
});

test('a failed newest version reads as revising while its session runs, and as not passed once idle', () => {
  expect(statusOf({ passed: false }, true).label).toBe('Revising');
  expect(statusOf({ passed: false }, false).label).toBe("Didn't pass");
  expect(statusOf({ passed: true }, true).label).toBe('Passed');
  expect(statusOf(null, false).label).toBe('Not checked');
});

test('Artifacts says so when the index is off', async () => {
  location.hash = '#/artifacts';
  render(<App />);
  expect(await screen.findByText(/The index is off/)).toBeInTheDocument();
});

test('a failed load shows its error on Home', async () => {
  delete routes['GET /sessions'];
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  expect(await screen.findAllByRole('alert')).not.toHaveLength(0);
});

test('a double-click on Approve posts once', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3')] };
  routes['GET /index/approvals?state=pending'] = pendingIndex;
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  const approve = await screen.findByRole('button', { name: 'Approve gh:3' });
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
  expect(await screen.findByRole('article', { name: 'Week 8 lineup' })).toBeInTheDocument();
  expect(strip).toHaveTextContent(/Lineup lock(s|ed)Sun/);
  expect(within(screen.getByRole('article', { name: 'Week 8 lineup' })).getByTitle('Double-checked')).toBeInTheDocument();
  expect(document.body).not.toHaveTextContent(/v1 ·|sealed|not checked/);
  const card = screen.getByRole('article', { name: 'Week 8 lineup' });
  expect(within(card).getByText('Allen')).toBeInTheDocument();
  expect(within(card).getByText('START')).toBeInTheDocument();
  expect(within(card).getByText('Why Allen')).toBeInTheDocument();
  expect(within(card).getByText(/replaces Concepcion/)).toBeInTheDocument();
  const news = within(card).getByText('News (1)').closest('details')!;
  expect(within(news).getByRole('link', { name: /Practice report/ })).toHaveAttribute('href', 'https://nfl.com/x');
  expect(news).toHaveTextContent('nfl.com · 2026-10-30');
  expect(within(card).getByRole('link', { name: 'Ask about this' })).toHaveAttribute('href', `#/s/${encodeURIComponent(KEY)}`);
});

const chk = (id: string, label: string, weight: number | 'gate', met: number | null, notes: string[] = []) => ({ criterion: 'c', id, label, weight, met, notes });

test('a failed lineup card names its failing checks by label, with the reason open', async () => {
  const failed = { passed: false, gatesFailed: [], score: 0.4, checks: [chk('summary_names_changes', 'Summary skips a lineup change', 'gate', 0, ['RB2: Summary says nothing about the swap. It also omits FLEX.'])] };
  await openSleeper([sealed(lineup(), null, 1), sealed(lineup(), failed, 2)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(within(card).getByText("Didn't pass its checks")).toBeInTheDocument();
  const label = within(card).getByText('Summary skips a lineup change');
  expect(label).toHaveAttribute('title', 'c.summary_names_changes');
  expect(card).not.toHaveTextContent('Summary names changes');
  expect(within(card).getByText('RB2')).toBeInTheDocument();
  expect(card).not.toHaveTextContent('RB2:');
  expect(card).toHaveTextContent('Summary says nothing about the swap.');
  expect(card).not.toHaveTextContent('It also omits FLEX');
  await userEvent.click(within(card).getByRole('button', { name: 'More' }));
  expect(card).toHaveTextContent('It also omits FLEX');
});

test('a note about the whole card drops its "card:" prefix and renders backticks as code', async () => {
  const failed = { passed: false, gatesFailed: [], score: 0.4, checks: [chk('x', 'Misses the news', 3, 0, ['card: Does not use `why` for the news.'])] };
  await openSleeper([sealed(lineup(), failed)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(card).not.toHaveTextContent('card:');
  expect(card.querySelector('code')).toHaveTextContent('why');
});

test('a passed pitfall is not shown as failed, a triggered pitfall and a missed positive check are', async () => {
  const checks = [chk('weather', 'Blames weather for a call weather did not change', -2, 0), chk('bye', 'Misses a bye week in the plan', -2, 1), chk('news', 'A close call does not cite the news', 3, 0.5), chk('na', 'Not applicable one', -2, null)];
  await openSleeper([sealed(lineup(), { passed: false, gatesFailed: [], score: 0.5, checks })]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(card).not.toHaveTextContent('Blames weather');
  expect(card).not.toHaveTextContent('Not applicable one');
  expect(card).toHaveTextContent('Misses a bye week in the plan');
  expect(card).toHaveTextContent('A close call does not cite the news');
});

test('a passed card shows no red boxes; partial misses sit behind its Double-checked line', async () => {
  const checks = [chk('news', 'A close call does not cite the news', 3, 0.6, ['RB2: Only half the report is cited.'])];
  await openSleeper([sealed(lineup(), { passed: true, gatesFailed: [], score: 0.9, checks })]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  const note = within(card).getByText('Double-checked').closest('details')!;
  expect(note).not.toHaveAttribute('open');
  expect(card).not.toHaveTextContent("Didn't pass");
  expect(card.querySelector('.text-danger')).toBeNull();
});

test('a failing check with no judge note has no disclosure', async () => {
  const failed = { passed: false, gatesFailed: [], score: 0.4, checks: [chk('s', 'Summary skips a lineup change', 'gate', 0)] };
  await openSleeper([sealed(lineup(), failed)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(card).toHaveTextContent('Summary skips a lineup change');
  expect(card.querySelector('details summary .chev')).toBeNull();
});

test('in the Work checks, a failed check starts open, a passed one with a note is closed, one with no note has no chevron', async () => {
  const checks = [chk('a', 'Quotes text that is not in the source', 3, 0, ['Not in the source.']), chk('b', 'Cites a source that was not opened', 3, 1, ['All sources opened.']), chk('c', 'A finding cites no source', 3, 1)];
  routes['GET /sessions/ui%3Au1/artifacts'] = { artifacts: [{ kind: 'research.report', version: 2, hash: 'h2', content: '## Short answer\n\nx', sealedAt: 2, verdict: { passed: false, gatesFailed: [], score: 0.5, checks } }] };
  const ws = await openSession();
  ws.frame(snapshot());
  await userEvent.click(await screen.findByRole('button', { name: 'Open Report version 2' }));
  const work = screen.getByRole('dialog', { name: 'Work' });
  await userEvent.click(within(work).getByRole('tab', { name: 'Checks' }));
  const open = (label: string) => within(work).getByText(label).closest('details');
  expect(open('Quotes text that is not in the source')).toHaveAttribute('open');
  expect(open('Cites a source that was not opened')).not.toHaveAttribute('open');
  expect(open('A finding cites no source')).toBeNull();
  expect(work.querySelectorAll('summary .chev')).toHaveLength(2);
  expect(work).not.toHaveTextContent('Why');
});

test('a card with no verdict yet shows no status text', async () => {
  await openSleeper([sealed(lineup(), null)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(card).not.toHaveTextContent(/checked|v1/);
});

test('a card with no changes shows the calm banner, not the change summary', async () => {
  await openSleeper([sealed(lineup({ summary: 'No changes.', starters: [row('Jefferson')] }), null)]);
  expect(await screen.findByText('No changes: your lineup stands')).toBeInTheDocument();
});

test('without the Sleeper route the page says it is not set up, whatever sessions exist', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1'), sess('github:a/b#1')] };
  location.hash = '#/sleeper';
  render(<App />);
  expect(await screen.findByText('Sleeper is not set up on this server.')).toBeInTheDocument();
});

test('an open question card keeps counting its age while the session is quiet', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: false });
  try {
    vi.setSystemTime(new Date('2026-11-01T01:05:00.000Z'));
    const ws = await openSession();
    ws.frame({ type: 'standing', questions: [{ id: '7:type', question: 'Which?', options: ['a'], evidence: null, askedAt: Date.parse('2026-11-01T01:04:00.000Z') }], failures: [] });
    const card = screen.getByRole('region', { name: 'Question' });
    expect(within(card).getByText('1m')).toBeInTheDocument();
    act(() => void vi.advanceTimersByTime(120_000));
    expect(within(card).getByText('3m')).toBeInTheDocument();
  } finally {
    vi.useRealTimers();
  }
});

test('the transcript line names each artifact in plain words and never shows the raw kind', async () => {
  const art = (kind: string, version: number, content: string, extra: object = {}) => ({ kind, version, hash: `h${version}`, content, sealedAt: version, verdict: null, ...extra });
  const pass = { passed: true, gatesFailed: [], score: 1, checks: [] };
  const fail = { passed: false, gatesFailed: [], score: 0.2, checks: [] };
  routes['GET /sessions/ui%3Au1/artifacts'] = {
    artifacts: [
      art('sleeper.cards', 10, JSON.stringify({ kind: 'lineup', starters: [] }), { verdict: pass }),
      art('sleeper.cards', 11, JSON.stringify({ kind: 'waivers' }), { verdict: pass }),
      art('sleeper.cards', 12, JSON.stringify({ kind: 'trade-finder' }), { verdict: pass }),
      art('sleeper.cards', 13, JSON.stringify({ kind: 'trade' }), { verdict: pass }),
      art('sleeper.cards', 14, JSON.stringify({ kind: 'trends' }), { verdict: pass }),
      art('sleeper.cards', 15, JSON.stringify({ kind: 'digest', week: 7, past: true }), { verdict: pass }),
      art('sleeper.cards', 16, JSON.stringify({ kind: 'digest', week: 8, past: false }), { verdict: pass }),
      art('sleeper.cards', 17, JSON.stringify({ kind: 'retro', week: 6 }), { verdict: pass }),
      art('sleeper.cards', 18, JSON.stringify({ kind: 'history' }), { verdict: pass }),
      art('sleeper.cards', 19, JSON.stringify({ kind: 'novel' }), { verdict: pass }),
      art('sleeper.cards', 30, JSON.stringify({ kind: 'lineup', starters: [] }), { verdict: fail }),
      art('github.review', 5, 'r', { verdict: pass }),
      art('github.review', 6, 'r', { verdict: pass, links: [{ label: 'GitHub', url: 'https://example.com/pull/1' }] }),
      art('remarkable.transcript', 7, 't'),
      art('mystery.thing', 8, 'x', { verdict: fail }),
      art('mystery.other', 9, 'x'),
    ],
  };
  const ws = await openSession();
  ws.frame(snapshot());
  const section = await screen.findByRole('region', { name: 'Artifacts' });
  await userEvent.click(within(section).getByRole('button', { name: /earlier versions/ }));
  const lines = within(section).getAllByRole('listitem').map((li) => li.textContent ?? '');
  const has = (x: string) => expect(lines.some((l) => l.includes(x)), x).toBe(true);
  for (const x of ['Lineup picks ready', 'Waiver claims ready', 'Trade ideas ready', 'Trade check ready', 'Trends ready', 'Week 7 recap ready', 'Week 8 preview ready', 'Week 6 retro ready', 'League history ready']) has(x);
  expect(lines.some((l) => l.replace('✓', '').startsWith('Ready'))).toBe(true);
   has("Lineup picks didn't pass its checks");
  has('Review ready'); has('Review posted'); has('Transcript ready'); has("Result didn't pass its checks"); has('Result ready');
  expect(lines.join(' ')).not.toMatch(/Cards|Thing|Other/);
  expect(within(section).getByRole('link', { name: /GitHub/ })).toBeInTheDocument();
});

test('a verdict sealed before checks had labels falls back to the id in plain words', async () => {
  const old = { passed: false, gatesFailed: [], score: 0.4, checks: [{ criterion: 'c', id: 'weather_mentioned_unmoved', weight: -2, met: 1, notes: [] }] };
  await openSleeper([sealed(lineup(), old)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(within(card).getByText('weather mentioned unmoved')).toBeInTheDocument();
});

test('a note does not end its first sentence at e.g.', async () => {
  const failed = { passed: false, gatesFailed: [], score: 0.4, checks: [chk('x', 'Misses the news', 3, 0, ['Cites no report, e.g. the practice update. It stops there.'])] };
  await openSleeper([sealed(lineup(), failed)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(card).toHaveTextContent('Cites no report, e.g. the practice update.');
  expect(card).not.toHaveTextContent('It stops there');
});

test('below lg the session pane subtracts the 48px nav plus the bottom safe inset', async () => {
  stubViewport(false);
  vi.stubGlobal('innerHeight', 844);
  vi.stubGlobal('visualViewport', { height: 844, addEventListener() {}, removeEventListener() {} });
  await openSession();
  const pane = document.querySelector<HTMLElement>('[style*="--ws-h"]');
  expect(pane?.style.getPropertyValue('--ws-h')).toBe('calc(844px - 48px - var(--safe-b))');
});

test('below lg main is the scroller inside a fixed frame that ends at the nav', async () => {
  stubViewport(false);
  await openSession();
  const main = screen.getByRole('main');
  expect(main).toHaveClass('flex-1', 'min-h-0', 'overflow-y-auto', 'overscroll-y-contain');
  expect(main.parentElement).toHaveClass('fixed', 'top-0', 'bottom-[calc(48px+var(--safe-b))]', 'lg:contents');
});

// Real prod index rows [version, judge passed, posted, verdict in the content] for #2019 and #2042.
const PROD: Record<string, Array<[number, boolean, boolean, string]>> = {
  'github:fagerbergj/quack#2019': [[1, false, false, 'approve'], [2, true, false, 'approve'], [3, true, false, 'request_changes'], [4, true, false, 'request_changes'], [5, false, false, 'request_changes'], [6, true, true, 'request_changes'], [7, false, true, 'approve']],
  'github:fagerbergj/quack#2042': [[1, false, false, 'comment'], [2, true, false, 'request_changes'], [3, true, true, 'request_changes'], [4, false, false, 'approve'], [5, true, true, 'request_changes']],
};
const prodArtifacts = (...keys: string[]) => keys.flatMap((k) => PROD[k]!.map(([v, ok, posted, outcome]) => indexed(k, 'github.review', ok, v, v, { outcome, posted })));

test('an idle review reads as its posted verdict, a limit-released one included, and a running request reads Reviewing', async () => {
  const k19 = 'github:fagerbergj/quack#2019';
  const k42 = 'github:fagerbergj/quack#2042';
  routes['GET /sessions'] = { sessions: [sess(k19, { title: 'quotes_exist fails only on nowhere' }), sess(k42, { title: 'Sleeper week stepper', running: true, active: true }), sess('github:fagerbergj/quack#2043', { title: 'Only commented' }), sess('github:fagerbergj/quack#2044', { title: 'Ended unposted' })] };
  routes['GET /index/artifacts'] = { artifacts: [...prodArtifacts(k19, k42), indexed('github:fagerbergj/quack#2043', 'github.review', true, 1, 1, { outcome: 'comment', posted: true }), indexed('github:fagerbergj/quack#2044', 'github.review', false)] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const row = async (name: RegExp) => within(await screen.findByRole('link', { name }));
  expect((await row(/#2019/)).getByText('Approved ✓')).toBeInTheDocument();
  expect((await row(/#2019/)).queryByText("Didn't pass its checks")).toBeNull();
  const running = await row(/#2042/);
  expect(running.getByText('Reviewing')).toBeInTheDocument();
  expect(running.queryByText(/Changes requested|✓/)).toBeNull();
  expect((await row(/#2043/)).getByText('Commented')).toBeInTheDocument();
  expect((await row(/#2044/)).getByText('No review posted')).toBeInTheDocument();
});

async function openReview(key: string, active: boolean, sealedItems: object[] = []) {
  routes['GET /sessions'] = { sessions: [sess(key, { title: 'Sleeper week stepper', running: active, active })] };
  routes['GET /index/artifacts'] = { artifacts: prodArtifacts(key) };
  routes[`GET /sessions/${encodeURIComponent(key)}/artifacts`] = { artifacts: sealedItems };
  routes[`GET /sessions/${encodeURIComponent(key)}/approvals`] = { approvals: [] };
  location.hash = `#/s/${encodeURIComponent(key)}`;
  render(<App />);
  await screen.findByRole('heading', { name: /Sleeper week stepper/ });
}

test('an idle session header shows the posted verdict with its Posted link', async () => {
  const key = 'github:fagerbergj/quack#2042';
  await openReview(key, false);
  expect(await screen.findByRole('link', { name: 'Posted ↗' })).toHaveAttribute('href', `https://github.com/${key}/review-5`);
  expect(screen.getAllByText('Changes requested').length).toBeGreaterThan(0);
});

test('a session header shows no earlier result while a new request runs', async () => {
  await openReview('github:fagerbergj/quack#2042', true);
  await screen.findByRole('status');
  expect(screen.queryByRole('link', { name: 'Posted ↗' })).toBeNull();
  expect(screen.queryByText('Changes requested')).toBeNull();
});

const postedV5 = { kind: 'github.review', version: 5, hash: 'h5', content: '## Verdict\nrequest_changes', sealedAt: 5, verdict: { passed: true, gatesFailed: [], score: 1, checks: [] }, links: [{ label: 'Posted', url: 'https://github.com/x/review-5' }] };

test('the pinned artifact line reads Previous review, without a tick, while a new request runs, and Review posted once it ends', async () => {
  await openReview('github:fagerbergj/quack#2042', true, [postedV5]);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot());
  ws.frame(view({ run: true }));
  const line = (await screen.findByText('Previous review')).closest('li')!;
  expect(within(line).queryByRole('img', { name: 'Double-checked' })).toBeNull();
  expect(within(line).queryByRole('region', { name: 'Review' })).toBeNull();
  expect(line).not.toHaveTextContent('Double-checked');
  ws.frame(view({}));
  expect(await screen.findByText('Review posted')).toBeInTheDocument();
  const done = screen.getByText('Review posted').closest('li')!;
  expect(within(done).getByRole('region', { name: 'Review' })).toBeInTheDocument();
  expect(done).toHaveTextContent('Double-checked');
  // The card carries the one Posted link; the pinned line drops its own.
  expect(within(screen.getByText('Review posted').closest('li')!).getAllByRole('link', { name: /Posted/ })).toHaveLength(1);
});

test('the transcript scroller is the containing block for absolute children, so sr-only headings cannot stretch main', async () => {
  stubViewport(false);
  await openSession();
  expect(document.querySelector('[aria-live="polite"]')).toHaveClass('relative', 'overflow-y-auto');
});

const withRow = (over: object = {}) => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1', { title: 'Plan the trip', ...over })] };
  routes['PATCH /sessions/ui%3Au1'] = { key: 'ui:u1' };
  routes['POST /sessions/ui%3Au1/delete'] = { key: 'ui:u1' };
};
const menu = async (item: string) => {
  await userEvent.click(await screen.findByRole('button', { name: 'More actions' }));
  await userEvent.click(screen.getByRole('menuitem', { name: item }));
};
const sent = (method: string) => fetchMock.mock.calls.filter(([, init]) => init?.method === method).map(([url, init]) => [url, JSON.parse(String(init?.body))]);

test('Rename saves the title, reloads the list and the header shows the new name', async () => {
  withRow();
  await openSession();
  await menu('Rename');
  const dialog = screen.getByRole('dialog', { name: 'Rename session' });
  const field = within(dialog).getByLabelText('Title');
  expect(field).toHaveValue('Plan the trip');
  routes['GET /sessions'] = { sessions: [sess('ui:u1', { title: 'Book flights' })] };
  await userEvent.clear(field);
  await userEvent.type(field, 'Book flights');
  await userEvent.click(within(dialog).getByRole('button', { name: 'Save' }));
  expect(sent('PATCH')).toEqual([['sessions/ui%3Au1', { title: 'Book flights' }]]);
  expect(await screen.findByRole('heading', { level: 1, name: 'Book flights' })).toBeInTheDocument();
  expect(screen.queryByRole('dialog')).toBeNull();
});

test('archived sessions leave the list and Home, and the Archived filter shows them', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1', { title: 'Plan the trip', archived: true, running: true, active: true }), sess('ui:u2', { title: 'Other' })] };
  render(<App />);
  await screen.findByRole('link', { name: /Other/ });
  expect(screen.queryByRole('link', { name: /Plan the trip/ })).toBeNull();
  expect(screen.queryByRole('region', { name: 'Running' })).toBeNull();
  await userEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await userEvent.selectOptions(screen.getByLabelText('Show'), 'Archived');
  expect(screen.getByRole('link', { name: /Plan the trip/ })).toBeInTheDocument();
  expect(screen.queryByRole('link', { name: /Other/ })).toBeNull();
});

test('Archive posts the flag from the session menu', async () => {
  withRow();
  await openSession();
  await menu('Archive');
  expect(sent('PATCH')).toEqual([['sessions/ui%3Au1', { archived: true }]]);
});

test('Export is a download link to the session file', async () => {
  withRow();
  await openSession();
  await userEvent.click(await screen.findByRole('button', { name: 'More actions' }));
  const link = screen.getByRole('menuitem', { name: 'Export' });
  expect(link).toHaveAttribute('href', 'sessions/ui%3Au1/export');
  expect(link).toHaveAttribute('download');
});

test('an archived session is still readable and offers Unarchive', async () => {
  withRow({ archived: true });
  const ws = await openSession();
  ws.frame(snapshot());
  expect(await screen.findByText('Archived')).toBeInTheDocument();
  await menu('Unarchive');
  expect(sent('PATCH')).toEqual([['sessions/ui%3Au1', { archived: false }]]);
});

test('a refused archive says why and the session stays', async () => {
  withRow();
  delete routes['PATCH /sessions/ui%3Au1'];
  await openSession();
  await menu('Archive');
  expect(await screen.findByRole('alert')).toHaveTextContent('not found');
});

test('Delete asks first, names the session, and goes back to the list once confirmed', async () => {
  withRow();
  await openSession();
  await menu('Delete');
  const dialog = screen.getByRole('dialog', { name: 'Delete session?' });
  expect(dialog).toHaveTextContent('Plan the trip');
  await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
  expect(sent('POST')).toEqual([]);
  await menu('Delete');
  routes['GET /sessions'] = { sessions: [] };
  await userEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await waitFor(() => expect(location.hash).toBe('#/'));
  expect(sent('POST')).toEqual([['sessions/ui%3Au1/delete', {}]]);
});

test('a refused delete keeps the dialog open with the server reason', async () => {
  withRow();
  delete routes['POST /sessions/ui%3Au1/delete'];
  await openSession();
  await menu('Delete');
  await userEvent.click(screen.getByRole('button', { name: 'Delete' }));
  expect(await within(screen.getByRole('dialog')).findByRole('alert')).toHaveTextContent('not found');
});

const phoneAt = (hash: string) => {
  stubViewport(false);
  location.hash = hash;
};

test('Home shows what needs you and what is running, each linking to its session', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { title: 'Needs a look' }), sess('gh:2', { title: 'Busy one', running: true, active: true }), sess('gh:3', { title: 'Old work' })] };
  routes['GET /index/approvals?state=pending'] = { approvals: [{ session: 'gh:1', id: 'a', tool: 'push', state: 'pending', decision: null, by: null, at: 1 }] };
  phoneAt('#/');
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByRole('link', { name: 'Needs a look wants to continue' })).toHaveAttribute('href', '#/s/gh%3A1');
  const running = screen.getByRole('region', { name: 'Running' });
  expect(within(running).getByRole('link', { name: /Busy one/ })).toHaveAttribute('href', '#/s/gh%3A2');
  expect(within(running).getByRole('link', { name: 'All sessions' })).toHaveAttribute('href', '#/sessions');
  expect(screen.queryByRole('combobox')).toBeNull();
  expect(screen.queryByText('Old work')).toBeNull();
});

test('an approval with a summary reads in plain words and keeps the raw call behind Details', async () => {
  routes['GET /sessions'] = { sessions: [sess('home:h1', { title: 'Waivers', running: true, active: true })] };
  routes['GET /sessions/home%3Ah1/approvals'] = { approvals: [{ id: 'a', tool: 'start', args: { summary: 'Process waivers for week 9' }, requestedAt: 1 }] };
  phoneAt('#/');
  render(<App />);
  const card = await screen.findByRole('region', { name: 'Approval' });
  expect(within(card).getAllByText('Process waivers for week 9')[0]).toBeInTheDocument();
  expect(within(card).getByText('start', { exact: false })).not.toBeVisible();
});

test('a failed session that is not running is on Home with Run again; a running one is not', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Trade finder' }), sess('gh:4', { title: 'Retrying', running: true, active: true })] };
  routes['GET /index/failures'] = { failures: [{ session: 'gh:3', ...failure, at: 1 }, { session: 'gh:4', ...failure, at: 1 }] };
  routes['POST /sessions/gh%3A3/resume'] = { resumed: true };
  phoneAt('#/');
  render(<App />);
  const card = await screen.findByRole('region', { name: 'Failed run' });
  expect(screen.getAllByRole('region', { name: 'Failed run' })).toHaveLength(1);
  await userEvent.click(within(card).getByRole('button', { name: 'Run Trade finder again' }));
  expect(fetchMock.mock.calls.some(([u, i]) => u === 'sessions/gh%3A3/resume' && i?.method === 'POST')).toBe(true);
});

test('the Home box opens a home: session for the quack agent and seeds its first message', async () => {
  phoneAt('#/');
  render(<App />);
  const send = screen.getByRole('button', { name: 'Send' });
  expect(send).toBeDisabled();
  await userEvent.type(screen.getByRole('textbox', { name: 'What do you need?' }), 'how did the review go?');
  await userEvent.click(send);
  await waitFor(() => expect(FakeSocket.all[0]?.url).toMatch(/\/sessions\/home%3Amain\/ws\?agent=quack$/));
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('how did the review go?');
  expect(screen.getByRole('heading', { name: 'quack' })).toBeInTheDocument();
  expect(document.querySelector('a[aria-label="Home"]')).toHaveAttribute('href', '#/');
});

test('#/sessions is the session list on a phone, and #/inbox goes to Home', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Old work' })] };
  phoneAt('#/sessions');
  render(<App />);
  expect(await screen.findByRole('region', { name: 'Recent' })).toBeInTheDocument();
  expect(screen.queryByRole('textbox', { name: 'What do you need?' })).toBeNull();
  goTo('#/inbox');
  expect(await screen.findByRole('textbox', { name: 'What do you need?' })).toBeInTheDocument();
  expect(location.hash).toBe('#/');
});

const tabs = () => within(screen.getAllByRole('navigation', { name: 'Main' })[1]).getAllByRole('link').map((l) => l.textContent);

describe('bottom bar', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.restoreAllMocks());

  test('defaults to Sessions, Artifacts, Sleeper between Home and More', () => {
    phoneAt('#/');
    render(<App />);
    expect(tabs()).toEqual(['Home', 'Sessions', 'Artifacts', 'Sleeper', 'More']);
  });

  test('Settings changes it live: unticking drops a page, order moves, and a page left out is under More', async () => {
    phoneAt('#/settings');
    render(<App />);
    await userEvent.click(screen.getByRole('button', { name: 'Move Sleeper up' }));
    expect(tabs()).toEqual(['Home', 'Sessions', 'Sleeper', 'Artifacts', 'More']);
    await userEvent.click(screen.getByRole('checkbox', { name: 'Sessions' }));
    expect(tabs()).toEqual(['Home', 'Sleeper', 'Artifacts', 'More']);
    expect(JSON.parse(localStorage.getItem('quack:bottom-bar')!)).toEqual(['sleeper', 'artifacts']);
    goTo('#/more');
    expect(await within(screen.getByRole('main')).findByRole('link', { name: /Sessions/ })).toHaveAttribute('href', '#/sessions');
  });

  test('the desktop sidebar honours the same picks, with the rest under More', async () => {
    localStorage.setItem('quack:bottom-bar', JSON.stringify(['remarkable', 'agents']));
    location.hash = '#/settings';
    render(<App />);
    const rail = screen.getAllByRole('navigation', { name: 'Main' })[0]!;
    const top = () => [...rail.querySelectorAll(':scope > a')].map((l) => l.textContent);
    expect(top()).toEqual(['Home', 'reMarkable', 'Agents']);
    expect([...rail.querySelectorAll('details a')].map((l) => l.textContent)).toEqual(['Sessions', 'Artifacts', 'Sleeper', 'Schedules', 'Contracts', 'Settings']);
    await userEvent.click(screen.getByRole('checkbox', { name: 'Sessions' }));
    expect(top()).toEqual(['Home', 'reMarkable', 'Agents', 'Sessions']);
  });

  test('on desktop, a page under More opens the sidebar More, so the current page shows', async () => {
    location.hash = '#/agents';
    routes['GET /agents'] = { agents: [] };
    render(<App />);
    const rail = screen.getAllByRole('navigation', { name: 'Main' })[0]!;
    expect(rail.querySelector('details')).toHaveAttribute('open');
    expect(within(rail).getByRole('link', { name: 'Agents' })).toHaveAttribute('aria-current', 'page');
  });

  test('allows three pages and no more', async () => {
    phoneAt('#/settings');
    render(<App />);
    expect(screen.getByRole('checkbox', { name: 'reMarkable' })).toBeDisabled();
    await userEvent.click(screen.getByRole('checkbox', { name: 'Artifacts' }));
    expect(screen.getByRole('checkbox', { name: 'reMarkable' })).toBeEnabled();
    await userEvent.click(screen.getByRole('checkbox', { name: 'reMarkable' }));
    expect(tabs()).toEqual(['Home', 'Sessions', 'Sleeper', 'reMarkable', 'More']);
  });

  test('reads a stored choice, ignoring pages that no longer exist', () => {
    localStorage.setItem('quack:bottom-bar', JSON.stringify(['remarkable', 'inbox', 'sessions']));
    phoneAt('#/');
    render(<App />);
    expect(tabs()).toEqual(['Home', 'reMarkable', 'Sessions', 'More']);
  });

  test('falls back to the default when storage throws, and still applies a change this session', async () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('blocked'); });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('blocked'); });
    phoneAt('#/settings');
    render(<App />);
    expect(tabs()).toEqual(['Home', 'Sessions', 'Artifacts', 'Sleeper', 'More']);
    await userEvent.click(screen.getByRole('checkbox', { name: 'Sleeper' }));
    expect(tabs()).toEqual(['Home', 'Sessions', 'Artifacts', 'More']);
  });
});

test('in a home: conversation the header reads quack and a confirm row shows what it will start', async () => {
  routes['GET /sessions'] = { sessions: [sess('home:h1', { title: 'Waivers', agent: 'quack' })] };
  routes['GET /sessions/home%3Ah1/approvals'] = { approvals: [{ id: 'a', tool: 'start', args: { summary: 'Process waivers for week 9' }, requestedAt: 1 }] };
  routes['GET /sessions/home%3Ah1/artifacts'] = { artifacts: [] };
  phoneAt('#/s/home%3Ah1');
  render(<App />);
  FakeSocket.all[0]!.open();
  const card = await screen.findByRole('region', { name: 'Approval' });
  expect(within(card).getAllByText('Process waivers for week 9')[0]).toBeInTheDocument();
  expect(screen.getByRole('heading', { name: 'quack' })).toBeInTheDocument();
});

test('archived sessions stay out of Home', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { title: 'Live one', running: true, active: true }), sess('gh:2', { title: 'Shelved', running: true, active: true, archived: true })] };
  phoneAt('#/');
  render(<App />);
  expect(await screen.findByRole('link', { name: /Live one/ })).toBeInTheDocument();
  expect(screen.queryByText(/Shelved/)).toBeNull();
});

test('the header shows the cost with tokens on hover when the model is priced, and never a $0', async () => {
  const ws = await openSession();
  ws.frame(snapshot({ usage: { models: { 'p/m': { input: 1200, output: 300, cacheRead: 5, cost: { total: 0.4162 } } }, tools: {} } }));
  const chip = await screen.findByText('$0.42');
  expect(chip).toHaveAttribute('title', '1,200 in · 300 out · 5 cached');
  ws.frame(snapshot({ usage: { models: { 'p/m': { input: 1200, output: 300, cost: { total: 0 } } }, tools: {} } }));
  expect(await screen.findByText('2k tokens')).toBeInTheDocument();
  expect(screen.queryByText(/\$/)).toBeNull();
});

test('session rows show the index spend: dollars when priced, tokens when not, nothing when idle', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { title: 'Priced' }), sess('gh:2', { title: 'Unpriced' }), sess('gh:3', { title: 'Fresh' })] };
  const tokens = (input: number) => ({ input, output: 0, cacheRead: 0, cacheWrite: 0 });
  routes['GET /index/sessions'] = { sessions: [{ key: 'gh:1', cost: 0.42, tokens: tokens(9000) }, { key: 'gh:2', cost: 0, tokens: tokens(12000) }, { key: 'gh:3', cost: 0, tokens: tokens(0) }] };
  render(<App />);
  expect(await screen.findByRole('link', { name: /Priced/ })).toHaveTextContent('$0.42');
  expect(screen.getByRole('link', { name: /Unpriced/ })).toHaveTextContent('12k tokens');
  expect(screen.getByRole('link', { name: /Fresh/ })).not.toHaveTextContent(/tokens|\$/);
});

describe('images', () => {
  const photo = new File(['x'], 'cat.png', { type: 'image/png' });

  beforeEach(() => {
    vi.stubGlobal('createImageBitmap', async () => ({ width: 4000, height: 1000, close() {} }));
    HTMLCanvasElement.prototype.getContext = (() => ({ fillRect() {}, drawImage() {} })) as never;
    HTMLCanvasElement.prototype.toDataURL = () => 'data:image/jpeg;base64,QUJD';
  });

  test('attaching shows a thumbnail with a remove button, and sending carries the image', async () => {
    const ws = await openSession();
    await userEvent.upload(screen.getByTestId('attach-input'), photo);
    expect(await screen.findByRole('img', { name: 'Attached image 1' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Remove image 1' }));
    expect(screen.queryByRole('img', { name: 'Attached image 1' })).toBeNull();
    await userEvent.upload(screen.getByTestId('attach-input'), photo);
    await screen.findByRole('img', { name: 'Attached image 1' });
    await userEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(ws.sent).toEqual([{ type: 'input', requestId: expect.any(String), input: '', images: [{ data: 'QUJD', mimeType: 'image/jpeg' }] }]);
    accept(ws);
    await waitFor(() => expect(screen.queryByRole('img', { name: 'Attached image 1' })).toBeNull());
  });

  test('a photo attached on Home seeds the home:main session and clears once accepted', async () => {
    render(<App />);
    await userEvent.type(await screen.findByRole('textbox', { name: 'What do you need?' }), 'what is this?');
    await userEvent.upload(screen.getByTestId('attach-input'), photo);
    await screen.findByRole('img', { name: 'Attached image 1' });
    await userEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(FakeSocket.all[0]?.url).toMatch(/\/sessions\/home%3Amain\/ws\?agent=quack$/));
    const sock = FakeSocket.all[0];
    expect(screen.getByRole('img', { name: 'Attached image 1' })).toBeInTheDocument();
    sock.open();
    await waitFor(() => expect(sock.sent).toEqual([{ type: 'input', requestId: expect.any(String), input: 'what is this?', images: [{ data: 'QUJD', mimeType: 'image/jpeg' }], whenBusy: 'followUp' }]));
    accept(sock);
    await waitFor(() => expect(screen.queryByRole('img', { name: 'Attached image 1' })).toBeNull());
  });

  test('answers that cross resolve the send they name', async () => {
    await startFromHome('review it');
    const sock = FakeSocket.all[0];
    sock.open();
    await waitFor(() => expect(sock.sent).toHaveLength(1));
    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), ' now');
    await userEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(sock.sent).toHaveLength(2);
    reject(sock, 'nope', 1);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled());
    expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('review it now');
    accept(sock, 0);
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue(''));
  });

  test('a send the server rejects keeps the text and the thumbnails', async () => {
    const ws = await openSession();
    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'what is this?');
    await userEvent.upload(screen.getByTestId('attach-input'), photo);
    await screen.findByRole('img', { name: 'Attached image 1' });
    await userEvent.click(screen.getByRole('button', { name: 'Send' }));
    reject(ws, 'Images need an image model: none');
    expect(await screen.findByRole('alert')).toHaveTextContent('Images need an image model');
    expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('what is this?');
    expect(screen.getByRole('img', { name: 'Attached image 1' })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled());
  });

  test('a fifth image is refused with a message', async () => {
    await openSession();
    await userEvent.upload(screen.getByTestId('attach-input'), [photo, photo, photo, photo, photo]);
    expect(await screen.findByRole('alert')).toHaveTextContent('At most 4 images per message.');
    expect(screen.getAllByRole('img')).toHaveLength(4);
  });

  test('a view with an image renders its thumbnail, and a tap opens it full size', async () => {
    const ws = await openSession();
    ws.frame(snapshot({ entries: [entry('e1', [{ role: 'user', content: [{ type: 'image', data: 'QUJD', mimeType: 'image/jpeg' }, { type: 'text', text: 'what is this?\n\n[Attached images attachments/1-1.jpg. You cannot see images.]' }], timestamp: 1 }])] }));
    const you = screen.getByRole('article', { name: 'You' });
    expect(you).toHaveTextContent('what is this?');
    expect(you).not.toHaveTextContent('Attached images');
    await userEvent.click(within(you).getByRole('button', { name: 'Open image 1' }));
    expect(screen.getByRole('img', { name: 'Full size' })).toHaveAttribute('src', 'data:image/jpeg;base64,QUJD');
    await userEvent.click(screen.getByRole('button', { name: 'Close image' }));
    expect(screen.queryByRole('img', { name: 'Full size' })).toBeNull();
  });
});

test('on desktop, #/sessions shows the list as the page, not Home, and the sidebar takes it back once a session opens', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { title: 'Alpha' })] };
  location.hash = '#/sessions';
  render(<App />);
  expect(await screen.findAllByRole('region', { name: 'Sessions' })).toHaveLength(1);
  expect(screen.getByRole('main')).toContainElement(screen.getByRole('region', { name: 'Sessions' }));
  expect(screen.queryByRole('region', { name: 'Running' })).toBeNull();
  location.hash = '#/s/gh%3A1';
  dispatchEvent(new HashChangeEvent('hashchange'));
  await waitFor(() => expect(screen.getByRole('main')).not.toContainElement(screen.getByRole('region', { name: 'Sessions' })));
});

test('a Home approval card says what it will do, from the args the index carries', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { title: 'Fix the typo' })] };
  const args = { summary: 'Push 1 commit(s) to fix-typo of o/r#1: docs: fix typo', commits: 'abc1234 docs: fix typo' };
  routes['GET /index/approvals?state=pending'] = { approvals: [{ session: 'gh:1', id: 'a', tool: 'deliver', args, state: 'pending', decision: null, by: null, at: 1 }] };
  phoneAt('#/');
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  // The headline, then again in the Details tree.
  expect(within(needs).getAllByText(args.summary)[0]).toBeInTheDocument();
  expect(within(needs).getByText(/abc1234 docs: fix typo/)).toBeInTheDocument();
});

test('a running session waiting for a model slot says so on its row, instead of looking like it works', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { title: 'Queued one', running: true, active: true, note: 'Waiting for a model slot (2 ahead)' }), sess('gh:2', { title: 'Working one', running: true, active: true })] };
  location.hash = '#/sessions';
  stubViewport(false);
  render(<App />);
  expect(await screen.findByRole('link', { name: /Queued one/ })).toHaveTextContent('Waiting for a model slot (2 ahead)');
  expect(screen.getByRole('link', { name: /Working one/ })).not.toHaveTextContent('Waiting for a model slot');
});

test('a contract session is named by its latest spec in the list, not by the text that started it', async () => {
  routes['GET /sessions'] = { sessions: [sess('contracts:c1', { title: 'The quack v1 distributor HTTP API (contract distributor.http in…' })] };
  routes['GET /index/artifacts'] = { artifacts: [{ session: 'contracts:c1', kind: 'contract.spec', version: 1, hash: 'h', verdict: { passed: true }, sealedAt: 1, summary: 'Distributor HTTP API, version 1', outcome: 'v1' }] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  expect(await screen.findByRole('link', { name: /Distributor HTTP API/ })).toHaveAttribute('href', '#/s/contracts%3Ac1');
  expect(screen.queryByText(/contract distributor\.http in/)).toBeNull();
});

test('the session menu names what it opens: Open review, not Work', async () => {
  routes['GET /sessions/ui%3Au1/artifacts'] = { artifacts: [{ kind: 'github.review', version: 1, hash: 'h', content: 'the review', sealedAt: 1, verdict: null }] };
  const ws = await openSession();
  ws.frame(snapshot());
  await userEvent.click(await screen.findByRole('button', { name: 'More actions' }));
  await userEvent.click(await screen.findByRole('menuitem', { name: 'Open review' }));
  expect(screen.getByRole('dialog', { name: 'Work' })).toBeInTheDocument();
  expect(screen.queryByRole('menuitem', { name: 'Work' })).toBeNull();
});

test('the session menu runs from harmless to destructive', async () => {
  routes['GET /sessions/ui%3Au1/artifacts'] = { artifacts: [{ kind: 'github.review', version: 1, hash: 'h', content: 'r', sealedAt: 1, verdict: null }] };
  routes['GET /sessions'] = { sessions: [sess('ui:u1', { title: 'Plan the trip' })] };
  const ws = await openSession();
  ws.frame(snapshot({ run: true }));
  await userEvent.click(await screen.findByRole('button', { name: 'More actions' }));
  const items = await screen.findAllByRole('menuitem');
  const names = items.map((i) => i.textContent);
  expect(names).toEqual(['Stop', 'Open review', 'Rename', 'Export', 'Archive', 'Delete']);
});

test('nothing above the nav duplicates Home: no link to #/new on desktop or on the phone Sessions header', async () => {
  routes['GET /sessions'] = { sessions: [] };
  location.hash = '#/sessions';
  const { unmount } = render(<App />);
  expect(document.querySelector('a[href="#/new"]')).toBeNull();
  unmount();
  stubViewport(false);
  render(<App />);
  expect(document.querySelector('a[href="#/new"]')).toBeNull();
});

test('a contract session reads its spec name on Home and in its header, and Changes requested on its row', async () => {
  const spec = { session: 'contracts:c2', kind: 'contract.spec', version: 1, hash: 'h', verdict: { passed: true }, sealedAt: 1, summary: 'Roster API, version 1', outcome: 'v1' };
  const changes = { session: 'contracts:c2', kind: 'contract.changes', version: 1, hash: 'c', verdict: null, sealedAt: 2, summary: 'Use cursor paging', outcome: 'v1' };
  routes['GET /sessions'] = { sessions: [sess('contracts:c2', { title: 'A long working title from the description' })] };
  routes['GET /index/artifacts'] = { artifacts: [spec, changes] };
  stubViewport(false);
  location.hash = '#/sessions';
  const { unmount } = render(<App />);
  const row = await screen.findByRole('link', { name: /Roster API/ });
  expect(row).toHaveTextContent('Changes requested');
  expect(row).not.toHaveTextContent('Spec ready');
  unmount();
  location.hash = '#/s/contracts%3Ac2';
  render(<App />);
  expect(await screen.findByRole('heading', { name: /Roster API/ })).toBeInTheDocument();
});
