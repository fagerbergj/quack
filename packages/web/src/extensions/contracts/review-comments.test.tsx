import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test, vi } from 'vitest';
import { App } from '../../App.tsx';
import type { SealedArtifact } from '../../api.ts';
import { Fail, fetchMock, FakeSocket, live, routes, snapshot, useHarness } from '../../test-harness.ts';
import type { Comment } from './comments.ts';
import { readReason, reasonText } from './comments.ts';
import v1 from './fixtures/sleeper-schedule-v1.json';
import v2 from './fixtures/sleeper-schedule-v2.json';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /sessions/contracts%3As1/asks': { asks: [] } }));

const KEY = 'contracts%3As1';
const APP = `/apps/contracts/contracts/${KEY}`;
const ASK = `PATCH /sessions/${KEY}/asks/p%2F1`;
const seal = (version: number, content: object): SealedArtifact => ({ kind: 'contract.spec', version, hash: `h${version}`, content: JSON.stringify(content), sealedAt: version, verdict: { passed: true, gatesFailed: [], score: 1, checks: [] }, outcome: `v${version}` });
const approved: SealedArtifact = { kind: 'contract.approved', version: 1, hash: 'r', content: JSON.stringify({ version: 1, artifact: 1, note: 'Approved version 1', approval: 'old' }), sealedAt: 10, verdict: null };
const proposal = { id: 'p/1', tool: 'propose_contract', args: { summary: 'Approve v2', version: 2, artifact: 2, hash: 'h2', findings: [], breaks: [] }, requestedAt: 20 };
const ROUTE = '/paths/~1apps~1sleeper~1schedule/get';
const ada = { kind: 'person', id: 'ada' } as const;
const draft = (id: string, over: Partial<Comment> = {}): Comment => ({ id, artifact: 2, version: 2, anchor: { on: 'document', ptr: ROUTE }, quote: '{"summary":"…"}', body: `Body of ${id}`, author: ada, state: 'draft', createdAt: '2026-11-17T15:00:00Z', replies: [], ...over });
const REVIEW = { id: 'rev_1', artifact: 2, askId: 'p/1', comments: ['cmt_1'], by: ada, preparedAt: '2026-11-17T15:01:00Z', state: 'prepared', reason: 'Note.\n\n```quack-review\n{"review":"rev_1","artifact":2,"comments":[{"id":"cmt_1","anchor":{"on":"document","ptr":"/x"},"body":"b"}]}\n```' };

// The pointer kind decides the Comment action's form: a hover icon with a mouse, a row menu on a touch screen.
const pointer = (coarse: boolean) => vi.stubGlobal('matchMedia', (q: string) => ({ matches: q.includes('coarse') ? coarse : !q.includes('1440'), addEventListener() {}, removeEventListener() {} }));

function open(status: string, comments: Comment[] = [], approvals: object[] = [proposal]) {
  routes[`GET ${APP}`] = { key: 'contracts:s1', versions: [{ artifact: 1, version: 1, status: 'approved' }, { artifact: 2, version: 2, status }] };
  routes[`GET ${APP}/comments`] = { comments, caller: ada };
  live[KEY] = { artifacts: [seal(1, v1), approved, seal(2, v2)], approvals };
  location.hash = `#/s/${KEY}`;
  render(<App />);
  FakeSocket.all[0]!.open();
  FakeSocket.all[0]!.frame(snapshot());
  return screen.queryByRole('region', { name: 'Contract' }) ?? screen.getByRole('tabpanel', { name: /^Contract/ });
}
const bar = () => screen.getByRole('region', { name: 'Approval' });
const sent = (method: string) => fetchMock.mock.calls.filter(([u, i]) => i?.method === method && String(u).startsWith(APP.slice(1)) || (method === 'PATCH' && String(u).includes('/asks/'))).map(([u, i]) => [String(u), i?.body && JSON.parse(String(i.body))]);

