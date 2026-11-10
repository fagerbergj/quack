import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test } from 'vitest';
import { App } from '../../App.tsx';
import { SUPERSEDED } from '../../SessionView.tsx';
import type { SealedArtifact, Verdict } from '../../api.ts';
import { entry, FakeSocket, fetchMock, live, routes, snapshot, stubViewport, useHarness } from '../../test-harness.ts';
import { diffUnits, units } from './diff.ts';
import v1 from './fixtures/sleeper-schedule-v1.json';
import v2 from './fixtures/sleeper-schedule-v2.json';
import settings from './fixtures/sleeper-settings.json';
import { approvalState } from './Studio.tsx';
import { declarations } from './typescript-model.ts';
import type { ContractSpec } from './spec.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /sessions/contracts%3As1/asks': { asks: [] } }));

const KEY = 'contracts%3As1';
const pass: Verdict = { passed: true, gatesFailed: [], score: 1, checks: [] };
const fail: Verdict = { passed: false, gatesFailed: ['justified.links_resolve'], score: 0.5, checks: [{ criterion: 'justified', id: 'links_resolve', label: "A choice's link doesn't resolve", weight: 'gate', met: 0, notes: [] }] };
const spec = (version: number, content: object, verdict: Verdict | null = pass): SealedArtifact => ({ kind: 'contract.spec', version, hash: `h${version}`, content: JSON.stringify(content), sealedAt: version, verdict });
const approvedRec = (artifact: number, version: number): SealedArtifact => ({ kind: 'contract.approved', version: 1, hash: 'r', content: JSON.stringify({ version, artifact, note: `Approved version ${version}`, approval: 'old' }), sealedAt: 10, verdict: null });
const proposal = (artifact: number, version: number, breaks: object[] = []) => ({ id: 'p/1', tool: 'propose_contract', args: { summary: `Approve GET /apps/sleeper/schedule v${version}`, version, artifact, hash: `h${artifact}`, findings: [], breaks }, requestedAt: 20 });

// The contracts app's status read, as the page fetches it for this session.
const STATUS = `GET /apps/contracts/contracts/${KEY}`;
const status = (...versions: Array<[number, string]>) => ({ key: 'contracts:s1', versions: versions.map(([artifact, st]) => ({ artifact, version: artifact, status: st })) });

// Beside the chat the pane is a region; on a phone it is the tab panel its tab ("Contract", with a count) names.
const contract = () => screen.queryByRole('region', { name: 'Contract' }) ?? screen.getByRole('tabpanel', { name: /^Contract/ });

function open(artifacts: SealedArtifact[], approvals: object[] = []) {
  live[KEY] = { artifacts, approvals };
  location.hash = `#/s/${KEY}`;
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot());
  return { ws, pane: contract() };
}
const bar = () => screen.getByRole('region', { name: 'Approval' });
const side = (name: 'from' | 'to') => screen.getByRole('button', { name: new RegExp(`^Compare ${name}: `) });
// The versions a side's menu offers, read by opening it and closing it again.
async function offered(name: 'from' | 'to') {
  await userEvent.click(side(name));
  const items = within(screen.getByRole('menu', { name: `Compare ${name}` })).getAllByRole('menuitem').map((i) => i.textContent);
  await userEvent.keyboard('{Escape}');
  return items;
}
const posts = () => fetchMock.mock.calls.filter(([, i]) => i?.method === 'PATCH').map(([u, i]) => [String(u), JSON.parse(String(i!.body))]);

const crumbs = (d: { changes: Array<{ mark: string; crumb: string; rows: unknown[] }> }) => d.changes.map((c) => `${c.mark} ${c.crumb}`);

test('a semantic diff marks the elements that changed by pointer, and lists each change where it is with its text before and after', () => {
  const rest = diffUnits(units(v1 as ContractSpec), units(v2 as ContractSpec));
  expect(rest.marks.get('/paths/~1apps~1sleeper~1schedule/get')).toBe('changed');
  expect([...rest.marks].some(([p, m]) => m === 'added' && p.endsWith('/status'))).toBe(true);
  expect(rest.changes.find((c) => c.crumb === 'GET /apps/sleeper/schedule › 200 › runs › description')?.rows).toEqual([{ was: 'Upcoming runs, soonest first', now: 'Runs from the start of the week, oldest first' }]);
  expect(rest.changes.find((c) => c.crumb === 'GET /apps/sleeper/schedule › 200 › runs › status')).toMatchObject({ mark: 'added', rows: expect.arrayContaining([{ name: 'type', was: '', now: 'string' }]) });
});

