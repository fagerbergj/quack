import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test, vi } from 'vitest';
import { App } from './App.tsx';
import { FakeSocket, fetchMock, routes, snapshot, stubViewport, useHarness } from './test-harness.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /asks': { asks: [] } }));

function openSession() {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot());
  return { ws, box: screen.getByRole('textbox', { name: 'Message' }) as HTMLTextAreaElement };
}

test('SessionView composer: plain Enter submits on fine pointer', async () => {
  const { ws, box } = openSession();
  await userEvent.type(box, 'hello');
  fireEvent.keyDown(box, { key: 'Enter' });
  await waitFor(() => expect(ws.sent).toEqual([expect.objectContaining({ type: 'input', input: 'hello' })]));
});

test('SessionView composer: Shift+Enter, Ctrl+Enter, and IME Enter do not submit', async () => {
  const { ws, box } = openSession();
  await userEvent.type(box, 'abcd');

  // Shift+Enter keeps browser's native newline behavior.
  expect(fireEvent.keyDown(box, { key: 'Enter', shiftKey: true })).toBe(true);
  expect(ws.sent).toEqual([]);

  // IME composing Enter is ignored.
  expect(fireEvent.keyDown(box, { key: 'Enter', isComposing: true })).toBe(true);
  expect(ws.sent).toEqual([]);

  // Ctrl+Enter inserts a newline at the cursor and advances the caret.
  box.setSelectionRange(2, 2);
  expect(fireEvent.keyDown(box, { key: 'Enter', ctrlKey: true })).toBe(false);
  await act(async () => {});
  expect(box).toHaveValue('ab\ncd');
  expect(box.selectionStart).toBe(3);
  expect(box.selectionEnd).toBe(3);
  expect(ws.sent).toEqual([]);
});

test('SessionView composer: plain Enter does not submit on coarse pointer', async () => {
  stubViewport(false);
  const { ws, box } = openSession();
  await userEvent.type(box, 'hello');
  expect(fireEvent.keyDown(box, { key: 'Enter' })).toBe(true);
  expect(ws.sent).toEqual([]);
});

test('Home composer: plain Enter submits and Shift+Enter / Ctrl+Enter / IME do not', async () => {
  routes['POST /sessions/home%3Amain/invocations?agent=quack'] = { status: 'accepted' };
  render(<App />);
  const box = (await screen.findByRole('textbox', { name: 'What do you need?' })) as HTMLTextAreaElement;
  await userEvent.type(box, 'hi there');

  expect(fireEvent.keyDown(box, { key: 'Enter', shiftKey: true })).toBe(true);
  expect(fireEvent.keyDown(box, { key: 'Enter', isComposing: true })).toBe(true);

  box.setSelectionRange(2, 3);
  expect(fireEvent.keyDown(box, { key: 'Enter', ctrlKey: true })).toBe(false);
  await act(async () => {});
  expect(box).toHaveValue('hi\nthere');
  expect(box.selectionStart).toBe(3);
  expect(fetchMock.mock.calls.some(([u]) => String(u).includes('/invocations'))).toBe(false);

  // Absent matchMedia falls back to Enter-to-send.
  vi.stubGlobal('matchMedia', undefined);
  fireEvent.keyDown(box, { key: 'Enter' });
  await waitFor(() => expect(fetchMock.mock.calls.some(([u]) => String(u).includes('sessions/home%3Amain/invocations'))).toBe(true));
});