test('with a mouse, a route, a field and a change row offer Comment on hover or focus, and the sheet adds a draft the bar then counts', async () => {
  pointer(false);
  routes[`POST ${APP}/comments`] = draft('cmt_1');
  const pane = open('proposed');
  await waitFor(() => expect(bar()).toHaveTextContent('v2 waits on you'));
  const route = await within(pane).findByRole('button', { name: 'Comment on GET /apps/sleeper/schedule' });
  expect(route.className, 'hidden until its row is pointed at or focused').toMatch(/opacity-0 group-hover\/c:opacity-100 group-focus-within\/c:opacity-100/);
  expect(within(pane).getByRole('button', { name: 'Comment on GET /apps/sleeper/schedule › responses › 200 › week' })).toBeInTheDocument();
  expect(within(pane).getByRole('button', { name: 'Comment on run-status' })).toBeInTheDocument();
  await userEvent.click(route);
  const sheet = screen.getByRole('dialog', { name: 'Comment' });
  expect(sheet).toHaveTextContent('SpecGET /apps/sleeper/schedule');
  expect(within(sheet).getByRole('button', { name: 'Add to review' })).toBeDisabled();
  await userEvent.type(within(sheet).getByRole('textbox', { name: 'Your comment' }), 'Say what week means.');
  routes[`GET ${APP}/comments`] = { comments: [draft('cmt_1')], caller: ada };
  await userEvent.click(within(sheet).getByRole('button', { name: 'Add to review' }));
  expect(sent('POST')).toEqual([[`${APP.slice(1)}/comments`, { artifact: 2, anchor: { on: 'document', ptr: ROUTE }, body: 'Say what week means.' }]]);
  await waitFor(() => expect(bar()).toHaveTextContent('1 draft comment'));
  expect(within(bar()).getByRole('button', { name: 'Review · 1' })).toBeInTheDocument();
  expect(within(pane).getByRole('button', { name: '1 comment on GET /apps/sleeper/schedule' }), 'the marker on its element').toBeInTheDocument();
});

test('on a touch screen the action is in a row menu, for spec rows, decisions and guarantees alike', async () => {
  pointer(true);
  const pane = open('proposed');
  await waitFor(() => expect(bar()).toHaveTextContent('v2 waits on you'));
  expect(within(pane).queryByRole('button', { name: /^Comment on/ })).toBeNull();
  await userEvent.click(await within(pane).findByRole('button', { name: 'More for GET /apps/sleeper/schedule' }));
  await userEvent.click(screen.getByRole('menuitem', { name: 'Comment' }));
  expect(screen.getByRole('dialog', { name: 'Comment' })).toHaveTextContent('GET /apps/sleeper/schedule');
  await userEvent.click(within(screen.getByRole('dialog', { name: 'Comment' })).getByRole('button', { name: 'Cancel' }));
  expect(within(pane).getByRole('button', { name: 'More for GET /apps/sleeper/schedule' }), 'focus returns to the row menu').toHaveFocus();
  await userEvent.click(within(pane).getByRole('button', { name: /^Guarantees/ }));
  await userEvent.click(within(screen.getByRole('dialog', { name: 'Guarantees' })).getByRole('button', { name: 'More for Read-only' }));
  await userEvent.click(screen.getByRole('menuitem', { name: 'Comment' }));
  expect(screen.getByRole('dialog', { name: 'Comment' })).toHaveTextContent('GuaranteeRead-only');
  await userEvent.keyboard('{Escape}{Escape}');
  await userEvent.click(within(pane).getByRole('button', { name: /^Decisions/ }));
  expect(within(screen.getByRole('dialog', { name: 'Decisions' })).getByRole('button', { name: 'More for read-only' })).toBeInTheDocument();
});

test('a version that waits on nobody takes no comments, but a sent comment still shows its marker and thread', async () => {
  pointer(false);
  const c = draft('cmt_1', { state: 'addressed', sentAt: '2026-11-17T15:02:00Z', review: 'rev_1', replies: [{ text: 'Now says what week is.', at: '2026-11-17T15:05:00Z' }], addressedIn: 2, outdated: { mark: 'changed', in: 2 }, artifact: 1, version: 1 });
  routes[`POST ${APP}/comments/cmt_1:resolve`] = { ...c, state: 'resolved', resolvedAt: '2026-11-17T16:00:00Z' };
  const pane = open('approved', [c], []);
  const marker = await within(pane).findByRole('button', { name: '1 comment on GET /apps/sleeper/schedule' });
  expect(within(pane).queryByRole('button', { name: /^Comment on/ })).toBeNull();
  await userEvent.click(marker);
  const thread = screen.getByRole('dialog', { name: 'Comments on GET /apps/sleeper/schedule' });
  expect(thread).toHaveTextContent('Addressed in v2');
  expect(thread).toHaveTextContent('Outdated: changed in v2');
  expect(thread).toHaveTextContent('On v1: {"summary":"…"}');
  expect(thread).toHaveTextContent('You ·');
  expect(thread).toHaveTextContent(/Agent · .*Now says what week is\./);
  await userEvent.click(within(thread).getByRole('button', { name: 'Resolve' }));
  expect(sent('POST')).toEqual([[`${APP.slice(1)}/comments/cmt_1:resolve`, undefined]]);
});