test('the breadcrumb names where a change is: route › parameter › part, schema path › part, declaration › member › part', () => {
  const s = v1 as ContractSpec;
  const doc = (desc: string) => ({ ...s, document: { paths: { '/contracts/{key}': { get: { parameters: [{ name: 'key', in: 'path', required: true, description: desc, schema: { type: 'string' } }], responses: {} } } } } });
  expect(crumbs(diffUnits(units(doc('A key no session answers for')), units(doc('A key that does not start with contracts:'))))).toEqual(['changed GET /contracts/{key} › key › description']);
  const json = settings as ContractSpec;
  const d = json.document as { properties: Record<string, any> };
  const nested = { ...json, document: { ...d, properties: { ...d.properties, notify: { ...d.properties.notify, properties: { notifyTime: { ...d.properties.notify.properties.notifyTime, description: 'When the digest goes out.' } } } } } };
  expect(crumbs(diffUnits(units(json), units(nested)))).toEqual(['changed notify › notifyTime › description']);
  const { leagueId: _, ...rest } = d.properties;
  expect(crumbs(diffUnits(units(json), units({ ...json, document: { ...d, properties: rest } })))).toEqual(['removed leagueId']);
  const ts = (src: string) => units({ ...json, kind: 'typescript', document: src }, declarations(src));
  const code = diffUnits(ts('export type A = { n: number };'), ts('export type A = { n: string; m?: B };\nexport type B = { x: number };'));
  expect([...code.marks]).toEqual([['/A/n', 'changed'], ['/A/m', 'added'], ['/B', 'added'], ['/B/x', 'added']]);
  expect(crumbs(code)).toEqual(['changed A › n › type', 'added A › m', 'added B', 'added B › x']);
  expect(code.changes[0]!.rows).toEqual([{ was: 'number', now: 'string' }]);
});

test('only a version put up for approval can be approved; every other status says why not, a verdict explaining a draft', () => {
  const v = (verdict?: Verdict | null) => ({ a: verdict === undefined ? bare(2, v2) : spec(2, v2, verdict), spec: v2 as ContractSpec });
  expect(approvalState(v(), undefined, undefined, false)).toEqual({ can: false, text: 'Reading where version 2 stands…' });
  expect(approvalState(v(null), undefined, undefined, true, 'draft')).toEqual({ can: false, text: "Version 2 can't be approved yet: its checks are still running." });
  expect(approvalState(v(fail), undefined, undefined, true, 'draft').text).toBe("Version 2 can't be approved: it didn't pass its checks (A choice's link doesn't resolve). The agent is revising it.");
  expect(approvalState(v({ ...fail, limitReached: true }), undefined, undefined, false, 'draft').text).toMatch(/Tell the agent what to change\.$/);
  expect(approvalState(v(), undefined, undefined, true, 'draft').text).toBe('Version 2 is a draft: the agent puts it up for approval once it passes its checks.');
  expect(approvalState(v(), undefined, undefined, true, 'proposed').can).toBe(false);
  expect(approvalState(v(), undefined, undefined, false, 'proposed').text).toBe('Version 2 is not up for approval. Send a message and the agent puts it up again.');
  expect(approvalState(v(), undefined, undefined, false, 'changes_requested').text).toBe('You asked for changes to version 2.');
  expect(approvalState(v(), proposal(2, 2), undefined, true, 'proposed')).toEqual({ can: true, text: 'Version 2 passed its checks.' });
  expect(approvalState(v(), undefined, 2, false, 'approved')).toEqual({ can: false, done: true, text: 'Version 2 is approved.' });
});

