import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, expect, test } from 'vitest';
import { App } from '../../App.tsx';
import { SUPERSEDED } from '../../SessionView.tsx';
import type { SealedArtifact, Verdict } from '../../api.ts';
import { FakeSocket, fetchMock, live, routes, snapshot, stubViewport, useHarness } from '../../test-harness.ts';
import { diffUnits, units } from './diff.ts';
import v1 from './fixtures/sleeper-schedule-v1.json';
import v2 from './fixtures/sleeper-schedule-v2.json';
import settings from './fixtures/sleeper-settings.json';
import { approvalState } from './Studio.tsx';
import { declarations } from './typescript-model.ts';
import type { ContractSpec } from './spec.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] } }));
// What a viewer last looked at lives in this browser; each test is a viewer of its own.
beforeEach(() => localStorage.clear());

const KEY = 'contracts%3As1';
const pass: Verdict = { passed: true, gatesFailed: [], score: 1, checks: [] };
const fail: Verdict = { passed: false, gatesFailed: ['justified.links_resolve'], score: 0.5, checks: [{ criterion: 'justified', id: 'links_resolve', label: "A choice's link doesn't resolve", weight: 'gate', met: 0, notes: [] }] };
const spec = (version: number, content: object, verdict: Verdict | null = pass): SealedArtifact => ({ kind: 'contract.spec', version, hash: `h${version}`, content: JSON.stringify(content), sealedAt: version, verdict });
const approvedRec = (artifact: number, version: number): SealedArtifact => ({ kind: 'contract.approved', version: 1, hash: 'r', content: JSON.stringify({ version, artifact, note: `Approved version ${version}`, approval: 'old' }), sealedAt: 10, verdict: null });
const proposal = (artifact: number, version: number, breaks: object[] = []) => ({ id: 'p/1', tool: 'propose_contract', args: { summary: `Approve GET /apps/sleeper/schedule v${version}`, version, artifact, hash: `h${artifact}`, findings: [], breaks }, requestedAt: 20 });

// The contracts app's status read, as the page fetches it for this session.
const STATUS = `GET /apps/contracts/contracts/${KEY}`;
const status = (...versions: Array<[number, string]>) => ({ key: 'contracts:s1', versions: versions.map(([artifact, st]) => ({ artifact, version: artifact, status: st })) });

function open(artifacts: SealedArtifact[], approvals: object[] = []) {
  live[KEY] = { artifacts, approvals };
  location.hash = `#/s/${KEY}`;
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot());
  return { ws, pane: screen.getByRole('region', { name: 'Contract' }) };
}
const bar = () => screen.getByRole('region', { name: 'Approval' });
const posts = () => fetchMock.mock.calls.filter(([, i]) => i?.method === 'PATCH').map(([u, i]) => [String(u), JSON.parse(String(i!.body))]);

test('a semantic diff marks the elements that changed, by the pointer each is drawn at, and names what was removed', () => {
  const rest = diffUnits(units(v1 as ContractSpec), units(v2 as ContractSpec));
  const ptrs = [...rest.marks].map(([p, m]) => `${m} ${p}`);
  expect(ptrs.some((p) => p.startsWith('added ') && p.endsWith('/status'))).toBe(true);
  expect(rest.marks.get('/paths/~1apps~1sleeper~1schedule/get')).toBe('changed');
  const s = settings as ContractSpec;
  const doc = s.document as { properties: Record<string, unknown> };
  const [first, ...others] = Object.keys(doc.properties);
  const lost = { ...s, document: { ...doc, properties: Object.fromEntries(others.map((k) => [k, doc.properties[k]])) } };
  expect(diffUnits(units(s), units(lost)).removed).toEqual([`field › ${first}`]);
  const ts = (src: string) => units({ ...s, kind: 'typescript', document: src }, declarations(src));
  const code = diffUnits(ts('export type A = { n: number };'), ts('export type A = { n: string; m?: B };\nexport type B = { x: number };'));
  expect([...code.marks]).toEqual([['/A/n', 'changed'], ['/A/m', 'added'], ['/B', 'added'], ['/B/x', 'added']]);
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
  localStorage.setItem('quack.contracts.seen.contracts:s1', '1');
  const { pane } = open([spec(1, v1), approvedRec(1, 1), spec(2, v2)]);
  await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(`apps/contracts/contracts/${KEY}`));
  expect(within(pane).queryByRole('heading', { name: /since version/ })).toBeNull();
  const from = screen.getByRole('combobox', { name: 'Compare from' });
  expect(from).toHaveValue('');
  expect(within(from).getAllByRole('option').map((o) => o.textContent)).toEqual(['none', 'Version 2', 'Version 1']);
  expect(within(bar()).getByRole('status')).toHaveTextContent('Reading where version 2 stands…');
});