async function reviewOpen(drafts: Comment[]) {
  pointer(false);
  const pane = open('proposed', drafts);
  await waitFor(() => expect(bar()).toHaveTextContent(`${drafts.length} draft`));
  await userEvent.click(within(bar()).getByRole('button', { name: `Review · ${drafts.length}` }));
  return { pane, sheet: screen.getByRole('dialog', { name: 'Your review of v2' }) };
}

test('the review sheet lists the drafts with edit and remove, then sends: it prepares the review and rejects with its reason, verbatim', async () => {
  routes[`PATCH ${APP}/comments/cmt_2`] = draft('cmt_2', { body: 'Edited.' });
  routes[`DELETE ${APP}/comments/cmt_3`] = {};
  routes[`POST ${APP}/reviews`] = REVIEW;
  routes[ASK] = { resolved: true };
  const { sheet } = await reviewOpen([draft('cmt_1'), draft('cmt_2', { anchor: { on: 'choice', id: 'read-only' } }), draft('cmt_3', { anchor: { on: 'guarantee', text: 'Read-only' } })]);
  expect(within(sheet).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['SpecGET /apps/sleeper/scheduleEditRemoveBody of cmt_1', 'Decisionread-onlyEditRemoveBody of cmt_2', 'GuaranteeRead-onlyEditRemoveBody of cmt_3']);
  await userEvent.click(within(sheet).getByRole('button', { name: 'Edit comment on read-only' }));
  const box = within(sheet).getByRole('textbox', { name: 'Comment on read-only' });
  await userEvent.clear(box);
  await userEvent.type(box, 'Edited.');
  await userEvent.click(within(sheet).getByRole('button', { name: 'Save' }));
  await userEvent.click(within(sheet).getByRole('button', { name: 'Remove comment on Read-only' }));
  await userEvent.type(within(sheet).getByRole('textbox', { name: 'Overall note (optional)' }), 'Note.');
  await userEvent.click(within(sheet).getByRole('button', { name: 'Send 3 comments' }));
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Your review of v2' })).toBeNull());
  expect(sent('PATCH')).toEqual([[`${APP.slice(1)}/comments/cmt_2`, { body: 'Edited.' }], [`sessions/${KEY}/asks/p%2F1`, { resolution: { choice: 'reject', reason: REVIEW.reason } }]]);
  expect(sent('POST')).toEqual([[`${APP.slice(1)}/reviews`, { comments: ['cmt_1', 'cmt_2', 'cmt_3'], note: 'Note.' }]]);
  expect(sent('DELETE')).toEqual([[`${APP.slice(1)}/comments/cmt_3`, undefined]]);
  expect(screen.queryByRole('textbox', { name: /reason/i }), 'no way to edit the reason').toBeNull();
});

test.each([
  ['a 400 from prepare', new Fail(400, { detail: "the review's reason is over 60000 bytes as JSON; send fewer or shorter comments" }), "the review's reason is over 60000 bytes as JSON"],
  ['a 409 from prepare', new Fail(409, { detail: 'comment cmt_1 is held by prepared review rev_0 until that review is sent or released' }), 'held by prepared review rev_0'],
])('%s is said in the sheet, and nothing is rejected', async (_n, refusal, says) => {
  routes[`POST ${APP}/reviews`] = refusal;
  const { sheet } = await reviewOpen([draft('cmt_1')]);
  await userEvent.click(within(sheet).getByRole('button', { name: 'Send 1 comment' }));
  expect(await within(sheet).findByRole('alert')).toHaveTextContent(says);
  expect(sent('PATCH')).toEqual([]);
});

test('a rejection that fails leaves the review prepared: Send again reads its reason back and resends it, Release frees the drafts', async () => {
  routes[`POST ${APP}/reviews`] = REVIEW;
  routes[ASK] = new Fail(502, { detail: 'the session did not answer' });
  const { sheet } = await reviewOpen([draft('cmt_1')]);
  await userEvent.click(within(sheet).getByRole('button', { name: 'Send 1 comment' }));
  expect(await within(sheet).findByRole('status')).toHaveTextContent('Your review is ready but was not sent: the session did not answer.');
  routes[`GET ${APP}/reviews/rev_1`] = REVIEW;
  routes[ASK] = { resolved: true };
  await userEvent.click(within(sheet).getByRole('button', { name: 'Send again' }));
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Your review of v2' })).toBeNull());
  expect(sent('PATCH').map(([, b]) => b)).toEqual([{ resolution: { choice: 'reject', reason: REVIEW.reason } }, { resolution: { choice: 'reject', reason: REVIEW.reason } }]);
});

