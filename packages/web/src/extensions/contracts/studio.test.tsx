import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test } from 'vitest';
import { App } from '../../App.tsx';
import { SUPERSEDED } from '../../SessionView.tsx';
import type { Approval, SealedArtifact, Verdict } from '../../api.ts';
import { byVersion, entry, Fail, FakeSocket, fetchMock, live, routes, snapshot, stubViewport, useHarness } from '../../test-harness.ts';
import { diffUnits, units } from './diff.ts';
import { changeItems } from './review.ts';
import v1 from './fixtures/sleeper-schedule-v1.json';
import v2 from './fixtures/sleeper-schedule-v2.json';
import settings from './fixtures/sleeper-settings.json';
import { unnamedBreaks, type Break } from './Approval.tsx';
import { approvalState, type Standing } from './Studio.tsx';
import { declarations } from './typescript-model.ts';
import type { ContractSpec, Listed } from './spec.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /sessions/contracts%3As1/asks': { asks: [] } }));

const KEY = 'contracts%3As1';
const pass: Verdict = { passed: true, gatesFailed: [], score: 1, checks: [] };
const fail: Verdict = { passed: false, gatesFailed: ['justified.links_resolve'], score: 0.5, checks: [{ criterion: 'justified', id: 'links_resolve', label: "A choice's link doesn't resolve", weight: 'gate', met: 0, notes: [] }] };
const spec = (version: number, content: object, verdict: Verdict | null = pass): SealedArtifact => ({ kind: 'contract.spec', version, hash: `h${version}`, content: JSON.stringify(content), sealedAt: version, verdict, outcome: `v${(content as { version: number }).version}` });
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
const side = (name: 'from' | 'to') => screen.getByRole('button', { name: new RegExp(`^Compare ${name} version: `) });
// The versions a side's menu offers, read by opening it and closing it again.
async function offered(name: 'from' | 'to') {
  await userEvent.click(side(name));
  const items = within(screen.getByRole('menu', { name: `Compare ${name} version` })).getAllByRole('menuitem').map((i) => i.textContent);
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

// The status line: the first line says what is going on, the second whether anything waits on the person.
const line = () => within(bar()).getByRole('status');
const sheet = (name: string) => screen.getByRole('dialog', { name });
const changesList = (name = 'Changes against v1') => within(contract()).findByRole('region', { name });
// The one line per change, in order, as the list shows them.
const lines = (region: HTMLElement) => within(within(region).getAllByRole('list')[0]!).getAllByRole('button').map((b) => b.textContent);

test.each([
  ['a person', { kind: 'person', id: 'dana' }, 'Asked by dana · nothing waits on you'],
  ['the tailnet', { kind: 'tailnet' }, 'Asked by a reviewer · nothing waits on you'],
] as const)('a rejection by %s says who asked, with the first two lines of the reason, and offers no Approve', async (_n, by, sub) => {
  routes[STATUS] = status([1, 'approved'], [2, 'changes_requested']);
  routes[`GET /sessions/${KEY}/asks`] = { asks: [{ session: KEY, id: 'p/1', type: 'approval', state: 'resolved', createdAt: '2026-11-01T01:00:00.000Z', toolCall: { tool: 'propose_contract', args: { artifact: 2 } }, resolution: { by, choice: 'reject', reason: 'first\nsecond\nthird' } }] };
  open([spec(1, v1), approvedRec(1, 1), spec(2, v2)]);
  await waitFor(() => expect(line()).toHaveTextContent('Changes asked for v2'));
  await waitFor(() => expect(bar()).toHaveTextContent(sub));
  expect(within(bar()).getByText(/first\s+second/)).not.toHaveTextContent('third');
  expect(within(bar()).queryByRole('button', { name: 'Approve' })).toBeNull();
});

test('a REST or JSON Schema break names a field by its own name, and a short or generic one only beside its parent', () => {
  const runs = (at: string) => ({ type: 'object', properties: { runs: { type: 'array', items: { type: 'object', properties: { at: { type: at } } } } } });
  const doc = (at: string) => ({ ...v1, document: { paths: { '/runs': { get: { responses: { '200': { description: 'ok', content: { 'application/json': { schema: runs(at) } } } } } } } } });
  const breaking = (what: string) => {
    const two = { ...doc('integer'), compatibility: { breaking: [{ what, callers: ['page'] }], safe: [] } } as ContractSpec;
    return changeItems(diffUnits(units(doc('string') as ContractSpec), units(two)), two).map((i) => `${i.line} ${i.breaking}`);
  };
  expect(breaking('runs[].at is an integer now.')).toEqual(['GET /runs › 200 › runs › at: type changed true']);
  expect(breaking('Runs start at the top of the hour.'), '`at` alone, without its parent, names nothing').toEqual(['GET /runs › 200 › runs › at: type changed false']);
  expect(breaking('Each report now has its own page.'), 'no whole word').toEqual(['GET /runs › 200 › runs › at: type changed false']);
});

test('the status line says what is going on and whether anything waits on you, in every state', () => {
  const at = (over: Partial<Standing & { isApproved: boolean }>) => approvalState({ name: 'v2', running: false, isApproved: false, status: 'draft', approved: 'v1', ...over });
  expect(at({ status: undefined })).toEqual({ can: false, title: 'Reading where v2 stands…' });
  expect(at({ running: true })).toEqual({ can: false, title: 'The agent is drafting v2', sub: 'Nothing waits on you · v1 is approved' });
  expect(at({ running: true, verdict: null })).toEqual({ can: false, title: 'Checking v2', sub: 'Nothing waits on you yet · v1 is approved' });
  expect(at({ running: true, verdict: fail })).toEqual({ can: false, title: 'v2 failed its checks', sub: 'The agent is revising it · nothing waits on you · v1 is approved', detail: "Failed: A choice's link doesn't resolve" });
  expect(at({ verdict: { ...fail, limitReached: true } }).sub).toBe('Tell the agent what to change');
  expect(at({})).toEqual({ can: false, title: 'v2 is a draft', sub: 'Not up for approval · send a message and the agent goes on' });
  expect(at({ status: 'proposed', running: true })).toEqual({ can: false, title: 'v2 passed its checks', sub: 'The agent puts it up for approval when this step ends · nothing waits on you yet' });
  expect(at({ status: 'proposed' })).toEqual({ can: false, title: 'v2 is not up for approval', sub: 'Send a message and the agent puts it up again' });
  expect(at({ status: 'proposed', proposal: proposal(2, 2) as Approval })).toEqual({ can: true, title: 'v2 waits on you', sub: 'Checks passed' });
  expect(at({ status: 'changes_requested', running: true })).toEqual({ can: false, title: 'Changes asked for v2', sub: 'Asked by someone · the agent is revising it · nothing waits on you', detail: undefined });
  expect(at({ status: 'approved', isApproved: true })).toEqual({ can: false, done: true, title: 'v2 is approved', sub: 'Nothing in progress · nothing waits on you' });
  expect(at({ status: 'approved', isApproved: true, running: true }).sub).toBe('The agent is working · nothing waits on you');
});

test('until the status answers, nothing is compared and no version carries a status label', async () => {
  const { pane } = open([spec(1, v1), approvedRec(1, 1), spec(2, v2)]);
  await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(`apps/contracts/contracts/${KEY}`));
  expect(within(pane).queryByRole('region', { name: /^Changes/ })).toBeNull();
  expect(side('from')).toHaveAccessibleName('Compare from version: None: the whole contract');
  expect(await offered('from')).toEqual(['None: the whole contract', 'Version 2', 'Version 1']);
  expect(line()).toHaveTextContent('Reading where v2 stands…');
});