test('the contract sits beside the conversation with what changed since this viewer last looked, and approving it decides the waiting proposal', async () => {
  localStorage.setItem('quack.contracts.seen.contracts:s1', '1');
  routes[`PATCH /sessions/${KEY}/asks/p%2F1`] = { resolved: true };
  routes[STATUS] = status([1, 'approved'], [2, 'proposed']);
  const { pane } = open([spec(1, v1), approvedRec(1, 1), spec(2, v2)], [proposal(2, 2, [{ text: 'runs are now oldest first' }])]);
  expect(await within(pane).findByRole('heading', { name: /^\d+ new · \d+ changed.* since version 1$/ })).toBeInTheDocument();
  expect(within(pane).getAllByText('New').length).toBeGreaterThan(0);
  expect(within(bar()).getByText(/Breaks callers of the approved version, checked in code: runs are now oldest first/)).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: /^Reject/ }), 'the pane decides it, not a transcript card').toBeNull();
  expect(localStorage.getItem('quack.contracts.seen.contracts:s1')).toBe('2');
  await userEvent.click(within(bar()).getByRole('button', { name: 'Approve' }));
  expect(posts()).toEqual([[`sessions/${KEY}/asks/p%2F1`, { resolution: { choice: 'approve' } }]]);
  await waitFor(() => expect(within(bar()).getByRole('button', { name: 'Approve' })).toBeDisabled());
});