test('a draft a failed send left held offers Release, which frees it', async () => {
  routes[`DELETE ${APP}/reviews/rev_1`] = {};
  const { sheet } = await reviewOpen([draft('cmt_1', { review: 'rev_1' })]);
  expect(bar()).toHaveTextContent('1 draft comment, ready but not sent');
  expect(within(sheet).getByRole('status')).toHaveTextContent('Your review is ready but was not sent. Send it again, or release it to edit its comments.');
  expect(within(sheet).queryByRole('button', { name: /^Edit/ })).toBeNull();
  await userEvent.click(within(sheet).getByRole('button', { name: 'Release' }));
  expect(await within(sheet).findByRole('alert')).toHaveTextContent('Your review was released; its comments are drafts again.');
  expect(sent('DELETE')).toEqual([[`${APP.slice(1)}/reviews/rev_1`, undefined]]);
});

test('a 409 because the ask was withdrawn says the version was replaced, and the comments are drafts again', async () => {
  routes[`POST ${APP}/reviews`] = REVIEW;
  routes[ASK] = new Fail(409, { detail: 'the request was withdrawn: a newer version replaced it' });
  const { sheet } = await reviewOpen([draft('cmt_1')]);
  await userEvent.click(within(sheet).getByRole('button', { name: 'Send 1 comment' }));
  expect(await within(sheet).findByRole('alert')).toHaveTextContent('v2 was replaced before your review was sent, so it was not sent; your comments are drafts again. the request was withdrawn: a newer version replaced it');
});

test('a rejection reason carrying a review reads as its note and how many comments, never its JSON', async () => {
  expect(readReason(REVIEW.reason)).toEqual({ note: 'Note.', comments: 1 });
  expect(reasonText('```quack-review\n{"comments":[{"id":"a"},{"id":"b"}]}\n```')).toBe('2 comments');
  expect(reasonText('Plain words.')).toBe('Plain words.');
  routes[`GET /sessions/${KEY}/asks`] = { asks: [{ session: KEY, id: 'p/1', type: 'approval', state: 'resolved', createdAt: '2026-11-01T01:00:00.000Z', toolCall: { tool: 'propose_contract', args: { artifact: 2 } }, resolution: { by: ada, choice: 'reject', reason: REVIEW.reason } }] };
  open('changes_requested', [], []);
  await waitFor(() => expect(bar()).toHaveTextContent('Changes asked for v2'));
  expect(bar()).toHaveTextContent('Note. 1 comment');
  expect(bar()).not.toHaveTextContent('quack-review');
  const { contracts } = await import('./index.ts');
  const { container } = render(<>{contracts.card!({ kind: 'contract.changes', version: 1, hash: 'h', content: JSON.stringify({ version: 2, note: REVIEW.reason }), sealedAt: 1, verdict: null })}</>);
  expect(container).toHaveTextContent('Changes asked for v2Note.1 review comment');
});