test('until the status answers, nothing is compared and no version carries a status label', async () => {
  const { pane } = open([spec(1, v1), approvedRec(1, 1), spec(2, v2)]);
  await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(`apps/contracts/contracts/${KEY}`));
  expect(within(pane).queryByRole('heading', { name: /^(\d+ changes?|No changes)/ })).toBeNull();
  expect(side('from')).toHaveAccessibleName('Compare from: None: the whole contract');
  expect(await offered('from')).toEqual(['None: the whole contract', 'Version 2', 'Version 1']);
  expect(within(bar()).getByRole('status')).toHaveTextContent('Reading where version 2 stands…');
});

test('the contract sits beside the conversation with what changed since the approved version, and approving it decides the waiting proposal', async () => {
  routes[`PATCH /sessions/${KEY}/asks/p%2F1`] = { resolved: true };
  routes[STATUS] = status([1, 'approved'], [2, 'proposed']);
  const { pane } = open([spec(1, v1), approvedRec(1, 1), spec(2, v2)], [proposal(2, 2, [{ text: 'runs are now oldest first' }])]);
  const changes = (await within(pane).findByRole('heading', { name: /^\d+ changes since the approved version$/ })).closest('section')!;
  const order = within(changes).getByText('GET /apps/sleeper/schedule › 200 › runs › description').closest('li')!;
  expect(order.querySelector('del')).toHaveTextContent('Upcoming runs, soonest');
  expect(order.querySelector('ins')).toHaveTextContent('Runs from the start of the week, oldest');
  expect(order, "the agent's why is the secondary line").toHaveTextContent('So the strip reads left to right through the week');
  expect(within(pane).queryByText('Changed from version 1'), 'the real diff replaces the agent prose card').toBeNull();
  expect(within(pane).getAllByText('New').length).toBeGreaterThan(0);
  expect(within(bar()).getByText(/Breaks callers of the approved version, checked in code: runs are now oldest first/)).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: /^Reject/ }), 'the pane decides it, not a transcript card').toBeNull();
  await userEvent.click(within(bar()).getByRole('button', { name: 'Approve' }));
  expect(posts()).toEqual([[`sessions/${KEY}/asks/p%2F1`, { resolution: { choice: 'approve' } }]]);
  await waitFor(() => expect(within(bar()).getByRole('button', { name: 'Approve' })).toBeDisabled());
});

test('each sealed draft replaces the shown one live: checking, then failed, each refused with its reason', async () => {
  routes[STATUS] = status([1, 'approved']);
  const { ws } = open([spec(1, v1), approvedRec(1, 1)]);
  await waitFor(() => expect(side('to')).toHaveAccessibleName('Compare to: Version 1, approved'));
  expect(screen.queryByRole('region', { name: 'Approval' }), 'an approved newest version is a badge, not a bar').toBeNull();
  routes[STATUS] = status([1, 'approved'], [2, 'draft']);
  ws.frame({ type: 'artifacts', artifacts: [spec(1, v1), approvedRec(1, 1), spec(2, v2, null)] });
  expect(await within(bar()).findByText("Version 2 can't be approved yet: its checks are still running.")).toBeInTheDocument();
  expect(await screen.findByRole('heading', { name: /since the approved version$/ })).toBeInTheDocument();
  expect(side('from')).toHaveAccessibleName('Compare from: Version 1, approved');
  expect(within(bar()).getByRole('button', { name: 'Approve' })).toBeDisabled();
  expect(contract()).toHaveTextContent('Runs from the start of the week');
  ws.frame({ type: 'artifacts', artifacts: [spec(1, v1), approvedRec(1, 1), spec(2, v2, fail)] });
  expect(within(bar()).getByRole('status')).toHaveTextContent("didn't pass its checks (A choice's link doesn't resolve)");
  expect(within(bar()).getByRole('button', { name: 'Approve' })).toBeDisabled();
  expect(posts()).toEqual([]);
});