// Version 1 approved and version 2 up for approval, with the code checker's breaks on the proposal.
async function upForApproval(breaks: object[] = []) {
  routes[`PATCH /sessions/${KEY}/asks/p%2F1`] = { resolved: true };
  routes[STATUS] = status([1, 'approved'], [2, 'proposed']);
  const opened = open([spec(1, v1), approvedRec(1, 1), spec(2, v2)], [proposal(2, 2, breaks)]);
  await waitFor(() => expect(line()).toHaveTextContent('v2 waits on you'));
  return opened;
}

test('the decision bar leads with what waits on you, and Approve confirms in a sheet: each change, the breaks with their callers, the checks', async () => {
  const { pane } = await upForApproval([{ text: 'runs are now oldest first' }]);
  expect(within(bar()).getByRole('button', { name: 'Checks passed' })).toBeInTheDocument();
  expect(within(pane).getByRole('region', { name: 'Changes against v1' }), 'the changes lead the page').toBeInTheDocument();
  await userEvent.click(within(bar()).getByRole('button', { name: 'Approve' }));
  const s = sheet('Approve v2?');
  expect(s).toHaveTextContent('REST contract, measured against v1.');
  expect(within(s).getAllByRole('list')[0]!.textContent).toBe(['GET /apps/sleeper/schedule: summary changed', 'GET /apps/sleeper/schedule › 200 › runs: description changed', 'New GET /apps/sleeper/schedule › 200 › runs › status', 'New GET /apps/sleeper/schedule › 200 › runs › resultUrl'].join(''));
  expect(within(s).getByRole('heading', { name: 'What could break · 1' })).toBeInTheDocument();
  expect(s).toHaveTextContent('The Sleeper page reads the first run as "next".Callers: Sleeper page');
  expect(s).toHaveTextContent("The code checker found breaks in 1 route not named in the spec's breaks: runs are now oldest first");
  expect(within(s).getByText('Automatic checks passed')).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: /^Reject/ }), 'the pane decides it, not a transcript card').toBeNull();
  await userEvent.click(within(s).getByRole('button', { name: 'Not yet' }));
  expect(posts()).toEqual([]);
  await userEvent.click(within(bar()).getByRole('button', { name: 'Approve' }));
  await userEvent.click(within(sheet('Approve v2?')).getByRole('button', { name: 'Approve v2' }));
  expect(posts()).toEqual([[`sessions/${KEY}/asks/p%2F1`, { resolution: { choice: 'approve' } }]]);
});