test('the diagram strip of the centred box offers Comment, and a TypeScript member row resolves to its pointer', async () => {
  pointer(false);
  const ts = (version: number, document: string) => ({ kind: 'typescript', title: 'Shapes', version, intent: { purpose: 'Shapes' }, choices: [], document });
  routes[`GET ${APP}`] = { key: 'contracts:s1', versions: [{ artifact: 1, version: 1, status: 'approved' }, { artifact: 2, version: 2, status: 'proposed' }] };
  routes[`GET ${APP}/comments`] = { comments: [draft('cmt_9', { anchor: { on: 'document', ptr: '/A/n' }, state: 'sent' })], caller: ada };
  live[KEY] = { artifacts: [seal(1, ts(1, 'export type A = { n: number };')), approved, seal(2, ts(2, '/** A thing. */\nexport type A = { n: string; m?: B };\nexport type B = { x: number } | { y: string };'))], approvals: [proposal] };
  location.hash = `#/s/${KEY}`;
  render(<App />);
  FakeSocket.all[0]!.open();
  FakeSocket.all[0]!.frame(snapshot());
  const diagram = await screen.findByRole('region', { name: 'Class diagram' });
  await waitFor(() => expect(within(diagram).getByRole('button', { name: 'Comment' })).toBeInTheDocument());
  await userEvent.click(within(diagram).getByRole('button', { name: 'Comment' }));
  expect(screen.getByRole('dialog', { name: 'Comment' })).toHaveTextContent('DiagramAA thing.');
  await userEvent.keyboard('{Escape}');
  const a = screen.getByRole('article', { name: 'A' });
  expect(within(a).getByRole('button', { name: 'Comment on A.n' })).toBeInTheDocument();
  expect(within(a).getByRole('button', { name: '1 comment on A.n' }), 'the row marker').toBeInTheDocument();
  expect(within(diagram).getByRole('button', { name: /^A, .*1 comment, go to its declaration$/ }), 'the box counts what is under it').toBeInTheDocument();
  expect(within(screen.getByRole('article', { name: 'B' })).queryByRole('button', { name: /^Comment on B\.0/ }), 'a union variant does not resolve').toBeNull();
});

// v2 was replaced by v3, now up for approval, while a draft of the caller's still sits on v2.
async function stranded() {
  pointer(false);
  routes[`GET ${APP}`] = { key: 'contracts:s1', versions: [{ artifact: 1, version: 1, status: 'approved' }, { artifact: 2, version: 2, status: 'draft' }, { artifact: 3, version: 3, status: 'proposed' }] };
  routes[`GET ${APP}/comments`] = { comments: [draft('cmt_old')], caller: ada };
  live[KEY] = { artifacts: [seal(1, v1), approved, seal(2, v2), seal(3, { ...v2, version: 3 })], approvals: [{ ...proposal, args: { ...proposal.args, version: 3, artifact: 3 } }] };
  location.hash = `#/s/${KEY}`;
  render(<App />);
  FakeSocket.all[0]!.open();
  FakeSocket.all[0]!.frame(snapshot());
  await waitFor(() => expect(bar()).toHaveTextContent('v3 waits on you'));
  expect(within(bar()).getByRole('button', { name: 'Ask for changes' }), 'a stranded draft is not one the review can send').toBeInTheDocument();
  await userEvent.click(await within(bar()).findByRole('button', { name: '1 draft is on a replaced version' }));
  const sheet = screen.getByRole('dialog', { name: "Drafts that can't be sent" });
  expect(sheet).toHaveTextContent('Draft on v2 · v2 was replaced, so this can\'t be sent');
  expect(within(sheet).getByText('Body of cmt_old').tagName, 'its text stays readable and selectable').toBe('P');
  return sheet;
}

test('a draft left on a replaced version is not stranded: the bar says so, and Remove deletes it', async () => {
  routes[`DELETE ${APP}/comments/cmt_old`] = {};
  const sheet = await stranded();
  await userEvent.click(within(sheet).getByRole('button', { name: 'Remove' }));
  await waitFor(() => expect(sent('DELETE')).toEqual([[`${APP.slice(1)}/comments/cmt_old`, undefined]]));
});

test('Move puts the draft on the version up for approval by the same anchor and body, then removes the old one', async () => {
  routes[`POST ${APP}/comments`] = draft('cmt_new', { artifact: 3, version: 3 });
  routes[`DELETE ${APP}/comments/cmt_old`] = {};
  const sheet = await stranded();
  await userEvent.click(within(sheet).getByRole('button', { name: 'Move to v3' }));
  await waitFor(() => expect(sent('DELETE')).toEqual([[`${APP.slice(1)}/comments/cmt_old`, undefined]]));
  expect(sent('POST')).toEqual([[`${APP.slice(1)}/comments`, { artifact: 3, anchor: { on: 'document', ptr: ROUTE }, body: 'Body of cmt_old' }]]);
});

test('a Move whose anchor is gone from the new version keeps the old draft and says so', async () => {
  routes[`POST ${APP}/comments`] = new Fail(400, { detail: 'the anchor does not resolve in artifact 3' });
  const sheet = await stranded();
  await userEvent.click(within(sheet).getByRole('button', { name: 'Move to v3' }));
  expect(await within(sheet).findByRole('alert')).toHaveTextContent('What this draft is about is not in v3, so it stays on v2. Remove it, or copy its text into a new comment.');
  expect(sent('DELETE')).toEqual([]);
});