test('a message sent while a version waits is steered into the run and declines the proposal', async () => {
  routes[`PATCH /sessions/${KEY}/asks/p%2F1`] = { resolved: true };
  const { ws } = open([spec(1, v1), spec(2, v2)], [proposal(2, 2)]);
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'Why oldest first?');
  await userEvent.click(screen.getByRole('button', { name: 'Send' }));
  const input = ws.sent.find((f) => (f as { type: string }).type === 'input') as { requestId: string; whenBusy: string; input: string };
  expect([input.input, input.whenBusy]).toEqual(['Why oldest first?', 'steer']);
  ws.frame({ type: 'accepted', requestId: input.requestId });
  await waitFor(() => expect(posts()).toEqual([[`sessions/${KEY}/asks/p%2F1`, { resolution: { choice: 'reject', reason: SUPERSEDED } }]]));
});

test('an approval another tab already decided says so instead of failing silently', async () => {
  routes[`PATCH /sessions/${KEY}/asks/p%2F1`] = new (await import('../../test-harness.ts')).Fail(409, { detail: 'unknown or already decided' });
  open([spec(1, v1), spec(2, v2)], [proposal(2, 2)]);
  await userEvent.click(within(bar()).getByRole('button', { name: 'Approve' }));
  expect(await within(bar()).findByRole('alert')).toHaveTextContent('already decided');
});

// A version as the catalog lists it: no verdict at all.
const bare = (version: number, content: object): SealedArtifact => { const { verdict: _, ...a } = spec(version, content); return a; };
const tsSpec = (version: number, document: string) => ({ kind: 'typescript', title: 'Shapes', version, intent: { purpose: 'Shapes' }, choices: [], document });

// Version 1 approved, then version 2 put up for approval, neither carrying a verdict.
async function proposeTwo(one: object, two: object) {
  routes[STATUS] = status([1, 'approved']);
  const { ws, pane } = open([bare(1, one)]);
  await waitFor(() => expect(side('to')).toHaveAccessibleName('Compare to: Version 1, approved'));
  routes[STATUS] = status([1, 'approved'], [2, 'proposed']);
  ws.frame({ type: 'artifacts', artifacts: [bare(1, one), bare(2, two)] });
  ws.frame({ type: 'standing', questions: [], failures: [], approvals: [proposal(2, 2)] });
  return pane;
}

test('with no verdicts, a REST contract marks the operations that changed since the approved version', async () => {
  const pane = await proposeTwo(v1, v2);
  expect(await within(pane).findByRole('heading', { name: /changes since the approved version$/ })).toBeInTheDocument();
  expect(within(pane).getAllByText('Changed').length).toBeGreaterThan(0);
  expect(within(pane).getAllByText('New').length).toBeGreaterThan(0);
  expect(within(bar()).getByRole('status')).toHaveTextContent('Version 2 passed its checks.');
  expect(within(bar()).getByRole('button', { name: 'Approve' })).toBeEnabled();
});

test('with no verdicts, a TypeScript contract marks the declarations that changed and the ones added', async () => {
  const pane = await proposeTwo(tsSpec(1, 'export type A = { n: number };'), tsSpec(2, 'export type A = { n: string; m?: B };\nexport type B = { x: number };'));
  const changes = (await within(pane).findByRole('heading', { name: '4 changes since the approved version' })).closest('section')!;
  const n = within(changes).getByText('A › n › type').closest('li')!;
  expect([n.querySelector('del')?.textContent, n.querySelector('ins')?.textContent]).toEqual(['Removed: number', 'Added: string']);
  // The cards render once the lazy code-contract chunk and the TypeScript model load, after the summary heading.
  expect((await within(await within(pane).findByRole('article', { name: 'B' })).findAllByText('New')).length).toBeGreaterThan(0);
  expect(await within(await within(pane).findByRole('article', { name: 'A' })).findByText('Changed')).toBeInTheDocument();
});

