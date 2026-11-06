import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, expect, test, vi } from 'vitest';
import { App } from './App.tsx';
import { assistant, entry, FakeSocket, snapshot, useHarness, view } from './test-harness.ts';

// Counts each transcript row's render by what it draws: prose and user lines render Markdown, a tool call renders its Args.
const renders = new Map<string, number>();
const count = (k: string) => renders.set(k, (renders.get(k) ?? 0) + 1);
vi.mock('./Markdown.tsx', () => ({ Markdown: ({ text }: { text: string }) => (count(text), <p>{text}</p>) }));
vi.mock('./JsonView.tsx', async (real) => ({ ...(await real<typeof import('./JsonView.tsx')>()), Args: ({ value }: { value: unknown }) => (count(`args:${JSON.stringify(value)}`), null) }));

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] } }));
beforeEach(() => renders.clear());

const call = { type: 'toolCall', id: 'c1', name: 'read', arguments: { path: 'a.md' } };
const committed = [
  entry('1', [{ role: 'user', content: 'Plan a trip', timestamp: 1 }]),
  entry('2', [{ ...assistant([{ type: 'text', text: 'First answer' }, call]), timestamp: 2 }]),
  entry('3', [{ role: 'toolResult', toolCallId: 'c1', toolName: 'read', content: [{ type: 'text', text: 'notes' }], isError: false, timestamp: 3 }]),
];
const ROWS = ['Plan a trip', 'First answer', 'args:{"path":"a.md"}'];
const rowRenders = () => Object.fromEntries(ROWS.map((k) => [k, renders.get(k)]));

async function open() {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot({ entries: committed, run: true }));
  expect(await screen.findByText('First answer')).toBeInTheDocument();
  return ws;
}

test('typing in the composer re-renders no transcript row', async () => {
  await open();
  const before = rowRenders();
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'hello world');
  expect(rowRenders()).toEqual(before);
});

test('a streamed delta re-renders only the streaming row, and a resent transcript re-renders none', async () => {
  const ws = await open();
  const before = rowRenders();
  const deltas = ['Str', 'Strea', 'Stream', 'Streamed', 'Streamed reply'];
  for (const text of deltas) ws.frame(view({ run: true, message: { ...assistant([{ type: 'text', text }]), timestamp: 4 } }));
  expect(rowRenders()).toEqual(before);
  expect(deltas.map((d) => renders.get(d))).toEqual([1, 1, 1, 1, 1]);
  // Every commit resends the transcript as fresh objects; rows whose content did not change stay as they are.
  ws.frame(view({ entries: structuredClone(committed), run: true }));
  expect(rowRenders()).toEqual(before);
});

test('a new committed message renders once and leaves earlier rows alone', async () => {
  const ws = await open();
  const before = rowRenders();
  ws.frame(view({ entries: [...committed, entry('4', [{ ...assistant([{ type: 'text', text: 'Second answer' }]), timestamp: 4 }])] }));
  expect(await screen.findByText('Second answer')).toBeInTheDocument();
  expect(rowRenders()).toEqual(before);
  expect(renders.get('Second answer')).toBe(1);
});

test('the transcript announces completed messages, not each streamed line', async () => {
  const ws = await open();
  const log = screen.getByRole('log', { name: 'Transcript' });
  expect(log.parentElement!.closest('[aria-live]')).toBeNull();
  ws.frame(view({ run: true, message: { ...assistant([{ type: 'text', text: 'Half a' }]), timestamp: 4 } }));
  expect(log).toHaveAttribute('aria-busy', 'true');
  ws.frame(view({ entries: [...committed, entry('4', [{ ...assistant([{ type: 'text', text: 'Half a reply' }]), timestamp: 4 }])] }));
  expect(log).toHaveAttribute('aria-busy', 'false');
});
