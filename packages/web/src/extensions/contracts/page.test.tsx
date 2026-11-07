import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, test, vi } from 'vitest';
import type { Session } from '../../api.ts';
import { rowOf } from '../../sessions.ts';
import { failedRun, sess } from '../../test-harness.ts';
import { Contracts } from './Contracts.tsx';

const row = (id: string, over: Partial<Session> | object = {}) => rowOf(sess(`contracts:${id}`, { agent: 'contracts', title: `Draft ${id}`, ...over }) as Session, false);

afterEach(() => vi.unstubAllGlobals());

test('each contract reads from its catalog row: waiting on you first, then drafting, a failed run, then idle', () => {
  const rows = [
    row('idle', { lastRun: { outcome: 'succeeded', endedAt: '', failedSteps: [] } }),
    row('a', { state: 'running' }),
    row('b', { state: 'running', pendingAsks: 1 }),
    row('c', failedRun('propose', 'boom')),
    row('archived', { state: 'archived' }),
    rowOf(sess('sleeper:x:1:lineup') as Session, false),
  ];
  render(<Contracts rows={rows} />);
  const items = screen.getAllByRole('listitem');
  expect(items.map((li) => li.textContent)).toEqual(['Draft bWaiting on you', 'Draft aDrafting', 'Draft cRun failed', 'Draft idleIdle']);
  expect(within(items[0]!).getByRole('link')).toHaveAttribute('href', '#/s/contracts%3Ab');
});

test('with no contracts it says what one is', () => {
  render(<Contracts rows={[]} />);
  expect(screen.getByText(/A contract is the agreed shape of an interface/)).toBeInTheDocument();
});

test('New contract asks for the description and starts the contracts agent', async () => {
  const calls: Array<[string, RequestInit]> = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => (calls.push([url, init]), new Response('{}', { status: 202 }))));
  render(<Contracts rows={[]} />);
  await userEvent.click(screen.getByRole('button', { name: 'New contract' }));
  await userEvent.type(screen.getByLabelText('What should it do, and for whom?'), 'GET /x for the page');
  await userEvent.click(screen.getByRole('button', { name: 'Start' }));
  expect(calls).toHaveLength(1);
  expect(calls[0]![0]).toMatch(/^sessions\/contracts%3A[0-9a-f-]{36}\/invocations\?agent=contracts$/);
  expect(JSON.parse(String(calls[0]![1].body))).toMatchObject({ input: 'GET /x for the page', title: 'GET /x for the page' });
  expect(location.hash).toMatch(/^#\/s\/contracts%3A/);
});
