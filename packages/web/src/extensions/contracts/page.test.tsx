import { render, screen, waitFor, within } from '@testing-library/react';
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

test("each contract shows its kind, version and approval status from the contracts extension's own read", async () => {
  const records = [
    { key: 'contracts:a', title: 'Sleeper schedule endpoint', kind: 'rest', draft: 2, proposed: 2, approved: 1, pendingApproval: true },
    { key: 'contracts:b', title: 'Extension SDK', kind: 'typescript', draft: 3, proposed: 3, changesRequested: 3, pendingApproval: false },
    { key: 'contracts:c', title: 'Payments API', kind: 'json-schema', draft: 4, proposed: 3, approved: 3, pendingApproval: false },
  ];
  // Two pages: the page reads every record, following nextPageToken.
  const pages: Record<string, object> = {
    'apps/contracts/contracts?pageSize=1000': { contracts: records.slice(0, 2), nextPageToken: 'a b' },
    'apps/contracts/contracts?pageSize=1000&pageToken=a+b': { contracts: records.slice(2) },
  };
  const fetchMock = vi.fn(async (url: string) => (pages[url] ? Response.json(pages[url]) : new Response('{}', { status: 404 })));
  vi.stubGlobal('fetch', fetchMock);
  render(<Contracts rows={[row('a'), row('b'), row('c'), row('new')]} />);
  await waitFor(() => expect(screen.getAllByRole('listitem')[0]).toHaveTextContent('Sleeper schedule endpoint'));
  expect(screen.getAllByRole('listitem').map((li) => li.textContent)).toEqual([
    'Sleeper schedule endpointREST · v2Awaiting your approval',
    'Extension SDKTypeScript · v3Changes requested',
    'Draft newIdle',
    'Payments APIJSON Schema · v4 · v4 is not approved yetApproved v3',
  ]);
  expect(fetchMock.mock.calls.map(([url]) => url)).toEqual(Object.keys(pages));
});

test('the page still lists contracts from their rows when the contracts app is not mounted', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response('{}', { status: 404 })));
  render(<Contracts rows={[row('b', { state: 'running', pendingAsks: 1 })]} />);
  expect(screen.getByRole('listitem')).toHaveTextContent('Draft bWaiting on you');
});

test('with no contracts it says what one is', () => {
  render(<Contracts rows={[]} />);
  expect(screen.getByText(/A contract is the agreed shape of an interface/)).toBeInTheDocument();
});

test('New contract asks for the description and starts the contracts agent', async () => {
  const calls: Array<[string, RequestInit]> = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => (url.startsWith('sessions/') && calls.push([url, init]), new Response('{}', { status: 202 }))));
  render(<Contracts rows={[]} />);
  await userEvent.click(screen.getByRole('button', { name: 'New contract' }));
  await userEvent.type(screen.getByLabelText('What should it do, and for whom?'), 'GET /x for the page');
  await userEvent.click(screen.getByRole('button', { name: 'Start' }));
  expect(calls).toHaveLength(1);
  expect(calls[0]![0]).toMatch(/^sessions\/contracts%3A[0-9a-f-]{36}\/invocations\?agent=contracts$/);
  expect(JSON.parse(String(calls[0]![1].body))).toMatchObject({ input: 'GET /x for the page', title: 'GET /x for the page' });
  expect(location.hash).toMatch(/^#\/s\/contracts%3A/);
});
