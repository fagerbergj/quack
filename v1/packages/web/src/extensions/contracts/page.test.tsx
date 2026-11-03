import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, test, vi } from 'vitest';
import type { IndexArtifact } from '../../api.ts';
import type { Row } from '../../sessions.ts';
import { Contracts } from './Contracts.tsx';

const art = (kind: string, version: number, over: Partial<IndexArtifact> = {}): IndexArtifact => ({ session: 's', kind, version, verdict: null, sealedAt: 1, ...over });
const spec = (version: number, title = 'Sleeper schedule endpoint') => art('contract.spec', version, { verdict: { passed: true }, summary: `${title}, version ${version}`.slice(0, 80), outcome: `v${version}` });
const row = (id: string, over: Partial<Row> = {}): Row => ({ key: `contracts:${id}`, agent: 'contracts', title: `Draft ${id}`, running: false, active: false, createdAt: '', lastActivity: '', approvals: [], questions: [], failures: [], artifacts: [], multiRepo: false, ...over });

afterEach(() => vi.unstubAllGlobals());

test('each contract reads as drafting, awaiting approval, approved or changes requested', () => {
  const rows = [
    row('a', { active: true }),
    row('b', { approvals: [{ id: 'x', tool: 'propose_contract', args: { summary: 'Approve GET /apps/sleeper/schedule v2' }, requestedAt: 1 }], artifacts: [spec(1), spec(2)] }),
    row('c', { artifacts: [spec(1, 'Waivers'), art('contract.approved', 1, { outcome: 'v1' })] }),
    row('d', { artifacts: [spec(1, 'Lineup'), art('contract.approved', 1, { outcome: 'v1' }), spec(2, 'Lineup')] }),
    row('e', { artifacts: [spec(1, 'Trades'), art('contract.changes', 1, { outcome: 'v1', summary: 'Use cursor pagination' })] }),
    row('f', { key: 'sleeper:x:1:lineup' }),
  ];
  render(<Contracts rows={rows} />);
  const items = screen.getAllByRole('listitem');
  expect(items).toHaveLength(5);
  const by = (name: string) => items.find((li) => within(li).queryByText(name))!;
  expect(by('Draft a')).toHaveTextContent('Drafting');
  expect(by('Sleeper schedule endpoint')).toHaveTextContent('REST · v2Awaiting your approval');
  expect(by('Waivers')).toHaveTextContent('REST · v1Approved v1');
  expect(by('Lineup')).toHaveTextContent('REST · v2 · v2 is not approved yetApproved v1');
  expect(by('Trades')).toHaveTextContent('Changes requested');
  expect(items[0]).toHaveTextContent('Awaiting your approval');
  expect(within(by('Waivers')).getByRole('link')).toHaveAttribute('href', '#/s/contracts%3Ac');
});

test('a title over 80 characters keeps its version, and Changes requested shows the reason', () => {
  const long = 'A very long contract title that runs well past the eighty character summary limit of the index';
  const rows = [row('l', { artifacts: [spec(3, long), art('contract.changes', 1, { outcome: 'v3', summary: 'Use cursor pagination' })] })];
  render(<Contracts rows={rows} />);
  expect(screen.getByRole('listitem')).toHaveTextContent('REST · v3 · Use cursor paginationChanges requested');
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

test('a run still finishing its reply after the approval reads Approved, not Drafting; a newer draft in progress reads Drafting', () => {
  const rows = [
    row('g', { active: true, artifacts: [spec(1, 'Schedule'), art('contract.approved', 1, { outcome: 'v1' })] }),
    row('h', { active: true, artifacts: [spec(1, 'Roster'), art('contract.approved', 1, { outcome: 'v1' }), spec(2, 'Roster')] }),
  ];
  render(<Contracts rows={rows} />);
  const by = (name: string) => screen.getAllByRole('listitem').find((li) => within(li).queryByText(name))!;
  expect(by('Schedule')).toHaveTextContent('REST · v1Approved v1');
  expect(by('Roster')).toHaveTextContent('REST · v2 · v1 approvedDrafting');
});
