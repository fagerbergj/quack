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

let routes: Record<string, unknown>;
const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
  const hit = routes[`${init?.method ?? 'GET'} ${url}`];
  return new Response(JSON.stringify(hit ?? { error: 'not found' }), { status: hit ? 200 : 404 });
});

beforeEach(() => {
  FakeSocket.all = [];
  routes = {};
  fetchMock.mockClear();
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal('fetch', fetchMock);
  vi.stubGlobal('crypto', { randomUUID: () => 'uuid-1' });
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

test('lists sessions with their agent and state', async () => {
  routes['GET /sessions'] = { sessions: [{ key: 'gh:1', agent: 'reviewer', running: true, lastActivity: 1000 }] };
  routes['GET /agents'] = { agents: [] };
  render(<App />);
  const link = await screen.findByRole('link', { name: 'gh:1' });
  expect(link).toHaveAttribute('href', '#/s/gh%3A1');
  expect(link.closest('li')).toHaveTextContent('reviewer running');
});

test('starting a session opens a ui: key with the chosen agent', async () => {
  routes['GET /sessions'] = { sessions: [] };
  routes['GET /agents'] = { agents: [{ name: 'coder', version: '1' }, { name: 'reviewer', version: '2' }] };
  render(<App />);
  await userEvent.selectOptions(await screen.findByRole('combobox', { name: 'Agent' }), 'reviewer 2');
  await userEvent.click(screen.getByRole('button', { name: 'New session' }));
  await waitFor(() => expect(FakeSocket.all[0]?.url).toMatch(/\/sessions\/ui%3Auuid-1\/ws\?agent=reviewer$/));
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

test('an idle send has no whenBusy; a busy send carries the chosen mode', async () => {
  const ws = await openSession();
  ws.frame(snapshot());
  expect(screen.queryByRole('group')).toBeNull();
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'first');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  expect(ws.sent[0]).toEqual({ type: 'input', input: 'first' });

  ws.frame({ type: 'events', events: [{ type: 'run_start', inputs: [] }] });
  const group = screen.getByRole('group', { name: 'While the agent is working' });
  expect(within(group).getByRole('radio', { name: 'Steer' })).toBeChecked();
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'turn left');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  await userEvent.click(within(group).getByRole('radio', { name: 'Follow up' }));
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
  render(<App />);
  expect(await screen.findByText(/Sessions: /)).toBeInTheDocument();
  expect(screen.getByText(/Agents: /)).toBeInTheDocument();
});