test.each([
  ['ids given to unchanged text are no change', 'Follows the REST guide', false],
  ['a changed fact is one line', 'Follows the REST guide, always', true],
] as const)('the approve sheet reads id\'d facts against stored strings by their text: %s', async (_n, last, changed) => {
  const facts = { ...v2, intent: { ...v2.intent, guarantees: [{ id: 'read-only', text: 'Read-only' }, 'Covers a week with no runs', { id: 'guide.rest', text: last }] } };
  routes[`PATCH /sessions/${KEY}/asks/p%2F1`] = { resolved: true };
  routes[STATUS] = status([1, 'approved'], [2, 'proposed']);
  open([spec(1, v1), approvedRec(1, 1), spec(2, facts)], [proposal(2, 2)]);
  await waitFor(() => expect(line()).toHaveTextContent('v2 waits on you'));
  await userEvent.click(within(bar()).getByRole('button', { name: 'Approve' }));
  const s = sheet('Approve v2?');
  expect(within(within(s).getAllByRole('list')[0]!).getAllByRole('listitem').map((li) => li.textContent).includes('Guarantees changed')).toBe(changed);
  expect(s.textContent).not.toMatch(/"id"|\[object Object\]/);
});

test('asking for changes needs a reason, and sends it as the rejection the agent reads', async () => {
  await upForApproval();
  await userEvent.click(within(bar()).getByRole('button', { name: 'Ask for changes' }));
  const s = sheet('Ask for changes to v2');
  const send = within(s).getByRole('button', { name: 'Send to the agent' });
  expect(send).toBeDisabled();
  await userEvent.type(within(s).getByRole('textbox', { name: 'What should change?' }), 'Keep soonest first');
  await userEvent.click(send);
  expect(posts()).toEqual([[`sessions/${KEY}/asks/p%2F1`, { resolution: { choice: 'reject', reason: 'Keep soonest first' } }]]);
});

