import { act, cleanup, fireEvent, render, renderHook, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { App } from './App.tsx';
import { parseArtifactRoute } from './ArtifactPage.tsx';
import { artifactHref } from './extension.ts';
import { CHAT_DEFAULT, CHAT_MIN, clampChat, PANE_MIN, useSplit } from './Split.tsx';
import { ask, assistant, entry, failedRun, FakeEventSource, FakeSocket, Fail, fetchMock, item, live, routes, sess, snapshot, stubViewport, useHarness, usage, view } from './test-harness.ts';

const requestOf = (sock: { sent: unknown[] }, i: number) => (sock.sent.at(i) as { requestId: string }).requestId;
const accept = (sock: FakeSocket, i = -1) => sock.frame({ type: 'accepted', requestId: requestOf(sock, i) });
const reject = (sock: FakeSocket, error: string, i = -1) => sock.frame({ type: 'error', error, requestId: requestOf(sock, i) });

// GET /asks is left unrouted: Home reads it only when a row says something is pending, and a test that expects it routes it.
useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /artifacts': { artifacts: [] } }));

// A list read with q, as the route the fake fetch answers.
const listed = (path: string, q: string, token?: string) => `GET /${path}?${new URLSearchParams({ pageSize: '1000', q, ...(token && { pageToken: token }) })}`;
const ACTIVE = 'NOT state:archived';
const queries = (path: string) => fetchMock.mock.calls.map(([u]) => new URL(String(u), 'http://x')).filter((u) => u.pathname === `/${path}`).map((u) => u.searchParams.get('q'));

// Types one character per change, faster than the debounce, then lets the pause elapse on a fake clock.
function typeQuickly(box: HTMLElement, text: string) {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  try {
    for (let i = 1; i <= text.length; i++) fireEvent.change(box, { target: { value: text.slice(0, i) } });
    act(() => void vi.advanceTimersByTime(250));
  } finally {
    vi.useRealTimers();
  }
}

async function openSession() {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0];
  ws.open();
  return ws;
}


test('the list is newest-created first however the server pages it, and labels rows by creation when an older session becomes active', async () => {
  const older = sess('gh:old', { title: 'Older', state: 'running', createdAt: '2026-11-01T01:00:00.000Z', lastActivity: '2026-11-01T09:00:00.000Z' });
  routes['GET /sessions'] = { sessions: [older, sess('gh:a', { title: 'Alpha', state: 'running', createdAt: '2026-11-01T03:00:00.000Z' }), sess('gh:z', { title: 'Zed', createdAt: '2026-11-01T00:00:00.000Z' }), sess('gh:new', { title: 'Newer', createdAt: '2026-11-01T02:00:00.000Z' })] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const running = await screen.findByRole('region', { name: 'Running' });
  expect(within(running).getAllByRole('link').map((l) => l.getAttribute('href'))).toEqual(['#/s/gh%3Aa', '#/s/gh%3Aold']);
  expect(within(screen.getByRole('region', { name: 'Recent' })).getAllByRole('link').map((l) => l.getAttribute('href'))).toEqual(['#/s/gh%3Anew', '#/s/gh%3Az']);
  expect(within(running).getByRole('link', { name: /Older/ }).querySelector('time')).toHaveAttribute('datetime', '2026-11-01T01:00:00.000Z');
});

test('sessions past the first page are listed: the pages are followed to the end', async () => {
  routes[listed('sessions', ACTIVE)] = { sessions: [sess('gh:1', { title: 'First page' })], nextPageToken: 'p2' };
  routes[listed('sessions', ACTIVE, 'p2')] = { sessions: [sess('gh:2', { title: 'Second page' })] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  expect(await screen.findByRole('link', { name: /First page/ })).toBeInTheDocument();
  expect(await screen.findByRole('link', { name: /Second page/ })).toBeInTheDocument();
});

test('groups sessions from the row alone: a running session with a pending ask lands in Needs you', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { state: 'running', title: 'Fix the flake', pendingAsks: 1, oldestAskAt: iso(1) }), sess('gh:2', { state: 'running' }), sess('gh:3', { title: 'Old work' })] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByRole('link', { name: /Fix the flake/ })).toHaveAttribute('href', '#/s/gh%3A1');
  expect(within(needs).getByText('Waiting on you')).toBeInTheDocument();
  expect(fetchMock.mock.calls.map(([u]) => String(u).split('?')[0])).not.toContain('asks');
  expect(within(screen.getByRole('region', { name: 'Running' })).getByRole('link', { name: /gh:2/ })).toBeInTheDocument();
  expect(within(screen.getByRole('region', { name: 'Recent' })).getByRole('link', { name: /Old work/ })).toHaveTextContent('reviewer');
});

