import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { App } from './App.tsx';
import { FakeSocket, snapshot, useHarness, view } from './test-harness.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] } }));

// The observers the page registers, so a test can say that content changed size.
let observers: Array<() => void> = [];
beforeEach(() => {
  observers = [];
  vi.stubGlobal('ResizeObserver', class { constructor(cb: () => void) { observers.push(cb); } observe() {} unobserve() {} disconnect() {} });
});
afterEach(() => vi.unstubAllGlobals());

// jsdom does no layout: the scroller reports the sizes the test sets and remembers where it was scrolled.
function layout(scroller: HTMLElement) {
  const box = { top: 0, height: 1000 };
  Object.defineProperty(scroller, 'scrollHeight', { get: () => box.height, configurable: true });
  Object.defineProperty(scroller, 'clientHeight', { value: 400, configurable: true });
  Object.defineProperty(scroller, 'scrollTop', { get: () => box.top, set: (v: number) => (box.top = v), configurable: true });
  return box;
}

async function open() {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot());
  const scroller = (await screen.findByRole('log', { name: 'Transcript' })).closest<HTMLElement>('.overflow-y-auto')!;
  return { ws, scroller, box: layout(scroller) };
}
const grow = (box: { height: number }, to: number) => act(() => { box.height = to; observers.forEach((cb) => cb()); });
const scrollTo = (scroller: HTMLElement, box: { top: number }, top: number) => act(() => { box.top = top; scroller.dispatchEvent(new Event('scroll')); });

test('a session opens at its newest message even when content above loads late', async () => {
  const { box } = await open();
  await grow(box, 3000);
  expect(box.top).toBe(3000);
  await grow(box, 5000);
  expect(box.top).toBe(5000);
  expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull();
});

test('scrolled up, new messages do not move the reader, and Jump to latest returns to the bottom', async () => {
  const { ws, scroller, box } = await open();
  await grow(box, 3000);
  await scrollTo(scroller, box, 500);
  await grow(box, 3200);
  expect(box.top).toBe(500);
  act(() => ws.frame(view({ run: true })));
  const jump = await screen.findByRole('button', { name: 'Jump to latest' });
  await userEvent.click(jump);
  expect(box.top).toBe(3200);
  expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull();
  await grow(box, 3600);
  expect(box.top).toBe(3600);
});