test("the checker's breaks show as wrapped blocks per place, a cut one marked, and none found says so", async () => {
  const long = `export "Judged": values core now produces no longer fit old consumers: ${'x'.repeat(300)}`;
  const found = [{ id: 'produced-breaks', text: long, operation: '', path: '/Judged' }, { id: 'provided-breaks', text: 'export "judged": values extensions already provide no longer fit: Type…', operation: '', path: '/judged' }];
  const { ws } = await upForApproval(found);
  await userEvent.click(within(bar()).getByRole('button', { name: 'Approve' }));
  const s = sheet('Approve v2?');
  expect(s).toHaveTextContent("The code checker found breaks in 2 routes not named in the spec's breaks: Judged, judged");
  await userEvent.click(within(s).getByText('Checker output (2)'));
  expect([...s.querySelectorAll('pre')].map((b) => b.textContent)).toEqual([long, found[1]!.text]);
  expect(within(s).getAllByText('(cut by the checker)')).toHaveLength(2);
  ws.frame({ type: 'standing', questions: [], failures: [], approvals: [proposal(2, 2, [])] });
  ws.frame({ type: 'artifacts', artifacts: [spec(1, v1), approvedRec(1, 1), spec(2, { ...v2, compatibility: { breaking: [], safe: [] } })] });
  expect(await within(sheet('Approve v2?')).findByText('The spec lists no breaks')).toBeInTheDocument();
  expect(within(sheet('Approve v2?')).getByText('The code checker found no break against the approved version.')).toBeInTheDocument();
});

// Shared with the judge's breaking_named gate (packages/contracts/test/breaks.test.ts): the page warns on exactly the breaks the gate fails.
const BREAK_NAMES = JSON.parse(readFileSync(join(process.cwd(), '../../test-fixtures/break-names.json'), 'utf8')) as Array<{ case: string; listed: Listed[]; found: Break[]; unnamed: string[] }>;
test.each(BREAK_NAMES.map((c) => [c.case, c] as const))("a checker break counts as listed only when the spec's breaks name it as a whole word: %s", (_n, c) => {
  expect(unnamedBreaks(c.listed, c.found)).toEqual(c.unnamed);
});

test('a request for an older seal than the newest says it is out of date and offers no approval', async () => {
  routes[STATUS] = status([1, 'approved'], [2, 'proposed'], [3, 'draft']);
  open([spec(1, v1), approvedRec(1, 1), spec(2, v2), spec(3, v2, null)], [proposal(2, 2)]);
  expect(await within(bar()).findByText('The approval request for v2 draft 1 is out of date: the agent has since written v2 draft 2.')).toBeInTheDocument();
  expect(line()).toHaveTextContent('Checking v2 draft 2');
  expect(bar()).toHaveTextContent('Nothing waits on you yet · v1 is approved');
  expect(within(bar()).queryByRole('button', { name: 'Approve' })).toBeNull();
  expect(within(bar()).queryByRole('button', { name: 'Ask for changes' })).toBeNull();
});

test('with two requests out of date, the line names the newer and counts them', async () => {
  routes[STATUS] = status([1, 'approved'], [2, 'proposed'], [3, 'proposed'], [4, 'draft']);
  open([spec(1, v1), approvedRec(1, 1), spec(2, v2), spec(3, v2), spec(4, v2, null)], [proposal(2, 2), { ...proposal(3, 2), id: 'p/2' }]);
  expect(await within(bar()).findByText('2 approval requests are out of date, the newest for v2 draft 2: the agent has since written v2 draft 3.')).toBeInTheDocument();
});