test("each version is labelled by where it stands with the person, and the approval bar reads the newest one's status", async () => {
  routes[STATUS] = status([1, 'approved'], [2, 'changes_requested'], [3, 'proposed'], [4, 'draft']);
  open([bare(1, v1), approvedRec(1, 1), bare(2, v2), bare(3, v2), bare(4, v2)]);
  await waitFor(() => expect(side('to')).toHaveAccessibleName('Compare to: Version 2 (#4), draft'));
  expect(await offered('to')).toEqual(['Version 2 (#4)draft, draft', 'Version 2 (#3)awaiting, awaiting approval', 'Version 2 (#2)changes asked, changes requested', 'Version 1approved, approved']);
  expect(within(bar()).getByRole('status')).toHaveTextContent('Version 2 is a draft and not up for approval.');
  expect(side('from')).toHaveAccessibleName('Compare from: Version 1, approved');
  const v = { a: bare(2, v2), spec: v2 as ContractSpec };
  expect(approvalState(v, undefined, undefined, false, 'changes_requested').text).toBe('You asked for changes to version 2.');
  expect(approvalState(v, undefined, undefined, true, 'draft').text).toBe('Version 2 is a draft: the agent puts it up for approval once it passes its checks.');
  expect(approvalState(v, undefined, undefined, false, 'proposed').text).toBe('Version 2 is not up for approval. Send a message and the agent puts it up again.');
  expect(approvalState(v, undefined, 2, false, 'approved').done).toBe(true);
});

test('on a phone the contract and the chat are tabs, and the contract tab counts the questions it shows', async () => {
  stubViewport(false);
  const { ws } = open([spec(1, v1)]);
  const tabs = screen.getByRole('tablist', { name: 'Show' });
  expect(within(tabs).getByRole('tab', { name: 'Contract' })).toHaveAttribute('aria-selected', 'true');
  ws.frame({ type: 'standing', questions: [{ id: 'q', question: 'Cursor or offset?', options: ['Cursor'], evidence: null, askedAt: 1 }], failures: [], approvals: [] });
  expect(within(tabs).getByRole('tab', { name: 'Contract 1' })).toHaveAttribute('aria-selected', 'true');
  expect(within(contract()).getByRole('region', { name: 'Question' })).toHaveTextContent('Cursor or offset?');
  await userEvent.click(within(tabs).getByRole('tab', { name: 'Chat' }));
  expect(within(tabs).getByRole('tab', { name: 'Chat' })).toHaveAttribute('aria-selected', 'true');
});

const decidedAsk = { session: 'contracts:s1', id: 'q0', type: 'question', state: 'resolved', createdAt: '2026-11-01T01:00:00.000Z', prompt: { question: 'Paging?', title: 'Cursor or offset paging', options: ['Cursor', 'Offset'], freeText: false }, resolution: { by: { kind: 'tailnet' }, choice: 'Cursor' } };

test('before the first version the Contract tab opens on the waiting question, says what the run is doing, and keeps the brief and the decisions so far', async () => {
  stubViewport(false);
  routes[`PATCH /sessions/${KEY}/asks/q1`] = { resolved: true };
  routes[`GET /sessions/${KEY}/asks`] = { asks: [decidedAsk] };
  live[KEY] = { artifacts: [], approvals: [] };
  location.hash = `#/s/${KEY}`;
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot({ entries: [entry('u', [{ role: 'user', content: 'Define GET /roster for the roster page', timestamp: 1 }])] }));
  ws.frame({ type: 'standing', questions: [{ id: 'q1', question: 'Which auth?', options: ['Token', 'None'], evidence: null, askedAt: 1 }], failures: [], approvals: [] });
  expect(within(screen.getByRole('tablist', { name: 'Show' })).getByRole('tab', { name: 'Contract 1' })).toHaveAttribute('aria-selected', 'true');
  const pane = contract();
  const question = within(pane).getByRole('region', { name: 'Question' });
  expect(screen.getAllByRole('region', { name: 'Question' }), 'the chat does not repeat it').toHaveLength(1);
  expect(within(within(pane).getByRole('region', { name: 'Progress' })).getByRole('status')).toHaveTextContent('Waiting on you');
  expect(within(pane).getByText('Brief').closest('details')).not.toHaveAttribute('open');
  expect(within(pane).getByText('Define GET /roster for the roster page')).toBeInTheDocument();
  expect((await within(pane).findByText('Cursor or offset paging')).closest('li')).toHaveTextContent('Decided: Cursor or offset paging →: Cursor');
  await userEvent.click(within(question).getByRole('button', { name: 'Token' }));
  expect(posts()).toEqual([[`sessions/${KEY}/asks/q1`, { resolution: { choice: 'Token' } }]]);
});