test('each sealed draft replaces the shown one live: checking, then failed, each refused with its reason', async () => {
  routes[STATUS] = status([1, 'approved']);
  const { ws } = open([spec(1, v1), approvedRec(1, 1)]);
  expect(await within(bar()).findByText('Version 1 is approved.')).toBeInTheDocument();
  routes[STATUS] = status([1, 'approved'], [2, 'draft']);
  ws.frame({ type: 'artifacts', artifacts: [spec(1, v1), approvedRec(1, 1), spec(2, v2, null)] });
  expect(await within(bar()).findByText("Version 2 can't be approved yet: its checks are still running.")).toBeInTheDocument();
  expect(screen.getByText('the approved version'), 'a viewer who never looked compares with the approved version').toBeInTheDocument();
  expect(screen.getByRole('combobox', { name: 'Compare from' })).toHaveValue('1');
  expect(within(bar()).getByRole('button', { name: 'Approve' })).toBeDisabled();
  expect(screen.getByRole('region', { name: 'Contract' })).toHaveTextContent('Runs from the start of the week');
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

// Versions seen one after another, none carrying a verdict: the base is the last one put to this viewer.
async function proposeTwo(one: object, two: object) {
  routes[STATUS] = status([1, 'proposed']);
  const { ws, pane } = open([bare(1, one)], [proposal(1, 1)]);
  await waitFor(() => expect(localStorage.getItem('quack.contracts.seen.contracts:s1')).toBe('1'));
  routes[STATUS] = status([1, 'proposed'], [2, 'proposed']);
  ws.frame({ type: 'artifacts', artifacts: [bare(1, one), bare(2, two)] });
  ws.frame({ type: 'standing', questions: [], failures: [], approvals: [proposal(2, 2)] });
  return pane;
}

test('with no verdicts, a REST contract marks the operations that changed since the version this viewer was shown', async () => {
  const pane = await proposeTwo(v1, v2);
  expect(await within(pane).findByRole('heading', { name: /since version 1$/ })).toBeInTheDocument();
  expect(within(pane).getAllByText('Changed').length).toBeGreaterThan(0);
  expect(within(pane).getAllByText('New').length).toBeGreaterThan(0);
  expect(within(bar()).getByRole('status')).toHaveTextContent('Version 2 passed its checks.');
  expect(within(bar()).getByRole('button', { name: 'Approve' })).toBeEnabled();
});

test('with no verdicts, a TypeScript contract marks the declarations that changed and the ones added', async () => {
  const pane = await proposeTwo(tsSpec(1, 'export type A = { n: number };'), tsSpec(2, 'export type A = { n: string; m?: B };\nexport type B = { x: number };'));
  expect(await within(pane).findByRole('heading', { name: /since version 1$/ })).toBeInTheDocument();
  // The cards render once the lazy code-contract chunk and the TypeScript model load, after the summary heading.
  expect((await within(await within(pane).findByRole('article', { name: 'B' })).findAllByText('New')).length).toBeGreaterThan(0);
  expect(await within(await within(pane).findByRole('article', { name: 'A' })).findByText('Changed')).toBeInTheDocument();
});

test("each version is labelled by where it stands with the person, and the approval bar reads the newest one's status", async () => {
  routes[STATUS] = status([1, 'approved'], [2, 'changes_requested'], [3, 'proposed'], [4, 'draft']);
  open([bare(1, v1), approvedRec(1, 1), bare(2, v2), bare(3, v2), bare(4, v2)]);
  const from = screen.getByRole('combobox', { name: 'Compare from' });
  await waitFor(() => expect(within(from).getAllByRole('option').map((o) => o.textContent)).toEqual(['none', 'Version 2 (#4), draft', 'Version 2 (#3), awaiting approval', 'Version 2 (#2), changes requested', 'Version 1, approved']));
  expect(within(bar()).getByRole('status')).toHaveTextContent('Version 2 is a draft and not up for approval.');
  expect(from, 'a viewer who never looked compares with the approved version').toHaveValue('1');
  const v = { a: bare(2, v2), spec: v2 as ContractSpec };
  expect(approvalState(v, undefined, undefined, false, 'changes_requested').text).toBe('You asked for changes to version 2.');
  expect(approvalState(v, undefined, undefined, true, 'draft').text).toBe('Version 2 is a draft: the agent puts it up for approval once it passes its checks.');
  expect(approvalState(v, undefined, undefined, false, 'proposed').text).toBe('Version 2 is not up for approval. Send a message and the agent puts it up again.');
  expect(approvalState(v, undefined, 2, false, 'approved').done).toBe(true);
});

test('on a phone the contract and the chat are tabs, and the chat tab counts waiting questions', async () => {
  stubViewport(false);
  const { ws } = open([spec(1, v1)]);
  const tabs = screen.getByRole('tablist', { name: 'Show' });
  expect(within(tabs).getByRole('tab', { name: 'Contract' })).toHaveAttribute('aria-selected', 'true');
  ws.frame({ type: 'standing', questions: [{ id: 'q', question: 'Cursor or offset?', options: ['Cursor'], evidence: null, askedAt: 1 }], failures: [], approvals: [] });
  await userEvent.click(within(tabs).getByRole('tab', { name: 'Chat 1' }));
  expect(within(tabs).getByRole('tab', { name: 'Chat 1' })).toHaveAttribute('aria-selected', 'true');
});

test('on a phone a version counts as looked at only while the Contract tab shows it', async () => {
  stubViewport(false);
  const seen = () => localStorage.getItem('quack.contracts.seen.contracts:s1');
  routes[STATUS] = status([1, 'proposed']);
  const { ws } = open([spec(1, v1)]);
  await waitFor(() => expect(seen()).toBe('1'));
  routes[STATUS] = status([1, 'proposed'], [2, 'proposed']);
  const tabs = screen.getByRole('tablist', { name: 'Show' });
  await userEvent.click(within(tabs).getByRole('tab', { name: 'Chat' }));
  ws.frame({ type: 'artifacts', artifacts: [spec(1, v1), spec(2, v2)] });
  await waitFor(() => expect(within(screen.getByRole('combobox', { name: 'Compare from' })).getByRole('option', { name: 'Version 2, awaiting approval' })).toBeInTheDocument());
  expect(seen(), 'version 2 sealed while the chat tab was shown').toBe('1');
  await userEvent.click(within(tabs).getByRole('tab', { name: 'Contract' }));
  expect(seen()).toBe('2');
  expect(screen.getByRole('combobox', { name: 'Compare from' })).toHaveValue('1');
});