test('each sealed draft replaces the shown one live: approved and idle, then checking, then failed, each with its reason', async () => {
  routes[STATUS] = status([1, 'approved']);
  const { ws } = open([spec(1, v1), approvedRec(1, 1)]);
  await waitFor(() => expect(side('to')).toHaveAccessibleName('Compare to version: Version 1, approved'));
  expect(line()).toHaveTextContent('v1 is approved');
  expect(bar()).toHaveTextContent('Nothing in progress · nothing waits on you');
  expect(within(contract()).queryByRole('region', { name: /^Changes/ }), 'nothing is compared with an approved newest version').toBeNull();
  routes[STATUS] = status([1, 'approved'], [2, 'draft']);
  ws.frame({ type: 'artifacts', artifacts: [spec(1, v1), approvedRec(1, 1), spec(2, v2, null)] });
  expect(await within(bar()).findByText('Checking v2')).toBeInTheDocument();
  expect(await changesList()).toBeInTheDocument();
  expect(side('from')).toHaveAccessibleName('Compare from version: Version 1, approved');
  expect(within(bar()).queryByRole('button', { name: 'Approve' })).toBeNull();
  expect(contract()).toHaveTextContent('Runs from the start of the week');
  ws.frame({ type: 'artifacts', artifacts: [spec(1, v1), approvedRec(1, 1), spec(2, v2, fail)] });
  expect(line()).toHaveTextContent('v2 failed its checks');
  expect(bar()).toHaveTextContent("Failed: A choice's link doesn't resolve");
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
  await userEvent.click(within(sheet('Approve v2?')).getByRole('button', { name: 'Approve v2' }));
  expect(await within(sheet('Approve v2?')).findByRole('alert')).toHaveTextContent('already decided');
});

// A version as the catalog lists it: no verdict at all.
const bare = (version: number, content: object): SealedArtifact => { const { verdict: _, ...a } = spec(version, content); return a; };
const tsSpec = (version: number, document: string, over: object = {}) => ({ kind: 'typescript', title: 'Shapes', version, intent: { purpose: 'Shapes' }, choices: [], document, ...over });

// Version 1 approved, then version 2 put up for approval, neither carrying a verdict.
async function proposeTwo(one: object, two: object) {
  routes[STATUS] = status([1, 'approved']);
  const { ws, pane } = open([bare(1, one)]);
  await waitFor(() => expect(side('to')).toHaveAccessibleName('Compare to version: Version 1, approved'));
  routes[STATUS] = status([1, 'approved'], [2, 'proposed']);
  ws.frame({ type: 'artifacts', artifacts: [bare(1, one), bare(2, two)] });
  ws.frame({ type: 'standing', questions: [], failures: [], approvals: [proposal(2, 2)] });
  return pane;
}

// The fixture's break, written the way a REST break names the field it changes.
const v2Runs = { ...v2, compatibility: { ...v2.compatibility, breaking: [{ what: 'runs now start from the start of the week, oldest first.', callers: ['Sleeper page'] }] } };

test('with no verdicts, a REST contract lists its changes first and marks the operations and fields that changed, a break by its field name', async () => {
  const pane = await proposeTwo(v1, v2Runs);
  expect(lines(await changesList())).toEqual(['GET /apps/sleeper/schedule: summary changed', 'GET /apps/sleeper/schedule › 200 › runs: description changedBreaks a caller', 'New GET /apps/sleeper/schedule › 200 › runs › status', 'New GET /apps/sleeper/schedule › 200 › runs › resultUrl']);
  const runs = pane.querySelector('[data-ptr$="/schema/properties/runs"]') as HTMLElement;
  expect(within(runs).getByRole('img', { name: 'Breaks a caller' })).toBeInTheDocument();
  expect(runs).toHaveTextContent('Breaks: Sleeper page');
  expect(runs, 'a changed field shows its first changed sentence on each side').toHaveTextContent('v1Upcoming runs, soonest firstv2Runs from the start of the week, oldest first');
  expect(within(pane).getAllByRole('img', { name: 'New' }).length).toBe(2);
  expect(line()).toHaveTextContent('v2 waits on you');
  expect(within(bar()).getByRole('button', { name: 'Approve' })).toBeEnabled();
});

