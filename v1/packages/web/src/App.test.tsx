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

const sess = (key: string, over: object = {}) => ({ key, agent: 'reviewer', running: false, active: false, createdAt: '2026-11-01T01:00:00.000Z', lastActivity: '2026-11-01T01:00:00.000Z', ...over });

test('the list keeps the server order and labels rows by creation when an older session becomes active', async () => {
  const older = sess('gh:old', { title: 'Older', running: true, active: true, createdAt: '2026-11-01T01:00:00.000Z', lastActivity: '2026-11-01T09:00:00.000Z' });
  routes['GET /sessions'] = { sessions: [sess('gh:a', { title: 'Alpha', running: true, active: true, createdAt: '2026-11-01T03:00:00.000Z' }), sess('gh:new', { title: 'Newer', createdAt: '2026-11-01T02:00:00.000Z' }), older, sess('gh:z', { title: 'Zed', createdAt: '2026-11-01T00:00:00.000Z' })] };
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
  expect(screen.getByText('Worked · 1 tool · bash').closest('details')).not.toHaveAttribute('open');
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
  const group = screen.getByText('Worked · 2 tools · web_search, bash').closest('details')!;
  expect(group).not.toHaveAttribute('open');
  expect(within(group).getByText('weighing options')).toBeInTheDocument();

  const search = within(group).getByText('web_search').closest('details')!;
  expect(within(search.querySelector('summary')!).getByText('quack ducks')).toBeInTheDocument();
  expect(within(search.querySelector('summary')!).getByText('3 k chars')).toBeInTheDocument();
  expect(search).not.toHaveAttribute('open');
  await userEvent.click(within(search).getByText('web_search'));
  expect(search).toHaveAttribute('open');
  expect(search).toHaveTextContent('"limit": 3');
  const out = search.querySelectorAll('pre')[1]!;
  expect(out.textContent).toHaveLength(2003);
  await userEvent.click(within(search).getByRole('button', { name: 'Show all (2500 characters)' }));
  expect(search.querySelectorAll('pre')[1]!.textContent).toHaveLength(2500);

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
  expect(submit.querySelectorAll('pre')[1]).toHaveTextContent('## Takeaway');
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
  expect(ws.sent).toEqual([{ type: 'input', input: 'first' }]);

  ws.frame({ type: 'events', events: [{ type: 'run_start', inputs: [] }] });
  expect(screen.queryByRole('group', { name: 'While the agent is working' })).toBeNull();
  expect(box()).toHaveAttribute('placeholder', 'Message · sends after this step');
  await send('turn left');
  expect(ws.sent[1]).toEqual({ type: 'input', input: 'turn left', whenBusy: 'followUp' });
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
  await userEvent.click(screen.getByRole('button', { name: 'More actions' }));
  await userEvent.click(screen.getByRole('menuitem', { name: 'Abort' }));
  expect(fetchMock).toHaveBeenCalledWith('sessions/ui%3Au1/abort', { method: 'POST' });
  ws.frame({ type: 'events', events: [{ type: 'run_end', inputs: [] }] });
  expect(screen.queryByRole('button', { name: 'Abort' })).toBeNull();
});

test('a failed abort is reported', async () => {
  const ws = await openSession();
  ws.frame(snapshot({ run: {} }));
  await userEvent.click(screen.getByRole('button', { name: 'More actions' }));
  await userEvent.click(screen.getByRole('menuitem', { name: 'Abort' }));
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
  expect(within(rail).getAllByRole('link').map((l) => l.getAttribute('href'))).toEqual(['#/', '#/inbox', '#/artifacts', '#/sleeper', '#/remarkable']);
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

test('the landing page offers the suggestions its extensions supply and fills the composer with their prompt', async () => {
  routes['GET /agents'] = { agents: [{ name: 'reviewer', version: 2 }] };
  location.hash = '#/new';
  render(<App />);
  await userEvent.click(await screen.findByRole('button', { name: 'Research a question' }));
  expect(screen.getByRole('textbox', { name: 'Question or task' })).toHaveValue('Research: ');
  expect(screen.getByRole('button', { name: 'Review a pull request' })).toBeInTheDocument();
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
  expect(post()).toEqual({ decision: 'approve' });

  await userEvent.click(screen.getByRole('button', { name: 'Reject github_post_review' }));
  await userEvent.type(screen.getByRole('textbox', { name: 'Reason' }), 'wrong repo');
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  await userEvent.click(screen.getByRole('button', { name: 'Confirm reject github_post_review' }));
  expect(post()).toEqual({ decision: 'reject', reason: 'wrong repo' });
  await screen.findByText('Nothing is waiting on you');
});

const pendingDecision = { questions: [{ session: 'gh:3', id: '7:type', question: 'What kind of document is this?', options: ['meeting', 'idea'], evidence: 'Agenda: standup', state: 'pending', choice: null, by: null, at: 1 }] };

test('the Inbox shows a workflow question with its options, and an option answers it', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Campaign Notes' })] };
  routes['GET /index/approvals?state=pending'] = { approvals: [] };
  routes['GET /index/questions?state=pending'] = pendingDecision;
  routes['POST /sessions/gh%3A3/questions/7%3Atype'] = { answered: true };
  location.hash = '#/inbox';
  render(<App />);
  expect(await screen.findByText('What kind of document is this?')).toBeInTheDocument();
  expect(screen.getByText('1 waiting for your answer')).toBeInTheDocument();
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
  location.hash = '#/inbox';
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
  location.hash = '#/inbox';
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
  expect(within(needs).getByText('Answer: What kind of document is this?')).toBeInTheDocument();
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
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByText('Run failed at decide · Validation failed for tool "decide":')).toBeInTheDocument();
  expect(screen.getByText('1 failed · 0 need you · 0 running')).toBeInTheDocument();
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
  const open = await screen.findByRole('button', { name: 'Open Old work' });
  expect(open).toHaveTextContent('Double-checked');
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
  ws.frame({ type: 'events', events: [{ type: 'run_start', inputs: [] }] });
  const line = (await screen.findByText('Previous review')).closest('li')!;
  expect(within(line).queryByRole('img', { name: 'Double-checked' })).toBeNull();
  expect(within(line).queryByRole('region', { name: 'Review' })).toBeNull();
  expect(line).not.toHaveTextContent('Double-checked');
  ws.frame({ type: 'events', events: [{ type: 'run_end', inputs: [] }] });
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
