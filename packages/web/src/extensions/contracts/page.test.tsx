import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, test, vi } from 'vitest';
import type { Session } from '../../api.ts';
import { rowOf } from '../../sessions.ts';
import { failedRun, sess } from '../../test-harness.ts';
import { Contracts } from './Contracts.tsx';

const row = (id: string, over: Partial<Session> | object = {}) => rowOf(sess(`contracts:${id}`, { agent: 'contracts', title: `Draft ${id}`, ...over }) as Session, false);

afterEach(() => vi.unstubAllGlobals());

const names = (group: string) => within(screen.getByRole('region', { name: group })).getAllByRole('link').map((a) => a.textContent);
const mark = (link: HTMLElement) => within(link).getByRole('img');

test('each contract reads from its catalog row: waiting on you first, then drafting, a failed run, then one with no draft', () => {
  const rows = [
    row('idle', { lastRun: { outcome: 'succeeded', endedAt: '', failedSteps: [] } }),
    row('a', { state: 'running' }),
    row('b', { state: 'running', pendingAsks: 1 }),
    row('c', failedRun('propose', 'boom')),
    row('archived', { state: 'archived' }),
    rowOf(sess('sleeper:x:1:lineup') as Session, false),
  ];
  render(<Contracts rows={rows} />);
  expect(names('In progress')).toEqual(['Draft b!', 'Draft a', 'Draft c✗', 'Draft idle']);
  expect(screen.queryByRole('region', { name: 'Approved' })).toBeNull();
  const links = within(screen.getByRole('region', { name: 'In progress' })).getAllByRole('link');
  expect(links.map((l) => mark(l).getAttribute('aria-label'))).toEqual(['Waiting on you', 'Drafting', 'Run failed', 'No draft yet']);
  expect(links[0]).toHaveAttribute('href', '#/s/contracts%3Ab');
});

const records = [
  { key: 'contracts:a', title: 'Sleeper schedule endpoint', kind: 'rest', draft: 2, proposed: 2, approved: 1, pendingApproval: true },
  { key: 'contracts:b', title: 'Extension SDK', kind: 'typescript', draft: 3, proposed: 3, changesRequested: 3, pendingApproval: false },
  { key: 'contracts:c', title: 'Payments API', kind: 'json-schema', draft: 4, proposed: 3, approved: 3, pendingApproval: false },
  { key: 'contracts:d', title: 'Older API', kind: 'rest', draft: 7, proposed: 7, approved: 7, pendingApproval: false },
];

test("each row is its kind tag, its name, and one version with a mark that names the status", async () => {
  // Two pages: the page reads every record, following nextPageToken.
  const pages: Record<string, object> = {
    'apps/contracts/contracts?pageSize=1000': { contracts: records.slice(0, 2), nextPageToken: 'a b' },
    'apps/contracts/contracts?pageSize=1000&pageToken=a+b': { contracts: records.slice(2) },
  };
  const fetchMock = vi.fn(async (url: string) => (pages[url] ? Response.json(pages[url]) : new Response('{}', { status: 404 })));
  vi.stubGlobal('fetch', fetchMock);
  const at = (day: number) => ({ lastActivity: `2026-11-${day}T01:00:00.000Z` });
  render(<Contracts rows={[row('a'), row('b'), row('c', at(10)), row('d', at(12)), row('new')]} />);
  await waitFor(() => expect(names('Approved')).toHaveLength(2));
  expect(names('In progress')).toEqual(['RESTSleeper schedule endpointv2!', 'TSExtension SDKv3✗', 'Draft new']);
  expect(names('Approved')).toEqual(['RESTOlder APIv7✓', 'JSONPayments APIv3✓']);
  const status = (name: string) => mark(screen.getByRole('link', { name: new RegExp(name) })).getAttribute('aria-label');
  expect(status('Sleeper schedule')).toBe('Awaiting your approval');
  expect(status('Extension SDK')).toBe('Changes requested');
  expect(status('Payments API')).toBe('Approved; v4 is not approved yet');
  expect(screen.getByText('TS')).toHaveAttribute('title', 'TypeScript');
  expect(fetchMock.mock.calls.map(([url]) => url)).toEqual(Object.keys(pages));
});

test('a row says its version once and never prints its status: the group heading and the mark carry it', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => Response.json({ contracts: records.slice(2, 3) })));
  render(<Contracts rows={[row('c')]} />);
  await waitFor(() => expect(names('Approved')).toHaveLength(1));
  const link = screen.getByRole('link', { name: /Payments API/ });
  expect(link.textContent?.match(/v\d+/g)).toEqual(['v3']);
  expect(link).not.toHaveTextContent(/Approved|Draft|Changes/);
  expect(mark(link)).toHaveAttribute('title', 'Approved; v4 is not approved yet');
});

test('the Approved group shows ten, newest first, and a button opens the rest', async () => {
  const many = Array.from({ length: 12 }, (_, i) => ({ key: `contracts:m${i}`, title: `API ${i}`, kind: 'rest', draft: 1, proposed: 1, approved: 1, pendingApproval: false }));
  vi.stubGlobal('fetch', vi.fn(async () => Response.json({ contracts: many })));
  render(<Contracts rows={many.map((_, i) => row(`m${i}`, { lastActivity: `2026-11-${String(i + 1).padStart(2, '0')}T01:00:00.000Z` }))} />);
  await waitFor(() => expect(names('Approved')).toHaveLength(10));
  expect(names('Approved')[0]).toBe('RESTAPI 11v1✓');
  await userEvent.click(screen.getByRole('button', { name: '2 more approved' }));
  expect(names('Approved')).toHaveLength(12);
});

test('the page still lists contracts from their rows when the contracts app is not mounted', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response('{}', { status: 404 })));
  render(<Contracts rows={[row('b', { state: 'running', pendingAsks: 1 })]} />);
  expect(mark(screen.getByRole('link', { name: /Draft b/ }))).toHaveAccessibleName('Waiting on you');
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

test('a long contract name wraps to two lines beside its status instead of truncating to one', () => {
  render(<Contracts rows={[row('a')]} />);
  expect(screen.getByText('Draft a')).toHaveClass('line-clamp-2');
});