const shapes1 = tsSpec(1, 'export type A = { n: number };');
const shapes2 = tsSpec(2, '/** A thing with a name. More about it. */\nexport type A = { n: string; m?: B };\nexport type B = { x: number };', { compatibility: { breaking: [{ what: 'A.n is a string now.', callers: ['web/src/a.ts:12'] }], safe: [] } });

test('a TypeScript contract marks each change on the diagram and in the spec, a break by a diamond and its own words', async () => {
  const pane = await proposeTwo(shapes1, shapes2);
  expect(lines(await changesList())).toEqual(['A: description changed', 'A.n: type changedBreaks a caller', 'New field A.m', 'New type B']);
  const diagram = await within(pane).findByRole('region', { name: 'Class diagram' });
  expect(await within(diagram).findByRole('button', { name: 'A, 3 changes, 1 breaks a caller, go to its declaration' })).toBeInTheDocument();
  expect(within(diagram).getByRole('button', { name: 'Centre the diagram on B, 1 change' })).toBeInTheDocument();
  expect(within(diagram).getByText('breaks a caller'), 'the legend names both marks').toBeInTheDocument();
  const a = await within(pane).findByRole('article', { name: 'A' });
  const n = a.querySelector('[data-ptr="/A/n"]') as HTMLElement;
  expect(within(n).getByRole('img', { name: 'Breaks a caller' })).toBeInTheDocument();
  expect(n, 'a short value reads before and after on its own line').toHaveTextContent('v1numberv2string');
  expect(n).toHaveTextContent('Breaks: web/src/a.ts:12');
  expect(within(a.querySelector('[data-ptr="/A/m"]') as HTMLElement).getByRole('img', { name: 'New' })).toBeInTheDocument();
  const b = await within(pane).findByRole('article', { name: 'B' });
  expect(within(b).getAllByText('New'), 'the new type is tagged once').toHaveLength(1);
  expect(within(b).queryByRole('img', { name: 'New' }), 'its field rides on the type').toBeNull();
});

test('a first tap on a box selects it and says what it is; Show in spec finds and highlights it, and a second tap centres there', async () => {
  const pane = await proposeTwo(shapes1, shapes2);
  const diagram = await within(pane).findByRole('region', { name: 'Class diagram' });
  const b = await within(diagram).findByRole('button', { name: 'Centre the diagram on B, 1 change' });
  fireEvent.pointerDown(b, { pointerType: 'touch' });
  fireEvent.click(b);
  expect(within(diagram).getByRole('heading')).toHaveTextContent('around A');
  expect(diagram).toHaveTextContent('Btype');
  expect(diagram).toHaveTextContent('It has no description.');
  expect(diagram).toHaveTextContent('1 change: B');
  await userEvent.click(within(diagram).getByRole('button', { name: 'Show in spec' }));
  const card = within(pane).getByRole('article', { name: 'B' });
  expect(card).toHaveFocus();
  expect(card).toHaveClass('bg-link-bg');
  fireEvent.pointerDown(b, { pointerType: 'touch' });
  fireEvent.click(b);
  expect(within(diagram).getByRole('heading')).toHaveTextContent('around B');
});