test('a GitHub key shows as #n with its title, and the full key stays as the tooltip', async () => {
  routes['GET /sessions'] = { sessions: [sess('github:fagerbergj/quack#1885', { title: 'Review the shell' })] };
  render(<App />);
  const link = await screen.findByRole('link', { name: /#1885 Review the shell/ });
  expect(link).toHaveAttribute('title', 'Review the shell\ngithub:fagerbergj/quack#1885');
});

test('review rows carry one PR badge each from the mirror, neutral when the service is not connected', async () => {
  const pr = (n: number, over: object) => ({ number: n, state: 'open', merged: false, merged_at: null, closed_at: null, html_url: `https://github.com/A/One/pull/${n}`, title: 't', ...over });
  routes['GET /sessions'] = { sessions: [sess('github:a/one#1', { title: 'fix: first' }), sess('github:a/one#2', { title: 'fix: second' }), sess('github:a/one#3', { title: 'fix: third' }), sess('gh:9', { title: 'Plain' })] };
  routes['GET /apps/github/repos/a/one/pulls/1'] = pr(1, {});
  routes['GET /apps/github/repos/a/one/pulls/2'] = pr(2, { state: 'closed', merged: true, merged_at: '2026-11-02T00:00:00Z', closed_at: '2026-11-02T00:00:00Z' });
  routes['GET /apps/github/repos/a/one/pulls/4'] = pr(4, { state: 'closed', closed_at: '2026-11-01T00:00:00Z' });
  const view = render(<App />);
  // Before the read answers, a badge is neutral and links by the session key.
  expect(await screen.findByRole('link', { name: 'Pull request #1 on GitHub' })).toHaveAttribute('href', 'https://github.com/a/one/pull/1');
  await waitFor(() => expect(FakeEventSource.all).toHaveLength(1));
  FakeEventSource.all[0].open();
  expect(await screen.findByRole('link', { name: 'Pull request #2, merged, on GitHub' })).toHaveTextContent(/^#2$/);
  // Once the mirror answers, the badge links to GitHub's html_url.
  expect(screen.getByRole('link', { name: 'Pull request #1, open, on GitHub' })).toHaveAttribute('href', 'https://github.com/A/One/pull/1');
  expect(screen.getByRole('link', { name: 'Pull request #3 on GitHub' })).toHaveAttribute('href', 'https://github.com/a/one/pull/3');
  expect(screen.getAllByRole('link', { name: /on GitHub$/ })).toHaveLength(3);
  // The badge shows the number, so the title keeps it for screen readers only.
  expect(within(screen.getByRole('link', { name: /#1 First/ })).getByText('#1')).toHaveClass('sr-only');
  view.unmount();
  expect(FakeEventSource.all[0].readyState, 'the stream closes with the last badge').toBe(2);
  routes['GET /sessions'] = { sessions: [sess('github:a/one#4', { title: 'fix: fourth' })] };
  render(<App />);
  await waitFor(() => expect(FakeEventSource.all).toHaveLength(2));
  FakeEventSource.all[1].open();
  expect(await screen.findByRole('link', { name: 'Pull request #4, closed, on GitHub' })).toBeInTheDocument();
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

const iso = (ms: number) => new Date(ms).toISOString();
const ran = (outcome = 'succeeded') => ({ lastRun: { outcome, endedAt: iso(1), failedSteps: [] } });

test('each kind of session shows its own title and how its last run ended', async () => {
  routes['GET /sessions'] = { sessions: [
    sess('github:o/r#7', { title: 'Fix the cache', state: 'running', ...ran() }),
    sess('github:o/r#8', { title: 'Add retries', ...ran() }),
    sess('github:o/r#9', { title: 'Drop the flag' }),
    sess('ui:r1', { agent: 'researcher', title: 'What changed in v4?', ...ran() }),
    ...['lineup', 'waivers', 'history', 'trends', 'trade-finder', 'trade:4:12+13:99', 'digest', 'retro'].map((job) => sess(`sleeper:123:8:${job}`, { agent: 'sleeper-x', ...ran() })),
    sess('remarkable:doc1', { agent: 'remarkable-ingest', title: 'Campaign Notes', ...ran('aborted') }),
    sess('ui:x', { agent: 'coder' }),
    sess('github:o/r#10', { state: 'idle', title: 'Review pull request o/r#10 (label quack:review). Title: feat(v1): Sleeper waivers job', ...ran() }),
  ] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const row = async (name: RegExp) => within(await screen.findByRole('link', { name }));
  expect((await row(/#7 Fix the cache/)).getByText('Reviewing')).toBeInTheDocument();
  expect((await row(/#8 Add retries/)).getByText('Reviewed')).toBeInTheDocument();
  expect((await row(/#9 Drop the flag/)).getByText('Not reviewed yet')).toBeInTheDocument();
  expect((await row(/What changed/)).getByText('Report ready')).toBeInTheDocument();
  expect((await row(/Lineup · wk 8/)).getByText('Lineup picks ready')).toBeInTheDocument();
  expect((await row(/Waivers · wk 8/)).getByText('Waiver claims ready')).toBeInTheDocument();
  expect((await row(/^History/)).getByText('League history ready')).toBeInTheDocument();
  expect((await row(/^Trends · wk 8/)).getByText('Trends ready')).toBeInTheDocument();
  expect((await row(/^Trade finder · wk 8/)).getByText('Trade ideas ready')).toBeInTheDocument();
  expect((await row(/^Trade · wk 8/)).getByText('Trade ideas ready')).toBeInTheDocument();
  expect((await row(/^Recap · wk 8/)).getByText('Week 8 recap ready')).toBeInTheDocument();
  expect((await row(/^Retro · wk 8/)).getByText('Week 8 retro ready')).toBeInTheDocument();
  expect((await row(/Campaign Notes/)).getByText('Stopped')).toBeInTheDocument();
  const idle = await row(/#10 Sleeper waivers job/);
  expect(idle.getByText('Reviewed')).toBeInTheDocument();
  expect(within(screen.getByRole('region', { name: 'Recent' })).getByRole('link', { name: /#10 Sleeper/ })).toBeInTheDocument();
  expect((await row(/ui:x/)).getByText('coder')).toBeInTheDocument();
});

test('search and the filters narrow the list on the server, by q', async () => {
  const flake = sess('github:1', { title: 'Fix the flake' });
  const sandboxes = sess('ui:2', { agent: 'researcher', title: 'Compare sandboxes' });
  routes['GET /sessions'] = { sessions: [flake, sandboxes] };
  routes[listed('sessions', `${ACTIVE} AND sandbox*`)] = { sessions: [sandboxes] };
  routes[listed('sessions', `${ACTIVE} AND key:github\\:*`)] = { sessions: [flake] };
  render(<App />);
  await screen.findByRole('link', { name: /Fix the flake/ });
  typeQuickly(screen.getByRole('searchbox', { name: 'Search sessions' }), 'sandbox');
  await waitFor(() => expect(screen.queryByRole('link', { name: /Fix the flake/ })).toBeNull());
  expect(screen.getByRole('link', { name: /Compare sandboxes/ })).toBeInTheDocument();
  await userEvent.clear(screen.getByRole('searchbox', { name: 'Search sessions' }));
  await userEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Started by' }), 'GitHub');
  await waitFor(() => expect(screen.queryByRole('link', { name: /Compare sandboxes/ })).toBeNull());
  expect(screen.getByRole('link', { name: /Fix the flake/ })).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Remove Started by: GitHub' })).toBeInTheDocument();
});

test('quick typing sends one search request, after the pause, and clearing applies at once', async () => {
  render(<App />);
  await waitFor(() => expect(queries('sessions')).toContain(ACTIVE));
  const box = screen.getByRole('searchbox', { name: 'Search sessions' });
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  try {
    for (const v of ['f', 'fl', 'fla', 'flak', 'flake']) fireEvent.change(box, { target: { value: v } });
    act(() => void vi.advanceTimersByTime(249));
    expect(queries('sessions').filter((q) => q?.includes('*'))).toEqual([]);
    act(() => void vi.advanceTimersByTime(1));
    expect(queries('sessions').filter((q) => q?.includes('*'))).toEqual([`${ACTIVE} AND flake*`]);
    const before = queries('sessions').length;
    fireEvent.change(box, { target: { value: '' } });
    expect(queries('sessions').slice(before)).toEqual([ACTIVE]);
  } finally {
    vi.useRealTimers();
  }
});

test('selecting a session opens it in the main pane and marks its row', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1', { title: 'Fix the flake' })] };
  render(<App />);
  await userEvent.click(await screen.findByRole('link', { name: /Fix the flake/ }));
  expect(await screen.findByRole('heading', { name: 'Fix the flake' })).toBeInTheDocument();
  expect(FakeSocket.all[0].url).toMatch(/\/sessions\/ui%3Au1\/ws$/);
  expect(screen.getByRole('link', { name: /Fix the flake/ })).toHaveAttribute('aria-current', 'true');
});

const homeExists = () => (routes['GET /sessions'] = { sessions: [sess('home:main')] });

async function startFromHome(text: string) {
  homeExists();
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

test('the first-ever Home message creates home:main by invocation before any socket opens', async () => {
  routes['POST /sessions/home%3Amain/invocations?agent=quack'] = { status: 'accepted' };
  render(<App />);
  await userEvent.type(await screen.findByRole('textbox', { name: 'What do you need?' }), 'hello');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  await waitFor(() => expect(FakeSocket.all[0]?.url).toMatch(/\/sessions\/home%3Amain\/ws\?agent=quack$/));
  const post = fetchMock.mock.calls.find(([u, i]) => i?.method === 'POST' && u.includes('home%3Amain/invocations'));
  expect(JSON.parse(String(post?.[1]?.body))).toMatchObject({ input: 'hello' });
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('');
});

test('a double click on the first-ever Home send creates the session once', async () => {
  routes['POST /sessions/home%3Amain/invocations?agent=quack'] = { status: 'accepted' };
  render(<App />);
  await userEvent.type(await screen.findByRole('textbox', { name: 'What do you need?' }), 'hello');
  const send = screen.getByRole('button', { name: 'Send' });
  await act(async () => { send.click(); send.click(); });
  await waitFor(() => expect(FakeSocket.all).toHaveLength(1));
  expect(fetchMock.mock.calls.filter(([u, i]) => i?.method === 'POST' && u.includes('home%3Amain/invocations'))).toHaveLength(1);
});

test('the first Home send asks only for home:main, by an escaped key term', async () => {
  routes['POST /sessions/home%3Amain/invocations?agent=quack'] = { status: 'accepted' };
  render(<App />);
  await userEvent.type(await screen.findByRole('textbox', { name: 'What do you need?' }), 'hello');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  await waitFor(() => expect(FakeSocket.all).toHaveLength(1));
  const queried = fetchMock.mock.calls.map(([u]) => new URL(String(u), 'http://x')).filter((u) => u.pathname === '/sessions' && u.searchParams.has('q'));
  expect(queried.map((u) => u.searchParams.get('q')).filter((q) => q !== ACTIVE), 'besides the sidebar list').toEqual(['key:home\\:main']);
  expect(fetchMock.mock.calls.some(([u]) => String(u).includes('filter='))).toBe(false);
});

test('a first send the distributor refuses shows the error and keeps the text', async () => {
  routes['POST /sessions/home%3Amain/invocations?agent=quack'] = new Fail(409, { title: 'Conflict', detail: 'refused by the server' });
  render(<App />);
  await userEvent.type(await screen.findByRole('textbox', { name: 'What do you need?' }), 'hello');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  expect(await screen.findByRole('alert')).toHaveTextContent('refused by the server');
  expect(screen.getByRole('textbox', { name: 'What do you need?' })).toHaveValue('hello');
  expect(FakeSocket.all).toEqual([]);
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
  (live['ui%3Au1'] ??= {}).artifacts = [{ kind: 'github.review', version: 1, hash: 'h', content: 'x', sealedAt: 1, verdict: { passed: true, gatesFailed: [], score: 1, checks: [] }, links: [{ label: 'Posted', url: 'https://github.com/o/r/pull/1#pullrequestreview-9' }] }];
  const ws = await openSession();
  ws.frame(snapshot());
  const link = await screen.findByRole('link', { name: 'Posted ↗' });
  expect(link).toHaveAttribute('href', 'https://github.com/o/r/pull/1#pullrequestreview-9');
  expect(link).toHaveAttribute('rel', expect.stringContaining('noreferrer'));
});

test('a Sleeper card in a session links back to that session, in the list and the Work drawer', async () => {
  const lineup = (await import('./extensions/sleeper/fixtures/8-lineup.json')).default as { content: string };
  (live['ui%3Au1'] ??= {}).artifacts = [{ kind: 'sleeper.cards', version: 1, hash: 'h', content: lineup.content, sealedAt: 1, verdict: null }];
  const ws = await openSession();
  ws.frame(snapshot());
  const asks = await screen.findAllByRole('link', { name: 'Ask about this' });
  for (const a of asks) expect(a).toHaveAttribute('href', '#/s/ui%3Au1');
});

test('the header shows the token total and Waiting on you for a pending approval', async () => {
  (live['ui%3Au1'] ??= {}).approvals = [{ id: 'a', tool: 'bash', args: {}, requestedAt: 1 }];
  const ws = await openSession();
  ws.frame(snapshot({ usage: { models: { 'p/m': { input: 1200, output: 300 } }, tools: {} } }));
  expect(await screen.findByRole('status')).toHaveTextContent('Waiting on you');
  expect(screen.getByText('2k tokens')).toBeInTheDocument();
});

test("a GitHub session's menu opens the pull request in a new tab", async () => {
  (live['github%3Ao%2Fr%231'] ??= {}).approvals = [];
  (live['github%3Ao%2Fr%231'] ??= {}).artifacts = [];
  location.hash = '#/s/github%3Ao%2Fr%231';
  render(<App />);
  await userEvent.click(await screen.findByRole('button', { name: 'More actions' }));
  const item = screen.getByRole('menuitem', { name: 'Open on GitHub' });
  expect(item).toHaveAttribute('href', 'https://github.com/o/r/pull/1');
  expect(item).toHaveAttribute('target', '_blank');
  expect(item).not.toHaveAttribute('download');
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
  (live[`${key}`] ??= {}).artifacts = [];
  (live[`${key}`] ??= {}).approvals = [];
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
  (live['ui%3Au1'] ??= {}).approvals = [];
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
  routes['GET /sessions'] = new Fail(503, { title: 'Unavailable', detail: 'index down' });
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
  routes['POST /sessions/ui%3Au1/abort'] = new Fail(409, { title: 'Conflict', detail: 'refused by the server' });
  const ws = await openSession();
  ws.frame(snapshot({ run: {} }));
  await userEvent.click(screen.getByRole('button', { name: 'More actions' }));
  await userEvent.click(screen.getByRole('menuitem', { name: 'Stop' }));
  expect(await screen.findByRole('alert')).toHaveTextContent('refused by the server');
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
  (live['ui%3Au1'] ??= {}).artifacts = [
      { kind: 'github.review', version: 1, hash: 'h1', content: 'draft one', sealedAt: 1, verdict: { passed: false, gatesFailed: ['cites.lines'], score: 0.4, checks: [check('cites', 'lines', 0), check('tone', 'polite', 1), check('misc', 'na', null)] } },
      { kind: 'github.review', version: 2, hash: 'h2', content: 'final review', sealedAt: 2, verdict: { passed: true, gatesFailed: [], score: 1, checks: [check('cites', 'lines', 1)] } },
    ];
  const ws = await openSession();
  ws.frame(snapshot());
  const section = await screen.findByRole('log', { name: 'Transcript' });
  await waitFor(() => expect(ledgerOf(section)).toHaveLength(2));
  const items = ledgerOf(section);
  expect(items).toHaveLength(2);
  expect(items[0]).toHaveTextContent("Review didn't pass its checks");
  expect(items[1]).toHaveTextContent(/Review ready/);
  expect(items[1]).not.toHaveTextContent(/Cards|github\.review|v2|sealed/);
  // The pane beside the chat shows the newest version; Open on an older line shows that one there.
  const pane = screen.getByRole('region', { name: 'Review' });
  expect(within(pane).getByText('final review')).toBeInTheDocument();
  expect(section).not.toHaveTextContent('final review');
  await userEvent.click(within(items[0]).getByRole('button', { name: 'Open Review' }));
  expect(within(pane).getByText('draft one')).toBeInTheDocument();
  expect(within(pane).getByRole('button', { name: "Version: Version 1, didn't pass" })).toBeInTheDocument();
  await userEvent.click(within(pane).getByRole('button', { name: 'Follow the newest version' }));
  expect(within(pane).getByText('final review')).toBeInTheDocument();
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

const REPORT = { kind: 'research.report', version: 2, hash: 'h2', content: '## Short answer\n\nGain is 1.9x [S9].', sealedAt: 2, verdict: { passed: false, gatesFailed: [], score: 0.5, checks: [{ criterion: 'citations', id: 'support', label: 'Cites a source that does not say it', weight: 3, met: 0, notes: ['S7 does not mention TP'] }, { criterion: 'dated', id: 'claims', label: 'A claim has no date', weight: 2, met: 1, notes: [] }] } };

test('a session shows chat full width until its first version seals, then that version beside the chat with its checks below', async () => {
  const ws = await openSession();
  ws.frame(snapshot());
  await screen.findByRole('log', { name: 'Transcript' });
  expect(screen.queryByRole('region', { name: 'Report' })).toBeNull();
  expect(screen.queryByRole('separator')).toBeNull();
  ws.frame({ type: 'artifacts', artifacts: [REPORT] });
  const pane = await screen.findByRole('region', { name: 'Report' });
  expect(within(pane).getByRole('heading', { name: 'Short answer' })).toBeInTheDocument();
  expect(screen.getByRole('log', { name: 'Transcript' })).not.toHaveTextContent('Gain is 1.9x');
  expect(within(pane).getByRole('button', { name: "Version: Version 2, didn't pass" })).toBeInTheDocument();
  expect(pane).toHaveTextContent('Cites a source that does not say it');
  expect(pane).toHaveTextContent('S7 does not mention TP');
  expect(pane).not.toHaveTextContent('citations.support');
  expect(screen.getByRole('separator', { name: 'Resize the chat' })).toBeInTheDocument();
  expect(screen.queryByRole('dialog')).toBeNull();
});

test('the divider resizes the chat from the keyboard within its bounds, and the width is remembered in this browser', async () => {
  localStorage.removeItem('quack.split.chat');
  (live['ui%3Au1'] ??= {}).artifacts = [REPORT];
  const ws = await openSession();
  ws.frame(snapshot());
  const bar = await screen.findByRole('separator', { name: 'Resize the chat' });
  expect(bar).toHaveAttribute('aria-valuenow', String(CHAT_DEFAULT));
  bar.focus();
  await userEvent.keyboard('{ArrowRight}{ArrowRight}');
  expect(bar).toHaveAttribute('aria-valuenow', String(CHAT_DEFAULT + 48));
  await userEvent.keyboard('{Home}{ArrowLeft}');
  expect(bar).toHaveAttribute('aria-valuenow', String(CHAT_MIN));
  expect(localStorage.getItem('quack.split.chat')).toBe(String(CHAT_MIN));
  cleanup();
  render(<App />);
  FakeSocket.all.at(-1)!.open();
  expect(await screen.findByRole('separator', { name: 'Resize the chat' })).toHaveAttribute('aria-valuenow', String(CHAT_MIN));
});

test('the chat width stays between its minimum and what leaves the pane its own, and a blocked store keeps the default', () => {
  expect(clampChat(100, 1600)).toBe(CHAT_MIN);
  expect(clampChat(1500, 1600)).toBe(1600 - PANE_MIN);
  expect(clampChat(500.4, 1600)).toBe(500);
  expect(clampChat(900)).toBe(900);
  vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('blocked'); });
  try {
    const { result } = renderHook(() => useSplit());
    expect(result.current.separator['aria-valuenow']).toBe(CHAT_DEFAULT);
  } finally {
    vi.restoreAllMocks();
  }
});

test('on a phone a session with a version opens on its tab, named for it; a pending question opens it on Chat, counted there', async () => {
  stubViewport(false);
  (live['ui%3Au1'] ??= {}).artifacts = [REPORT];
  const ws = await openSession();
  ws.frame(snapshot());
  expect(await screen.findByRole('tab', { name: 'Report' })).toHaveAttribute('aria-selected', 'true');
  expect(screen.getByRole('tab', { name: 'Chat' })).toHaveAttribute('aria-selected', 'false');
  ws.frame({ type: 'standing', questions: [{ id: '7:type', question: 'Which source wins?', options: ['a', 'b'], evidence: null, askedAt: 1 }], failures: [], approvals: [] });
  expect(await screen.findByRole('tab', { name: 'Chat 1' })).toHaveAttribute('aria-selected', 'true');
  expect(screen.getByRole('region', { name: 'Question' })).toHaveTextContent('Which source wins?');
  // Once the person picks a tab it stays picked.
  await userEvent.click(screen.getByRole('tab', { name: 'Report' }));
  expect(screen.getByRole('tab', { name: 'Report' })).toHaveAttribute('aria-selected', 'true');
});

test('Open on a ledger line shows that version on the pane tab on a phone', async () => {
  stubViewport(false);
  (live['ui%3Au1'] ??= {}).artifacts = [{ ...REPORT, version: 1, hash: 'h1', content: 'first take', sealedAt: 1 }, REPORT];
  (live['ui%3Au1'] ??= {}).approvals = [{ id: 'a', tool: 'bash', args: {}, requestedAt: 1 }];
  const ws = await openSession();
  ws.frame(snapshot());
  expect(await screen.findByRole('tab', { name: 'Chat 1' })).toHaveAttribute('aria-selected', 'true');
  await userEvent.click(screen.getByRole('button', { name: 'Open Report' }));
  expect(screen.getByRole('tab', { name: 'Report' })).toHaveAttribute('aria-selected', 'true');
  expect(within(screen.getByRole('region', { name: 'Report' })).getByText('first take')).toBeInTheDocument();
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

const pendingAsks = { asks: [ask('gh:3', 'a/1', { createdAt: iso(1), toolCall: { tool: 'github_post_review', args: {} } })] };
// A row with one ask waiting since `at`; Home reads the asks themselves only when a row says so.
const waiting = (key: string, over: object = {}, at = 1) => sess(key, { pendingAsks: 1, oldestAskAt: iso(at), ...over });
const asksRead = () => fetchMock.mock.calls.map(([u]) => new URL(String(u), 'http://x')).filter((u) => u.pathname === '/asks');
const goTo = (hash: string) => {
  location.hash = hash;
  act(() => void dispatchEvent(new HashChangeEvent('hashchange')));
};

test('an idle session waiting on approval is in Needs you on the list, and Home reads it from one pending-asks query', async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:3', { title: 'Old work' })] };
  routes['GET /asks'] = pendingAsks;
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByText('Waiting on you')).toBeInTheDocument();
  expect(asksRead()).toEqual([]);
  goTo('#/');
  const home = await screen.findByRole('region', { name: 'Approval' });
  expect(within(home).getByRole('link', { name: 'Old work wants to continue' })).toHaveAttribute('href', '#/s/gh%3A3');
  expect(within(home).getByText('github_post_review', { exact: false })).not.toBeVisible();
  expect(asksRead().map((u) => u.searchParams.get('q'))).toEqual(['state:pending']);
  expect(fetchMock.mock.calls.some(([u]) => /index\/|\/approvals|\/questions/.test(String(u)))).toBe(false);
});

test('Home reads no asks while no row has one pending', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Old work', state: 'running' })] };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  expect(await screen.findByRole('region', { name: 'Running' })).toBeInTheDocument();
  expect(screen.queryByRole('region', { name: 'Needs you' })).toBeNull();
  expect(asksRead()).toEqual([]);
});

const homeApprovals = async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:3')] };
  routes['GET /asks'] = pendingAsks;
  routes['PATCH /sessions/gh%3A3/asks/a%2F1'] = { ...pendingAsks.asks[0], state: 'resolved' };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  return screen.findByRole('button', { name: 'Approve gh:3' });
};
const patched = () => fetchMock.mock.calls.filter(([, i]) => i?.method === 'PATCH').map(([u, i]) => [u, JSON.parse(String(i!.body))]);
const lastPost = () => patched().at(-1)![1];

test('Home approve resolves the ask on its session, and the card goes before the next read', async () => {
  await userEvent.click(await homeApprovals());
  expect(patched()).toEqual([['sessions/gh%3A3/asks/a%2F1', { resolution: { choice: 'approve' } }]]);
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Approval' })).toBeNull());
  expect(fetchMock.mock.calls.some(([u, i]) => i?.method === 'POST' && /approvals/.test(String(u)))).toBe(false);
});

test('Home reject sends its reason with the resolution', async () => {
  await homeApprovals();
  await userEvent.click(screen.getByRole('button', { name: 'Reject gh:3' }));
  await userEvent.type(screen.getByRole('textbox', { name: 'Reason' }), 'wrong repo');
  await userEvent.click(screen.getByRole('button', { name: 'Confirm reject gh:3' }));
  expect(lastPost()).toEqual({ resolution: { choice: 'reject', reason: 'wrong repo' } });
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Approval' })).toBeNull());
});

test('a decision the session says is unknown or already decided removes the stale card', async () => {
  const approve = await homeApprovals();
  fetchMock.mockImplementationOnce(async () => new Response(JSON.stringify({ error: 'unknown or already decided' }), { status: 409, headers: { 'content-type': 'application/json' } }));
  await userEvent.click(approve);
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Approval' })).toBeNull());
});

test('a failed decision keeps the card with its error', async () => {
  const approve = await homeApprovals();
  fetchMock.mockImplementationOnce(async () => new Response(JSON.stringify({ error: 'session is down' }), { status: 503, headers: { 'content-type': 'application/json' } }));
  await userEvent.click(approve);
  expect(await screen.findByRole('alert')).toHaveTextContent('session is down');
  expect(screen.getByRole('region', { name: 'Approval' })).toBeInTheDocument();
});

const PROMPT = { question: 'What kind of document is this?', options: ['meeting', 'idea'], evidence: 'Agenda: standup', freeText: false };
const question = (prompt: object = {}, at = 1) => ask('gh:3', '7:type', { type: 'question', createdAt: iso(at), toolCall: undefined, prompt: { ...PROMPT, ...prompt } });
const pendingDecision = { asks: [question()] };
const ANSWER = 'PATCH /sessions/gh%3A3/asks/7%3Atype';

test('Home shows a workflow question with its options, and an option resolves it', async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:3', { title: 'Campaign Notes' })] };
  routes['GET /asks'] = pendingDecision;
  routes[ANSWER] = { ...question(), state: 'resolved' };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  expect(await screen.findByText('What kind of document is this?')).toBeInTheDocument();
  expect(screen.getByText('Agenda: standup')).not.toBeVisible();
  expect(screen.getByRole('link', { name: 'Campaign Notes' })).toHaveAttribute('href', '#/s/gh%3A3');
  await userEvent.click(screen.getByRole('button', { name: 'meeting' }));
  expect(patched()).toEqual([['sessions/gh%3A3/asks/7%3Atype', { resolution: { choice: 'meeting' } }]]);
  expect(screen.queryByRole('region', { name: 'Question' })).toBeNull();
});

test('a structured question renders title, Markdown detail, suggested option first, You decide as a text button, and a Why fold', async () => {
  const q = question({ header: 'Errors', title: 'Error body for `404`?', detail: 'Callers parse **problem+json** today, see `errors.ts`.', options: ['Plain text', 'Problem JSON', 'You decide'], optionDescriptions: { 'Plain text': 'Smallest change.', 'Problem JSON': 'Typed, one more field.' }, suggested: 1 });
  routes['GET /sessions'] = { sessions: [waiting('gh:3')] };
  routes['GET /asks'] = { asks: [q] };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  const card = await screen.findByRole('region', { name: 'Question' });
  expect(within(card).getByText('Errors')).toBeInTheDocument();
  expect(within(card).getByText('404').tagName).toBe('CODE');
  expect(within(card).getByText('problem+json').tagName).toBe('STRONG');
  expect(within(card).getByText('errors.ts').tagName).toBe('CODE');
  const rows = within(card).getAllByRole('button');
  expect(rows.map((b) => b.textContent)).toEqual(['Problem JSONSuggestedTyped, one more field.', 'Plain textSmallest change.', 'You decide']);
  expect(within(card).getByText('Why this matters')).toBeInTheDocument();
  routes[ANSWER] = { ...q, state: 'resolved' };
  await userEvent.click(rows[0]!);
  expect(lastPost()).toEqual({ resolution: { choice: 'Problem JSON' } });
});

const sendBody = () => lastPost();
async function showFreeText() {
  const q = question({ title: 'Which kind?', options: ['meeting', 'idea', 'You decide'], freeText: true });
  routes['GET /sessions'] = { sessions: [waiting('gh:3')] };
  routes['GET /asks'] = { asks: [q] };
  routes[ANSWER] = { ...q, state: 'resolved' };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  return within(await screen.findByRole('region', { name: 'Question' }));
}

test('Add a note opens a note that goes with the option tapped, in one tap', async () => {
  const card = await showFreeText();
  await userEvent.click(card.getByRole('button', { name: 'Add a note' }));
  await userEvent.type(card.getByLabelText('Note'), 'loose thought');
  await userEvent.click(card.getByRole('button', { name: 'idea' }));
  expect(sendBody()).toEqual({ resolution: { choice: 'idea', reason: 'loose thought' } });
});

test('an empty note sends the option alone', async () => {
  const card = await showFreeText();
  await userEvent.click(card.getByRole('button', { name: 'Add a note' }));
  await userEvent.click(card.getByRole('button', { name: 'idea' }));
  expect(sendBody()).toEqual({ resolution: { choice: 'idea' } });
});

test('Something else resolves with the typed text as both choice and text; a plain option click sends only the label', async () => {
  const card = await showFreeText();
  await userEvent.click(card.getByRole('button', { name: 'Something else…' }));
  expect(card.getByRole('button', { name: 'Send' })).toBeDisabled();
  await userEvent.type(card.getByLabelText('Your answer'), 'a retro');
  await userEvent.click(card.getByRole('button', { name: 'Send' }));
  expect(sendBody()).toEqual({ resolution: { choice: 'a retro', text: 'a retro' } });
});

test('a plain option click on a free-text question is one step', async () => {
  const card = await showFreeText();
  await userEvent.click(card.getByRole('button', { name: 'meeting' }));
  expect(sendBody()).toEqual({ resolution: { choice: 'meeting' } });
});

test('a question that does not allow free text shows no note or Something else', async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:3')] };
  routes['GET /asks'] = { asks: [question({ title: 'Which kind?' })] };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  const card = within(await screen.findByRole('region', { name: 'Question' }));
  expect(card.queryByRole('button', { name: 'Add a note' })).toBeNull();
  expect(card.queryByRole('button', { name: 'Something else…' })).toBeNull();
});

test('a question without title keeps the plain layout and Evidence fold', async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:3')] };
  routes['GET /asks'] = pendingDecision;
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  const card = await screen.findByRole('region', { name: 'Question' });
  expect(within(card).getByText('Evidence')).toBeInTheDocument();
  expect(within(card).queryByText('Suggested')).toBeNull();
  expect(within(card).getByRole('button', { name: 'meeting' })).toHaveTextContent(/^meeting$/);
});

test('a re-asked question with a newer createdAt shows again after the first was answered', async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:3')] };
  routes['GET /asks'] = pendingDecision;
  routes[ANSWER] = { ...question(), state: 'resolved' };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  const first = await screen.findByRole('button', { name: 'meeting' });
  routes['GET /sessions'] = { sessions: [waiting('gh:3', {}, 2)] };
  routes['GET /asks'] = { asks: [question({}, 2)] };
  await userEvent.click(first);
  expect(await screen.findByRole('region', { name: 'Question' })).toBeInTheDocument();
  expect(asksRead()).toHaveLength(2);
});

test('a failed answer keeps the question card and shows the error', async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:3')] };
  routes['GET /asks'] = pendingDecision;
  routes[ANSWER] = new Fail(409, { title: 'Conflict', detail: 'refused by the server' });
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  await userEvent.click(await screen.findByRole('button', { name: 'meeting' }));
  expect(await screen.findByRole('alert')).toHaveTextContent('refused by the server');
  expect(screen.getByRole('region', { name: 'Question' })).toBeInTheDocument();
});

test('a session waiting on a question is in Needs you, and its view shows the question card from its socket', async () => {
  routes['GET /sessions'] = { sessions: [waiting('ui:u1')] };
  const ws = await openSession();
  ws.frame({ type: 'standing', questions: [{ id: '7:type', question: 'What kind of document is this?', options: ['meeting', 'idea'], evidence: null, askedAt: 1 }], failures: [], approvals: [] });
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByText('Waiting on you')).toBeInTheDocument();
  expect(asksRead()).toEqual([]);
  const card = await screen.findByRole('region', { name: 'Question' });
  expect(within(card).getByText('NEEDS YOU')).toBeInTheDocument();
  expect(within(card).getByRole('button', { name: 'idea' })).toBeInTheDocument();
});

const failure = { step: 'decide', error: 'Validation failed for tool "decide":\n- summary: must not have more than 300 characters' };

test('a failed run shows Failed in the header and a card with the step, the first error line and Run again', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1')] };
  const ws = await openSession();
  ws.frame({ type: 'standing', questions: [], failures: [failure], approvals: [] });
  const card = await screen.findByRole('region', { name: 'Failed run' });
  expect(within(card).getByText('decide')).toBeInTheDocument();
  expect(within(card).getByText('Validation failed for tool "decide":')).toBeInTheDocument();
  await userEvent.click(within(card).getByText('Full error'));
  expect(within(card).getByText(/must not have more than 300 characters/)).toBeVisible();
  expect(screen.getByRole('status')).toHaveTextContent('Failed · decide');
  routes['POST /sessions/ui%3Au1/resume'] = { submissionId: '2', status: 'placed' };
  await userEvent.click(within(card).getByRole('button', { name: 'Run again' }));
  expect(fetchMock).toHaveBeenCalledWith('sessions/ui%3Au1/resume', { method: 'POST' });
  ws.frame({ type: 'standing', questions: [], failures: [], approvals: [] });
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Failed run' })).toBeNull());
  expect(screen.getByRole('status')).toHaveTextContent('Idle');
});

test('a failed run that cannot resume shows why, and Copy error copies the whole error', async () => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1')] };
  routes['POST /sessions/ui%3Au1/resume'] = new Fail(409, { title: 'Conflict', detail: 'no failed run to resume' });
  const ws = await openSession();
  ws.frame({ type: 'standing', questions: [], failures: [failure], approvals: [] });
  const writeText = vi.fn(async () => undefined);
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
  const card = await screen.findByRole('region', { name: 'Failed run' });
  await userEvent.click(within(card).getByRole('button', { name: 'Copy error' }));
  expect(writeText).toHaveBeenCalledWith(failure.error);
  await userEvent.click(within(card).getByRole('button', { name: 'Run again' }));
  expect(await within(card).findByRole('alert')).toHaveTextContent('no failed run to resume');
});

test('a session whose last run failed is under Needs you with its step and the error first line, read from its row', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Trade finder', ...failedRun(failure.step, failure.error) })] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  expect(within(needs).getByText('Run failed at decide · Validation failed for tool "decide":')).toBeInTheDocument();
  expect(fetchMock.mock.calls.filter(([u]) => /^(index|asks)/.test(String(u)))).toEqual([]);
});

test('a failed last run is not listed as failed while a new run works on it', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Trade finder', state: 'running', ...failedRun(failure.step, failure.error) })] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const running = await screen.findByRole('region', { name: 'Running' });
  expect(within(running).getByRole('link', { name: /Trade finder/ })).toBeInTheDocument();
  expect(screen.queryByRole('region', { name: 'Needs you' })).toBeNull();
});

test('Home pond goes still when a session has failed', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Trade finder', ...failedRun(failure.step, failure.error) })] };
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

test('a Home approval card reads its tool call from the ask, details behind a disclosure', async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:1', { state: 'running' })] };
  routes['GET /asks'] = { asks: [ask('gh:1', 'a', { toolCall: { tool: 'push', args: { branch: 'main' } } })] };
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  expect(await screen.findByRole('button', { name: 'Approve gh:1' })).toBeInTheDocument();
  expect(screen.getByText('gh:1 wants to continue')).toBeInTheDocument();
  expect(screen.getByText('Details').closest('details')).toHaveTextContent('branchmain');
  expect(screen.getByText('push')).not.toBeVisible();
});

test('Artifacts opens one on its own page with a real URL: versions to pick, a link to its session, and Back returns to the list', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Old work' })] };
  routes['GET /artifacts'] = { artifacts: [
    { session: 'gh:3', kind: 'github.review', version: 2, hash: 'h', sealedAt: iso(2) },
    { session: 'gh:3', kind: 'github.review', version: 1, hash: 'g', sealedAt: iso(1) },
  ] };
  routes['GET /sessions/gh%3A3/artifacts'] = { artifacts: [
    { session: 'gh:3', kind: 'github.review', version: 2, hash: 'h', content: 'final review', sealedAt: iso(2) },
    { session: 'gh:3', kind: 'github.review', version: 1, hash: 'g', content: 'first draft', sealedAt: iso(1) },
  ] };
  location.hash = '#/artifacts';
  render(<App />);
  const open = await screen.findByRole('link', { name: 'Open Old work' });
  expect(within(open).queryByRole('img')).toBeNull();
  expect(open).not.toHaveTextContent(/versions|Double-checked|Passed/);
  expect(screen.getAllByRole('link', { name: /^Open / })).toHaveLength(1);
  expect(open).toHaveAttribute('href', '#/s/gh%3A3/a/github.review');
  await userEvent.click(open);
  await waitFor(() => expect(location.hash).toBe('#/s/gh%3A3/a/github.review'));
  const main = within(screen.getByRole('main'));
  expect(await main.findByText('final review')).toBeInTheDocument();
  expect(main.getByRole('heading', { name: 'Review' })).toBeInTheDocument();
  expect(main.getByText('Old work')).toBeInTheDocument();
  expect(main.getByRole('link', { name: 'Open its session' })).toHaveAttribute('href', '#/s/gh%3A3');
  await userEvent.click(main.getByRole('button', { name: 'Version: Version 2' }));
  await userEvent.click(main.getByRole('menuitem', { name: 'Version 1' }));
  await waitFor(() => expect(location.hash).toBe('#/s/gh%3A3/a/github.review?v=1'));
  expect(await main.findByText('first draft')).toBeInTheDocument();
  expect(screen.queryByRole('dialog')).toBeNull();
  history.back();
  expect(await screen.findByRole('link', { name: 'Open Old work' })).toBeInTheDocument();
  expect(location.hash).toBe('#/artifacts');
  history.forward();
  expect(await within(screen.getByRole('main')).findByText('first draft')).toBeInTheDocument();
});

test('an artifact route names its session, kind and version, and round-trips the link it is built from', () => {
  expect(parseArtifactRoute(artifactHref('github:o/r#7', 'github.review', 3))).toEqual({ key: 'github:o/r#7', kind: 'github.review', version: 3 });
  expect(parseArtifactRoute('#/s/gh%3A3/a/research.report')).toEqual({ key: 'gh:3', kind: 'research.report', version: undefined });
  expect(parseArtifactRoute('#/s/gh%3A3')).toBeUndefined();
  expect(parseArtifactRoute('#/s/%E0%A4%A/a/x')).toBeUndefined();
});

test('Artifacts rows say what each is and its summary, and the kind filter asks the server', async () => {
  routes['GET /sessions'] = { sessions: [sess('github:o/r#7', { title: 'feat: Declare requires' }), sess('ui:r1', { agent: 'researcher', title: 'Why is it slow?', state: 'running' })] };
  routes['GET /artifacts'] = { artifacts: [
    { session: 'github:o/r#7', kind: 'github.review', version: 1, hash: 'h', summary: 'Approve · 1 nit', sealedAt: iso(2) },
    { session: 'ui:r1', kind: 'research.report', version: 1, hash: 'g', summary: 'Cold caches dominate', sealedAt: iso(1) },
    { session: 'ui:old', kind: 'research.report', version: 1, hash: 'o', sealedAt: iso(1) },
  ] };
  location.hash = '#/artifacts';
  render(<App />);
  const review = await screen.findByRole('link', { name: 'Open #7 Declare requires' });
  expect(review).toHaveTextContent('Review · Approve · 1 nit');
  const research = screen.getByRole('link', { name: 'Open Why is it slow?' });
  expect(research).toHaveTextContent('Research report · Cold caches dominate');
  expect(screen.getByRole('link', { name: 'Open ui:old' })).not.toHaveTextContent('·');
  routes[listed('artifacts', 'kind:github\\.review')] = { artifacts: [{ session: 'github:o/r#7', kind: 'github.review', version: 1, hash: 'h', sealedAt: iso(2) }] };
  await userEvent.click(within(screen.getByRole('main')).getByRole('button', { name: 'Filter' }));
  await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Kind' }), 'Review');
  await waitFor(() => expect(screen.getAllByRole('link', { name: /^Open / })).toHaveLength(1));
  expect(screen.getByRole('option', { name: 'Report' })).toBeInTheDocument();
});

test('Artifacts shows why when the list cannot be read', async () => {
  routes['GET /artifacts'] = new Fail(503, { title: 'Service Unavailable', detail: 'the catalog is unavailable' });
  location.hash = '#/artifacts';
  render(<App />);
  expect(await screen.findByRole('alert')).toHaveTextContent('the catalog is unavailable');
});

test('a failed load shows its error on Home', async () => {
  routes['GET /sessions'] = new Fail(503, { title: 'Unavailable', detail: 'index down' });
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  expect(await screen.findAllByRole('alert')).not.toHaveLength(0);
});

test('a double-click on Approve resolves once', async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:3')] };
  routes['GET /asks'] = pendingAsks;
  stubViewport(false);
  location.hash = '#/';
  render(<App />);
  const approve = await screen.findByRole('button', { name: 'Approve gh:3' });
  // The decision stays in flight, as it does while the session wakes.
  fetchMock.mockImplementationOnce(() => new Promise<Response>(() => {}));
  await userEvent.dblClick(approve);
  expect(fetchMock.mock.calls.filter(([, i]) => i?.method === 'PATCH')).toHaveLength(1);
  // The card answers the click at once: the pressed button says what is happening and the other cannot start a second decision.
  expect(approve).toHaveTextContent('Approving…');
  expect(screen.getByRole('button', { name: 'Reject gh:3' })).toBeDisabled();
  expect(approve.closest('section')).toHaveAttribute('aria-busy', 'true');
});

const KEY = 'sleeper:L1:8:lineup';
const player = (name: string, over: object = {}) => ({ id: name, name, pos: 'WR', team: 'DET', inj: null, ...over });
const row = (name: string, over: object = {}) => ({ slot: 'WR1', player: player(name), proj: 12.34, action: 'keep', locked: false, replaces: null, game: { opponent: 'MIN', home: true, kickoff: '2026-11-01T18:00:00.000Z', locked: false, status: 'pre_game' }, why: `Why ${name}`, news: [], ...over });
const lineup = (over: object = {}) => ({ kind: 'lineup', league: 'L1', week: 8, team: 'Substation', opponent: 'Shore69', my_proj: 118.8, opp_proj: 130.5, summary: 'Start Allen over Concepcion.', bench: [{ player: player('Benchy'), proj: 3 }],
  starters: [row('Jefferson'), row('Allen', { slot: 'FLEX', action: 'start', replaces: player('Concepcion'), news: [{ title: 'Practice report', url: 'https://nfl.com/x', date: '2026-10-30', player: 'Concepcion' }] })], ...over });
const sealed = (card: object, verdict: object | null, version = 1) => ({ kind: 'sleeper.cards', version, hash: 'h', content: JSON.stringify(card), sealedAt: 1, verdict });

// The Sleeper page reads results from the Sessions API, which carries no verdict.
async function openSleeper(artifacts: Array<ReturnType<typeof sealed>>, sessionOver: object = {}) {
  routes['GET /apps/sleeper'] = { league: 'L1' };
  routes['GET /sessions'] = { sessions: [sess(KEY, { agent: 'sleeper-lineup', ...sessionOver })] };
  routes[`GET /sessions/${encodeURIComponent(KEY)}/artifacts`] = { artifacts: artifacts.map(({ verdict: _, sealedAt, ...a }) => ({ session: KEY, ...a, sealedAt: iso(sealedAt) })) };
  location.hash = '#/sleeper';
  render(<App />);
}

// A card's checks come with its verdict over the session's own socket, so they render in the session view.
async function openCard(artifacts: object[]) {
  routes['GET /sessions'] = { sessions: [sess(KEY, { agent: 'sleeper-lineup' })] };
  live[encodeURIComponent(KEY)] = { artifacts, approvals: [] };
  location.hash = `#/s/${encodeURIComponent(KEY)}`;
  render(<App />);
  FakeSocket.all[0]!.open();
  FakeSocket.all[0]!.frame(snapshot());
}

test('Sleeper leads with the week strip and renders a lineup card, with no verdict tick', async () => {
  await openSleeper([sealed(lineup(), { passed: true, gatesFailed: [], score: 1, checks: [] })]);
  const strip = await screen.findByRole('region', { name: 'This week' });
  expect(await screen.findByRole('article', { name: 'Week 8 lineup' })).toBeInTheDocument();
  expect(strip).toHaveTextContent(/Lineup lock(s|ed)Sun/);
  expect(within(screen.getByRole('article', { name: 'Week 8 lineup' })).queryByTitle('Double-checked')).toBeNull();
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
  await openCard([sealed(lineup(), null, 1), sealed(lineup(), failed, 2)]);
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
  await openCard([sealed(lineup(), failed)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(card).not.toHaveTextContent('card:');
  expect(card.querySelector('code')).toHaveTextContent('why');
});

test('a passed pitfall is not shown as failed, a triggered pitfall and a missed positive check are', async () => {
  const checks = [chk('weather', 'Blames weather for a call weather did not change', -2, 0), chk('bye', 'Misses a bye week in the plan', -2, 1), chk('news', 'A close call does not cite the news', 3, 0.5), chk('na', 'Not applicable one', -2, null)];
  await openCard([sealed(lineup(), { passed: false, gatesFailed: [], score: 0.5, checks })]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(card).not.toHaveTextContent('Blames weather');
  expect(card).not.toHaveTextContent('Not applicable one');
  expect(card).toHaveTextContent('Misses a bye week in the plan');
  expect(card).toHaveTextContent('A close call does not cite the news');
});

test('a passed card shows no red boxes; partial misses are named in its title tick', async () => {
  const checks = [chk('news', 'A close call does not cite the news', 3, 0.6, ['RB2: Only half the report is cited.'])];
  await openCard([sealed(lineup(), { passed: true, gatesFailed: [], score: 0.9, checks })]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(within(card).getByRole('img', { name: 'Double-checked; partly missed: A close call does not cite the news' })).toBeInTheDocument();
  expect(card).not.toHaveTextContent("Didn't pass");
  expect(card.querySelector('.text-danger')).toBeNull();
});

test('a failing check with no judge note has no disclosure', async () => {
  const failed = { passed: false, gatesFailed: [], score: 0.4, checks: [chk('s', 'Summary skips a lineup change', 'gate', 0)] };
  await openCard([sealed(lineup(), failed)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(card).toHaveTextContent('Summary skips a lineup change');
  expect(card.querySelector('details summary .chev')).toBeNull();
});

test('in the pane, a failed check starts open, a passed one with a note is closed, one with no note has no chevron of its own', async () => {
  const checks = [chk('a', 'Quotes text that is not in the source', 3, 0, ['Not in the source.']), chk('b', 'Cites a source that was not opened', 3, 1, ['All sources opened.']), chk('c', 'A finding cites no source', 3, 1)];
  (live['ui%3Au1'] ??= {}).artifacts = [{ kind: 'research.report', version: 2, hash: 'h2', content: '## Short answer\n\nx', sealedAt: 2, verdict: { passed: false, gatesFailed: [], score: 0.5, checks } }];
  const ws = await openSession();
  ws.frame(snapshot());
  const pane = await screen.findByRole('region', { name: 'Report' });
  const row = (label: string) => within(pane).getByText(label).closest('details');
  expect(row('Quotes text that is not in the source')).toHaveAttribute('open');
  expect(row('Cites a source that was not opened')).not.toHaveAttribute('open');
  // A check with no note sits directly in the result's own disclosure.
  expect(row('A finding cites no source')!.querySelector('summary')).toHaveTextContent(/^Double-check failed/);
  expect(pane.querySelectorAll('summary .chev')).toHaveLength(3);
  expect(pane).not.toHaveTextContent('Why');
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
    ws.frame({ type: 'standing', questions: [{ id: '7:type', question: 'Which?', options: ['a'], evidence: null, askedAt: Date.parse('2026-11-01T01:04:00.000Z') }], failures: [], approvals: [] });
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
  (live['ui%3Au1'] ??= {}).artifacts = [
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
    ];
  const ws = await openSession();
  ws.frame(snapshot());
  const section = await screen.findByRole('log', { name: 'Transcript' });
  await waitFor(() => expect(ledgerOf(section).length).toBeGreaterThan(0));
  const lines = ledgerOf(section).map((li) => li.textContent ?? '');
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
  await openCard([sealed(lineup(), old)]);
  const card = await screen.findByRole('article', { name: 'Week 8 lineup' });
  expect(within(card).getByText('weather mentioned unmoved')).toBeInTheDocument();
});

test('a note does not end its first sentence at e.g.', async () => {
  const failed = { passed: false, gatesFailed: [], score: 0.4, checks: [chk('x', 'Misses the news', 3, 0, ['Cites no report, e.g. the practice update. It stops there.'])] };
  await openCard([sealed(lineup(), failed)]);
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

async function openReview(key: string, active: boolean, sealedItems: object[] = []) {
  routes['GET /sessions'] = { sessions: [sess(key, { title: 'Sleeper week stepper', state: active ? 'running' : 'idle' })] };
  (live[`${encodeURIComponent(key)}`] ??= {}).artifacts = sealedItems;
  (live[`${encodeURIComponent(key)}`] ??= {}).approvals = [];
  location.hash = `#/s/${encodeURIComponent(key)}`;
  render(<App />);
  await screen.findByRole('heading', { name: /Sleeper week stepper/ });
}

const postedV5 = { kind: 'github.review', version: 5, hash: 'h5', content: '## Verdict\nrequest_changes', sealedAt: 5, verdict: { passed: true, gatesFailed: [], score: 1, checks: [] }, links: [{ label: 'Posted', url: 'https://github.com/x/review-5' }] };

test('the pinned artifact line reads Previous review, without a tick, while a new request runs, and Review posted once it ends', async () => {
  await openReview('github:fagerbergj/quack#2042', true, [postedV5]);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot());
  ws.frame(view({ run: true }));
  const line = (await screen.findByText('Previous review')).closest<HTMLElement>('[data-ledger]')!;
  expect(within(line).queryByRole('img', { name: 'Double-checked' })).toBeNull();
  expect(within(line).queryByRole('region', { name: 'Review' })).toBeNull();
  expect(line).not.toHaveTextContent('Double-checked');
  ws.frame(view({}));
  expect(await screen.findByText('Review posted')).toBeInTheDocument();
  const done = screen.getByText('Review posted').closest<HTMLElement>('[data-ledger]')!;
  expect(within(done).getAllByRole('img', { name: 'Double-checked' })).toHaveLength(1);
  // The card, in the pane beside the chat, carries the one Posted link; the line drops its own.
  expect(within(done).queryByRole('link', { name: /Posted/ })).toBeNull();
  expect(screen.getAllByRole('link', { name: /Posted/ })).toHaveLength(1);
});

const withRow = (over: object = {}) => {
  routes['GET /sessions'] = { sessions: [sess('ui:u1', { title: 'Plan the trip', ...over })] };
  routes['PATCH /sessions/ui%3Au1'] = { key: 'ui:u1' };
  routes['DELETE /sessions/ui%3Au1'] = { key: 'ui:u1' };
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
  const trip = sess('ui:u1', { title: 'Plan the trip', state: 'archived' });
  routes['GET /sessions'] = { sessions: [trip, sess('ui:u2', { title: 'Other' })] };
  routes[listed('sessions', ACTIVE)] = { sessions: [sess('ui:u2', { title: 'Other' })] };
  routes[listed('sessions', 'state:archived')] = { sessions: [trip] };
  render(<App />);
  await screen.findByRole('link', { name: /Other/ });
  expect(screen.queryByRole('link', { name: /Plan the trip/ })).toBeNull();
  expect(screen.queryByRole('region', { name: 'Running' })).toBeNull();
  await userEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await userEvent.selectOptions(screen.getByLabelText('Show'), 'Archived');
  expect(await screen.findByRole('link', { name: /Plan the trip/ })).toBeInTheDocument();
  expect(screen.queryByRole('link', { name: /Other/ })).toBeNull();
});

test('Archive posts the flag from the session menu', async () => {
  withRow();
  await openSession();
  await menu('Archive');
  expect(sent('PATCH')).toEqual([['sessions/ui%3Au1', { state: 'archived' }]]);
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
  withRow({ state: 'archived' });
  const ws = await openSession();
  ws.frame(snapshot());
  expect(await screen.findByText('Archived')).toBeInTheDocument();
  await menu('Unarchive');
  expect(sent('PATCH')).toEqual([['sessions/ui%3Au1', { state: 'stopped' }]]);
});

test('a refused archive says why and the session stays', async () => {
  withRow();
  routes['PATCH /sessions/ui%3Au1'] = new Fail(409, { title: 'Conflict', detail: 'refused by the server' });
  await openSession();
  await menu('Archive');
  expect(await screen.findByRole('alert')).toHaveTextContent('refused by the server');
});

test('Delete asks first, names the session, and goes back to the list once confirmed', async () => {
  withRow();
  await openSession();
  await menu('Delete');
  const dialog = screen.getByRole('dialog', { name: 'Delete session?' });
  expect(dialog).toHaveTextContent('Plan the trip');
  await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
  const deletes = () => fetchMock.mock.calls.filter(([, init]) => init?.method === 'DELETE').map(([url]) => url);
  expect(deletes()).toEqual([]);
  await menu('Delete');
  routes['GET /sessions'] = { sessions: [] };
  await userEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await waitFor(() => expect(location.hash).toBe('#/'));
  expect(deletes()).toEqual(['sessions/ui%3Au1']);
});

test('a refused delete keeps the dialog open with the server reason', async () => {
  withRow();
  routes['DELETE /sessions/ui%3Au1'] = new Fail(409, { title: 'Conflict', detail: 'refused by the server' });
  await openSession();
  await menu('Delete');
  await userEvent.click(screen.getByRole('button', { name: 'Delete' }));
  expect(await within(screen.getByRole('dialog')).findByRole('alert')).toHaveTextContent('refused by the server');
});

const phoneAt = (hash: string) => {
  stubViewport(false);
  location.hash = hash;
};

test('Home shows what needs you and what is running, each linking to its session', async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:1', { title: 'Needs a look' }), sess('gh:2', { title: 'Busy one', state: 'running' }), sess('gh:3', { title: 'Old work' })] };
  routes['GET /asks'] = { asks: [ask('gh:1', 'a')] };
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
  routes['GET /sessions'] = { sessions: [waiting('home:h1', { title: 'Waivers', state: 'running' })] };
  routes['GET /asks'] = { asks: [ask('home:h1', 'a', { toolCall: { tool: 'start', args: { summary: 'Process waivers for week 9' } } })] };
  phoneAt('#/');
  render(<App />);
  const card = await screen.findByRole('region', { name: 'Approval' });
  expect(within(card).getAllByText('Process waivers for week 9')[0]).toBeInTheDocument();
  expect(within(card).getByText('start', { exact: false })).not.toBeVisible();
});

test('a failed session that is not running is on Home with Run again; a running one is not', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:3', { title: 'Trade finder', ...failedRun(failure.step, failure.error) }), sess('gh:4', { title: 'Retrying', state: 'running', ...failedRun(failure.step, failure.error) })] };
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
  homeExists();
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
  (live['home%3Ah1'] ??= {}).approvals = [{ id: 'a', tool: 'start', args: { summary: 'Process waivers for week 9' }, requestedAt: 1 }];
  (live['home%3Ah1'] ??= {}).artifacts = [];
  phoneAt('#/s/home%3Ah1');
  render(<App />);
  FakeSocket.all[0]!.open();
  const card = await screen.findByRole('region', { name: 'Approval' });
  expect(within(card).getAllByText('Process waivers for week 9')[0]).toBeInTheDocument();
  expect(screen.getByRole('heading', { name: 'quack' })).toBeInTheDocument();
});

test('archived sessions stay out of Home', async () => {
  routes['GET /sessions'] = { sessions: [sess('gh:1', { title: 'Live one', state: 'running' }), sess('gh:2', { title: 'Shelved', state: 'archived' })] };
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

test('session rows show their spend from the cost in micros: dollars when priced, tokens when not, nothing when idle', async () => {
  const tokens = (input: number) => ({ input, output: 0, cacheRead: 0, cacheWrite: 0 });
  const usd = (amountMicros: string) => ({ currencyCode: 'USD', amountMicros });
  routes['GET /sessions'] = { sessions: [sess('gh:1', { title: 'Priced', cost: usd('420000'), tokens: tokens(9000) }), sess('gh:2', { title: 'Unpriced', cost: usd('0'), tokens: tokens(12000) }), sess('gh:3', { title: 'Fresh', cost: usd('0'), tokens: tokens(0) })] };
  render(<App />);
  const row = (name: RegExp) => screen.getByRole('link', { name }).parentElement!;
  await screen.findByRole('link', { name: /Priced/ });
  expect(row(/Priced/)).toHaveTextContent('$0.42');
  expect(row(/Unpriced/)).toHaveTextContent('12k tokens');
  expect(row(/Fresh/)).not.toHaveTextContent(/tokens|\$/);
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

  test('an image-only first send on an empty Home says to write a message and starts nothing', async () => {
    render(<App />);
    await userEvent.upload(await screen.findByTestId('attach-input'), photo);
    await screen.findByRole('img', { name: 'Attached image 1' });
    await userEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Write a message to start the conversation.');
    expect(FakeSocket.all).toEqual([]);
  });

  test('a photo attached on Home seeds the home:main session and clears once accepted', async () => {
    homeExists();
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

test('a Home approval card says what it will do, from the args the ask carries', async () => {
  routes['GET /sessions'] = { sessions: [waiting('gh:1', { title: 'Fix the typo' })] };
  const args = { summary: 'Push 1 commit(s) to fix-typo of o/r#1: docs: fix typo', commits: 'abc1234 docs: fix typo' };
  routes['GET /asks'] = { asks: [ask('gh:1', 'a', { toolCall: { tool: 'deliver', args } })] };
  phoneAt('#/');
  render(<App />);
  const needs = await screen.findByRole('region', { name: 'Needs you' });
  // The headline, then again in the Details tree.
  expect(within(needs).getAllByText(args.summary)[0]).toBeInTheDocument();
  expect(within(needs).getByText(/abc1234 docs: fix typo/)).toBeInTheDocument();
});

const ledgerOf = (root: HTMLElement) => Array.from(root.querySelectorAll<HTMLElement>('[data-ledger]'));

describe('the session page as a ledger', () => {
  const at = (ts: number, content: object[]) => ({ ...assistant(content), timestamp: ts });
  const call = (id: string, name: string) => ({ type: 'toolCall', id, name, arguments: {} });
  const result = (id: string, name: string, ts: number) => ({ role: 'toolResult', toolCallId: id, toolName: name, content: [{ type: 'text', text: 'ok' }], isError: false, timestamp: ts });
  const check = (id: string, met: number | null, notes: string[] = []) => ({ criterion: 'c', id, label: `Label ${id}`, weight: 2, met, notes });
  const fail = { passed: false, gatesFailed: [], score: 0.4, checks: [check('cites', 0, ['Finding 2 has no line number.']), check('tone', 1)] };
  const pass = { passed: true, gatesFailed: [], score: 1, checks: [check('cites', 1), check('tone', 1)] };
  const review = (version: number, sealedAt: number, verdict: object) => ({ kind: 'github.review', version, hash: `h${version}`, content: `body ${version}`, sealedAt, verdict });
  // draft work, submit (v1 sealed at 30, fails), revision work, submit again (v2 sealed at 70, passes)
  const entries = [
    entry('e1', [{ role: 'user', content: 'review it', timestamp: 10 }]),
    entry('e2', [at(20, [call('a', 'read'), call('b', 'submit_review')]), result('a', 'read', 25), result('b', 'submit_review', 40)]),
    entry('e3', [at(50, [call('c', 'bash'), call('d', 'submit_review')]), result('c', 'bash', 55), result('d', 'submit_review', 80)]),
    entry('e4', [at(90, [{ type: 'text', text: 'All done.' }])]),
  ];

  test('each version sits in time order between the work around it, and a revised version shows its round', async () => {
    (live['ui%3Au1'] ??= {}).artifacts = [review(1, 30, fail), review(2, 70, pass)];
    const ws = await openSession();
    ws.frame(snapshot({ entries }));
    const log = await screen.findByRole('log', { name: 'Transcript' });
    await waitFor(() => expect(ledgerOf(log)).toHaveLength(2));
    const text = log.textContent ?? '';
    const order = ['Worked', "Review didn't pass its checks", 'Worked', 'Review ready', 'All done.'].reduce<number[]>((acc, s) => [...acc, text.indexOf(s, (acc.at(-1) ?? -1) + 1)], []);
    expect(order.every((n) => n >= 0)).toBe(true);
    const [one, two] = ledgerOf(log);
    expect(one).not.toHaveTextContent(/revision \d/);
    expect(two).toHaveTextContent('Version 2 · revision 1');
    expect(screen.queryByRole('region', { name: 'Artifacts' })).toBeNull();
  });

  test('a failed check opens on what the revision must fix; a passed one is only the tick on its line', async () => {
    (live['ui%3Au1'] ??= {}).artifacts = [review(1, 30, fail), review(2, 70, pass)];
    const ws = await openSession();
    ws.frame(snapshot({ entries }));
    const log = await screen.findByRole('log', { name: 'Transcript' });
    await waitFor(() => expect(ledgerOf(log)).toHaveLength(2));
    const [one, two] = ledgerOf(log);
    expect(within(one).getByText(/Double-check failed · 1 to fix · 40%/).closest('details')).toHaveAttribute('open');
    expect(within(one).getByText('The revision must fix')).toBeInTheDocument();
    expect(within(one).getByText('Finding 2 has no line number.')).toBeInTheDocument();
    expect(within(two).getByRole('img', { name: 'Double-checked' })).toBeInTheDocument();
    expect(within(two).queryByText(/Double-check passed/)).toBeNull();
  });

  test('a failed latest version opens on what to fix even with an extension card, and a partial check shows its score', async () => {
    const half = { ...check('cites', 0.5, ['Half the findings cite lines.']), weight: 2 };
    (live['ui%3Au1'] ??= {}).artifacts = [review(1, 30, pass), review(2, 70, { ...fail, checks: [half] })];
    const ws = await openSession();
    ws.frame(snapshot({ entries }));
    const log = await screen.findByRole('log', { name: 'Transcript' });
    await waitFor(() => expect(ledgerOf(log)).toHaveLength(2));
    const latest = ledgerOf(log)[1]!;
    expect(within(latest).getByText(/Double-check failed/).closest('details')).toHaveAttribute('open');
    expect(within(latest).getByText('Fail · 50%')).toBeInTheDocument();
  });

  test('while the judge runs, only running checks are listed and each one folds into Done as it finishes', async () => {
    const ws = await openSession();
    const checking = (checks: object[]) => view({ entries: entries.slice(0, 2), run: true, tools: [{ callId: 'b', name: 'submit_review', status: 'running', details: { rubric: 'r', checks } }] });
    const base = { criterion: 'c', weight: 2 };
    ws.frame(checking([{ ...base, id: 'cites', label: 'Cites lines' }, { ...base, id: 'tone', label: 'Polite tone' }, { ...base, id: 'scope', label: 'Stays in scope' }]));
    expect(await screen.findByLabelText('Double-checking · 0 of 3 · nothing found yet')).toBeInTheDocument();
    const running = screen.getByRole('list', { name: 'Checks running' });
    expect(within(running).getAllByText('Checking')).toHaveLength(3);
    expect(screen.queryByText(/^Done ·/)).toBeNull();
    ws.frame(checking([{ ...base, id: 'cites', label: 'Cites lines', met: 0 }, { ...base, id: 'tone', label: 'Polite tone', met: 1 }, { ...base, id: 'scope', label: 'Stays in scope' }]));
    expect(await screen.findByLabelText('Double-checking · 2 of 3 · 1 problem')).toBeInTheDocument();
    expect(within(screen.getByRole('list', { name: 'Checks running' })).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['Stays in scopeChecking']);
    const done = screen.getByText('Done · 1 problem · 1 nothing found');
    expect(done.closest('details')).not.toHaveAttribute('open');
    expect(within(screen.getByRole('list', { name: 'Checks done' })).getByText('Fail')).toBeInTheDocument();
    expect(screen.queryByText('Waiting for the double-check')).toBeNull();
  });
});

test('the session menu runs from harmless to destructive', async () => {
  (live['ui%3Au1'] ??= {}).artifacts = [{ kind: 'github.review', version: 1, hash: 'h', content: 'r', sealedAt: 1, verdict: null }];
  routes['GET /sessions'] = { sessions: [sess('ui:u1', { title: 'Plan the trip' })] };
  const ws = await openSession();
  ws.frame(snapshot({ run: true }));
  await userEvent.click(await screen.findByRole('button', { name: 'More actions' }));
  const items = await screen.findAllByRole('menuitem');
  const names = items.map((i) => i.textContent);
  expect(names).toEqual(['Stop', 'Rename', 'Export', 'Archive', 'Delete']);
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

describe('the session context rail', () => {
  const key = 'github%3Ao%2Fr%231';
  const opening = 'Review pull request o/r#1 (label quack:review).\nTitle: Cost per run\nHead: 9f43b50aaaa on feat/x; base: v1\n\nWhat changed';
  const sealed = (version: number, over: object = {}) => ({ kind: 'github.review', version, hash: `h${version}`, content: '## Takeaway\nx\n\n## Verdict\napprove', sealedAt: Date.parse('2026-11-01T14:00:00Z') + version, verdict: { passed: true, gatesFailed: [], score: 1, checks: [] }, ...over });
  const check = (name: string, status: string, conclusion: string | null) => ({ name, status, conclusion });

  async function openReview(wide: boolean, over: { checks?: object[]; artifacts?: object[]; approvals?: object[] } = {}) {
    stubViewport(true, wide);
    Element.prototype.scrollIntoView = vi.fn();
    routes['GET /sessions'] = { sessions: [sess('github:o/r#1', { agent: 'reviewer', createdAt: '2026-11-01T13:00:00Z', lastActivity: '2026-11-01T15:00:00Z' })] };
    routes['GET /agents'] = { agents: [{ name: 'reviewer', version: 3, status: 'promoted', model: 'qwen3.8-27b' }] };
    (live[`${key}`] ??= {}).artifacts = over.artifacts ?? [sealed(1, { verdict: { passed: false, gatesFailed: [], score: 0.5, checks: [] } }), sealed(2, { links: [{ label: 'Posted', url: 'https://github.com/o/r/pull/1#r' }] })];
    (live[`${key}`] ??= {}).approvals = over.approvals ?? [];
    routes[`GET /sessions/${key}/meta`] = { meta: [{ id: '9f43b50aaaa', files: [{ path: 'a.ts', additions: 200, deletions: 10 }, { path: 'b.ts', additions: 12, deletions: 4 }], checks: over.checks ?? [check('test', 'completed', 'success'), check('image', 'completed', 'skipped')] }] };
    location.hash = `#/s/${key}`;
    render(<App />);
    const ws = FakeSocket.all[0];
    ws.open();
    ws.frame(snapshot({ entries: [entry('u', [{ role: 'user', content: opening, timestamp: 1 }])] }));
    return ws;
  }
  // Beside a sealed version the pane takes the rail's column, so even at 1440px Details opens the rail as a sheet.
  const details = async () => {
    await userEvent.click(await screen.findByRole('button', { name: 'Details' }));
    return screen.getByRole('dialog', { name: 'Details' });
  };

  test('at 1440px with no version yet the rail sits beside the ledger, and no Details button shows', async () => {
    await openReview(true, { artifacts: [] });
    const rail = await screen.findByRole('complementary', { name: 'Context' });
    expect(await within(within(rail).getByRole('region', { name: 'Pull request' })).findByText('#1 Cost per run')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Details' })).toBeNull();
  });

  test('at 1440px beside a version, Details shows every section', async () => {
    await openReview(true);
    expect(screen.queryByRole('complementary', { name: 'Context' })).toBeNull();
    const rail = await details();
    const pr = within(rail).getByRole('region', { name: 'Pull request' });
    expect(await within(pr).findByText('#1 Cost per run')).toBeInTheDocument();
    expect(within(pr).getByText('feat/x → v1')).toBeInTheDocument();
    expect(within(pr).getByText('✓ All passed')).toBeInTheDocument();
    expect(within(pr).queryByText(/image|skipped/i)).toBeNull();
    expect(within(pr).getByText(/2 files changed/)).toHaveTextContent('2 files changed +212 −14');
    expect(within(pr).getByRole('link', { name: 'Open on GitHub ↗' })).toHaveAttribute('href', 'https://github.com/o/r/pull/1');
    const versions = within(rail).getByRole('region', { name: 'Versions' });
    expect(await within(versions).findByText('Review v1')).toBeInTheDocument();
    expect(within(versions).getByText("Didn't pass its checks, 50%")).toBeInTheDocument();
    expect(within(versions).getByText('Double-checked · Posted to GitHub as Approve')).toBeInTheDocument();
    expect(within(rail).getByText('Nothing is waiting on you.')).toBeInTheDocument();
    expect(within(rail).queryByRole('region', { name: 'Cost' })).toBeNull();
    expect(await within(rail).findByText('Agent reviewer on qwen3.8-27b')).toBeInTheDocument();
    expect(within(rail).getByText(/^Started .* · Last activity /)).toBeInTheDocument();
  });

  test('failed and running checks are named and skipped ones never show', async () => {
    await openReview(true, { checks: [check('test', 'completed', 'failure'), check('lint', 'in_progress', null), check('build', 'completed', 'success'), check('image', 'completed', 'skipped')] });
    const pr = within(await details()).getByRole('region', { name: 'Pull request' });
    expect(await within(pr).findByText('✗ test failed')).toBeInTheDocument();
    expect(within(pr).getByText('… lint running')).toBeInTheDocument();
    expect(within(pr).queryByText(/All passed|image|build/)).toBeNull();
  });

  test('a version row scrolls the ledger to that version, and a pending approval to its card', async () => {
    const user = userEvent.setup();
    await openReview(true, { approvals: [{ id: 'a1', tool: 'push', args: { summary: 'Push the fix' }, requestedAt: 1 }] });
    const rail = await details();
    await user.click(await within(rail).findByRole('button', { name: /Review v1/ }));
    const target = vi.mocked(Element.prototype.scrollIntoView).mock.contexts.at(-1) as HTMLElement;
    expect(target.id).toMatch(/^v-github\.review-1-/);
    expect(target).toHaveAttribute('data-ledger');
    await user.click(await within(rail).findByRole('button', { name: 'Push the fix' }));
    expect((vi.mocked(Element.prototype.scrollIntoView).mock.contexts.at(-1) as HTMLElement).id).toBe('need-a1');
  });

  test('below 1440px a Details button opens the same content as a sheet that Escape closes', async () => {
    const user = userEvent.setup();
    await openReview(false);
    expect(screen.queryByRole('complementary', { name: 'Context' })).toBeNull();
    await user.click(await screen.findByRole('button', { name: 'Details' }));
    const sheet = screen.getByRole('dialog', { name: 'Details' });
    expect(await within(sheet).findByText('#1 Cost per run')).toBeInTheDocument();
    expect(within(sheet).getByText('Nothing is waiting on you.')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'Details' })).toBeNull();
  });

  test('a session that is not a pull request has no Pull request section', async () => {
    stubViewport(true, true);
    const ws = await openSession();
    ws.frame(snapshot());
    const rail = await screen.findByRole('complementary', { name: 'Context' });
    expect(within(rail).queryByRole('region', { name: 'Pull request' })).toBeNull();
    expect(within(rail).getByText('Nothing is waiting on you.')).toBeInTheDocument();
  });
});

test('a contract session reads its own title and its last run on its row, and the same title in its header', async () => {
  routes['GET /sessions'] = { sessions: [sess('contracts:c2', { title: 'Roster API', ...ran() })] };
  stubViewport(false);
  location.hash = '#/sessions';
  const { unmount } = render(<App />);
  const row = await screen.findByRole('link', { name: /Roster API/ });
  expect(row).toHaveTextContent('Draft ready');
  unmount();
  location.hash = '#/s/contracts%3Ac2';
  routes['GET /sessions/contracts%3Ac2/asks'] = { asks: [] };
  render(<App />);
  expect(await screen.findByRole('heading', { name: /Roster API/ })).toBeInTheDocument();
});