test('pointing at a box shows its first sentence as a tooltip', async () => {
  const pane = await proposeTwo(shapes1, shapes2);
  const diagram = await within(pane).findByRole('region', { name: 'Class diagram' });
  const a = await within(diagram).findByRole('button', { name: /^A, 3 changes/ });
  fireEvent.pointerOver(a, { pointerType: 'mouse' });
  expect(screen.getByRole('tooltip')).toHaveTextContent(/^A thing with a name\.$/);
  expect(a).toHaveAccessibleDescription('A thing with a name.');
  fireEvent.pointerOut(a, { pointerType: 'mouse' });
  expect(screen.queryByRole('tooltip')).toBeNull();
});

test('Next change steps through every change in list order, centring the diagram and highlighting the spec together', async () => {
  const pane = await proposeTwo(shapes1, shapes2);
  await changesList();
  // The stepper's live line, which says where the reader is among the changes.
  const where = () => pane.querySelector('p[aria-live=polite]');
  expect(where()).toHaveTextContent('4 changes against v1');
  const diagram = await within(pane).findByRole('region', { name: 'Class diagram' });
  const seen: string[] = [];
  await userEvent.click(within(pane).getByRole('button', { name: 'First change' }));
  for (let i = 0; i < 4; i++) {
    if (i) await userEvent.click(within(pane).getByRole('button', { name: 'Next change' }));
    expect(where()).toHaveTextContent(`Change ${i + 1} of 4 against v1`);
    seen.push(`${(document.activeElement as HTMLElement).dataset.ptr} ${within(diagram).getByRole('heading').textContent}`);
  }
  expect(seen).toEqual(['/A Class diagram around A', '/A/n Class diagram around A', '/A/m Class diagram around A', '/B Class diagram around B']);
  await userEvent.click(within(pane).getByRole('button', { name: 'Back to the top' }));
  expect(where()).toHaveTextContent('4 changes against v1');
  await userEvent.click(within(await changesList()).getByRole('button', { name: 'New field A.m' }));
  expect(document.activeElement).toHaveAttribute('data-ptr', '/A/m');
  expect(where()).toHaveTextContent('Change 3 of 4 against v1');
});

test('a JSON Schema contract lists its changes first, marks the fields, and Next change goes to each field', async () => {
  const doc = settings.document as { properties: Record<string, object> };
  const two = { ...settings, version: 2, document: { ...doc, properties: { ...doc.properties, mode: { ...doc.properties.mode, description: 'Which job runs.' }, timezone: { type: 'string' } } } };
  const pane = await proposeTwo(settings, two);
  expect(lines(await changesList())).toEqual(['mode: description changed', 'New field timezone']);
  const fields = within(pane).getByRole('region', { name: 'Fields' });
  expect(within(fields.querySelector('[data-ptr="/properties/mode"]') as HTMLElement).getByRole('img', { name: 'Changed' })).toBeInTheDocument();
  expect(within(fields.querySelector('[data-ptr="/properties/timezone"]') as HTMLElement).getByRole('img', { name: 'New' })).toBeInTheDocument();
  await userEvent.click(within(pane).getByRole('button', { name: 'First change' }));
  await userEvent.click(within(pane).getByRole('button', { name: 'Next change' }));
  expect(document.activeElement).toHaveAttribute('data-ptr', '/properties/timezone');
});

test("each version is labelled by where it stands with the person, and the status line reads the newest one's status", async () => {
  routes[STATUS] = status([1, 'approved'], [2, 'changes_requested'], [3, 'proposed'], [4, 'draft']);
  open([bare(1, v1), approvedRec(1, 1), bare(2, v2), bare(3, v2), bare(4, v2)]);
  await waitFor(() => expect(side('to')).toHaveAccessibleName('Compare to version: Version 2, draft 3, draft'));
  expect(side('to').textContent).toBe('v2 draft 3');
  expect(await offered('to')).toEqual(['Version 2, draft 3#4draft, draft', 'Version 2, draft 2#3awaiting, awaiting approval', 'Version 2, draft 1#2changes asked, changes requested', 'Version 1approved, approved']);
  expect(line()).toHaveTextContent('v2 draft 3 is a draft');
  expect(side('from')).toHaveAccessibleName('Compare from version: Version 1, approved');
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

test('the chat names each seal by the contract version a person knows, and its draft when several seals share one', async () => {
  open([spec(21, { ...v2, version: 15 }), spec(22, { ...v2, version: 15 }, null), spec(23, { ...v2, version: 16 })]);
  const lines = await waitFor(() => { const l = [...document.querySelectorAll<HTMLElement>('[data-ledger]')]; expect(l).toHaveLength(3); return l; });
  expect(lines.map((l) => l.textContent?.match(/Version \d+( · draft \d)?/)?.[0])).toEqual(['Version 15 · draft 1', 'Version 15 · draft 2', 'Version 16']);
  expect(within(lines[1]!).getByRole('button', { name: 'Open Spec version 15, draft 2' })).toBeInTheDocument();
});

// What the session sends now: only the newest version of a kind carries its content.
const thin = (artifacts: SealedArtifact[]) => artifacts.map((a, i) => (a.kind === 'contract.spec' && artifacts.findLastIndex((x) => x.kind === 'contract.spec') !== i ? (({ content: _, ...row }) => row)(a) : a));
const CONTENT = `sessions/${KEY}/artifacts`;
const contentReads = () => fetchMock.mock.calls.map(([u]) => String(u)).filter((u) => u.startsWith(CONTENT));
const olderRows = () => byVersion([{ session: 'contracts:s1', kind: 'contract.spec', version: 1, hash: 'h1', content: JSON.stringify(v1), sealedAt: '2026-01-01T00:00:00Z' }]);

test('an approved idle contract opens without reading any older version', async () => {
  routes[STATUS] = status([1, 'changes_requested'], [2, 'approved']);
  routes[`GET /${CONTENT}`] = olderRows();
  open(thin([spec(1, v1), approvedRec(2, 2), spec(2, v2)]));
  await waitFor(() => expect(line()).toHaveTextContent('v2 is approved'));
  // Opening the menu lists the older version by its outcome-derived name, still without reading it.
  expect(await offered('from')).toEqual(['None: the whole contract', 'Version 2approved, approved', 'Version 1changes asked, changes requested']);
  expect(contentReads()).toEqual([]);
});

test('a revision waiting on approval reads exactly the approved baseline, once, and compares against it', async () => {
  routes[STATUS] = status([1, 'approved'], [2, 'proposed']);
  routes[`GET /${CONTENT}`] = olderRows();
  const { pane } = open(thin([spec(1, v1), approvedRec(1, 1), spec(2, v2)]), [proposal(2, 2)]);
  await waitFor(() => expect(line()).toHaveTextContent('v2 waits on you'));
  expect(await within(pane).findByRole('region', { name: 'Changes against v1' })).toBeInTheDocument();
  const reads = contentReads().map((u) => decodeURIComponent(u));
  expect(reads).toHaveLength(1);
  expect(reads[0]).toMatch(/version:1/);
});

test('a baseline that cannot be loaded says so and retries', async () => {
  routes[STATUS] = status([1, 'approved'], [2, 'proposed']);
  routes[`GET /${CONTENT}`] = new Fail(500, { title: 'Server Error', detail: 'down' });
  const { pane } = open(thin([spec(1, v1), approvedRec(1, 1), spec(2, v2)]), [proposal(2, 2)]);
  expect(await within(pane).findByRole('alert')).toHaveTextContent('v1 could not be loaded');
  routes[`GET /${CONTENT}`] = olderRows();
  await userEvent.click(within(pane).getByRole('button', { name: 'Retry' }));
  expect(await within(pane).findByRole('region', { name: 'Changes against v1' })).toBeInTheDocument();
  expect(within(pane).queryByRole('alert')).toBeNull();
});
