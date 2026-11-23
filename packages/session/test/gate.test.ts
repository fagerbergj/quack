import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { obj, str } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { Approvals, gated, resolveApproval, withdrawApprovals, type Approval } from '@quack/sdk';
import { childPort, createSessionServer, currentEvents, openSession, PRINCIPAL_HEADER, principalHeader } from '../src/index.ts';
import { nextFrame } from './ws-helpers.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

const posted: string[] = [];
const post = gated(defineTool({
  name: 'post_comment',
  description: 'post_comment',
  parameters: obj({ body: str() }, { open: true }),
  execute: async (args) => {
    posted.push(args.body);
    return { output: [{ type: 'text', text: 'posted' }] };
  },
}));
// What the tool checks before its gate and adds to the gate's arguments; a test changes them between boots, as a deploy changes code.
const code: { refuse: string; suffix: string; before?: () => Promise<void> } = { refuse: '', suffix: '' };
const extension = defineExtension({
  name: 'test-github',
  tools: [defineTool({
    name: 'post_comment',
    description: 'Post a comment to the pull request',
    parameters: obj({ body: str() }, { open: true }),
    replay: 'safe',
    execute: async (args, api, ctx) => (await code.before?.(), code.refuse ? { output: [{ type: 'text', text: code.refuse }], isError: true } : post.execute({ body: `${args.body}${code.suffix}` }, api, ctx)),
  })],
});

function agentWith(...responses: Parameters<ReturnType<typeof fauxProvider>['setResponses']>[0]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  return { models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [extension] } };
}

const callPost = () => fauxAssistantMessage(fauxToolCall('post_comment', { body: 'LGTM' }), { stopReason: 'toolUse' });

const TOKEN = 'launch-token';
const BEARER = { authorization: `Bearer ${TOKEN}` };

// Serves with a launch token, as every launch does; `json` presents it unless a test passes its own headers.
async function serve(session: Awaited<ReturnType<typeof openSession>>) {
  const server = createSessionServer(session, { token: TOKEN }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const json = (path: string, body?: unknown, headers: Record<string, string> = { ...BEARER, [PRINCIPAL_HEADER]: principalHeader({ kind: 'person', name: 'jf' }) }) =>
    fetch(base + path, body === undefined ? { headers } : { method: 'POST', body: JSON.stringify(body), headers }).then(async (r) => ({ status: r.status, body: await r.json() }));
  return { json, base, stop: () => new Promise((r) => server.close(r)) };
}

const ctx = BACKGROUND_CONTEXT;
type Open = Awaited<ReturnType<typeof openSession>>;
const rowOf = async (s: Open, id: string) => (await s.harness.snapshot(Approvals, s.root.id, ctx))!.items[id]!;

// Waits for the first open approval not in `seen`; touching the document first lets it be watched before anything has asked.
async function firstAsk(s: Open, seen: string[] = []): Promise<Approval & { id: string }> {
  await s.root.commit(async (tx) => { await tx.doc(Approvals, s.root.id); }, ctx);
  const watch = (await s.harness.watchDoc(Approvals, s.root.id, ctx))!;
  return new Promise<Approval & { id: string }>((resolve) => {
    const find = () => {
      const hit = Object.entries(watch.value?.items ?? {}).find(([id, a]) => !a.verdict && !a.withdrawn && !seen.includes(id));
      return hit ? { id: hit[0], ...hit[1] } : undefined;
    };
    if (find()) return resolve(find()!);
    watch.start(async () => { if (find()) resolve(find()!); });
  }).finally(() => watch.stop());
}

test('a gated call waits across a restart, then runs once when approved', async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);

  const first = await openSession({ dir, ...agentWith(callPost()) });
  await first.submit('Approve this PR', 'run-1');
  const pending = await firstAsk(first);
  assert.equal(pending.tool, 'post_comment');
  assert.deepEqual(pending.args, { body: 'LGTM' });
  assert.deepEqual(posted, []);
  await first.close();

  const second = await openSession({ dir, ...agentWith(fauxAssistantMessage('Posted the review.')) });
  const http = await serve(second);
  const kept = (await http.json('/approvals')).body.approvals;
  assert.deepEqual(kept.map((a: { id: string }) => a.id), [pending.id], 'the rerun reached the gate with the same arguments, so it keeps its approval');
  assert.equal((await second.harness.snapshot(Approvals, second.root.id, BACKGROUND_CONTEXT))!.items[pending.id]!.withdrawn, undefined);

  const bad = await http.json(`/approvals/${pending.id}`, { decision: 'maybe', by: 'jf' });
  assert.equal(bad.status, 400);
  const ok = await http.json(`/approvals/${pending.id}`, { decision: 'approve', by: 'jf' });
  assert.equal(ok.status, 200);
  const again = await http.json(`/approvals/${pending.id}`, { decision: 'approve', by: 'jf' });
  assert.equal(again.status, 409);

  const done = await second.submit('Approve this PR', 'run-1', true);
  assert.equal(done.status, 'done');
  assert.equal(done.status === 'done' && done.answer, 'Posted the review.');
  assert.deepEqual(posted, ['LGTM']);
  await http.stop();
  await second.close();
});

for (const [label, who, decider] of [['the distributor names a person as', { kind: 'person', name: 'jason' }, 'jason'], ['the distributor names the tailnet as', { kind: 'tailnet' }, 'tailnet']] as const) {
test(`a rejection reaches the model and the tool never runs; ${label} the decider, never a body by`, async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  let sawRejection = '';
  const session = await openSession({
    dir,
    ...agentWith(callPost(), (context) => {
      sawRejection = JSON.stringify(context.messages.at(-1));
      return fauxAssistantMessage('Understood, not posting.');
    }),
  });
  await session.submit('Approve this PR', 'run-2');
  const http = await serve(session);
  const item = await firstAsk(session);
  await http.json(`/approvals/${item.id}`, { decision: 'reject', reason: 'finding 2 is wrong', by: 'evil' }, { ...BEARER, [PRINCIPAL_HEADER]: principalHeader(who), 'x-authentik-username': 'evil' });

  const done = await session.submit('Approve this PR', 'run-2', true);
  assert.equal(done.status, 'done');
  assert.deepEqual(posted, []);
  assert.match(sawRejection, new RegExp(`Rejected by ${decider}: finding 2 is wrong`));
  await http.stop();
  await session.close();
});
}

test('an approved action killed mid-execution is reported on restart, never re-run', async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  const effects = join(dir, 'side-effects');
  // No inherited stdio: a child holding the runner's pipes keeps the file from ending. The fixture reports its port and its side effect over IPC.
  const child = spawn(process.execPath, [fileURLToPath(new URL('./fixtures/gated-session.ts', import.meta.url))], { env: { ...process.env, QUACK_SESSION_DIR: dir, SIDE_EFFECTS: effects }, stdio: ['ignore', 'ignore', 'ignore', 'ipc'] });
  const exited = new Promise((r) => child.once('exit', r));
  const effect = new Promise((r) => child.on('message', (m) => (m as { effect?: boolean })?.effect && r(true)));
  let ws: WebSocket | undefined;
  try {
    // A never-firing timer: a child that dies first rejects, and a hang is the runner's timeout.
    const base = `http://127.0.0.1:${await childPort(child, 'session', 0, () => () => {})}`;
    ws = new WebSocket(`${base.replace('http', 'ws')}/ws`);
    const shown = nextFrame(ws, (f) => f.type === 'standing' && f.approvals.length === 1);
    await fetch(`${base}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'Approve this PR', requestId: 'run-3' }) });
    const [item] = (await shown).approvals;
    await fetch(`${base}/approvals/${item.id}`, { method: 'POST', body: JSON.stringify({ decision: 'approve', by: 'jf' }) });
    await effect;
  } finally {
    ws?.close();
    child.kill('SIGKILL');
    await exited;
  }

  let sawResult = '';
  const session = await openSession({ dir, ...agentWith((context) => { sawResult = JSON.stringify(context.messages.at(-1)); return fauxAssistantMessage('Noted.'); }) });
  const done = await session.submit('Approve this PR', 'run-3', true);
  assert.equal(done.status, 'done');
  assert.match(sawResult, /post_comment was interrupted after approval and was not re-run/);
  assert.equal(await readFile(effects, 'utf8'), 'LGTM\n', 'the side effect happened once');
  assert.deepEqual(posted, [], 'the restarted session did not run the tool');
  await session.close();
});

test('the session server answers only its launch token, and a decision only with the principal the distributor names', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  const session = await openSession({ dir, ...agentWith(callPost(), fauxAssistantMessage('Done.')) });
  await session.submit('Approve this PR', 'run-auth');
  const http = await serve(session);
  for (const headers of [{}, { authorization: 'Bearer wrong' }, { 'x-authentik-username': 'jason' }] as Array<Record<string, string>>) assert.equal((await http.json('/ping', undefined, headers)).status, 401);
  const ws = new WebSocket(`${http.base.replace('http', 'ws')}/ws`);
  assert.equal(await new Promise((r) => { ws.onopen = () => r('open'); ws.onerror = () => r('refused'); }), 'refused');
  const item = await firstAsk(session);
  for (const headers of [{ 'x-authentik-username': 'jason', [PRINCIPAL_HEADER]: principalHeader({ kind: 'tailnet' }) }, BEARER, { ...BEARER, [PRINCIPAL_HEADER]: 'forged' }] as Array<Record<string, string>>) {
    assert.notEqual((await http.json(`/approvals/${item.id}`, { decision: 'approve' }, headers)).status, 200);
  }
  assert.equal((await http.json('/approvals')).body.approvals.length, 1, 'still pending');
  await http.stop();
  await session.close();
});

// The incident in #2374: the session restarts under code that refuses the call before its gate, so nothing will ever act on the ask.
test('a rerun that fails before its gate withdraws the approval, and a late answer is refused with why', async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  const first = await openSession({ dir, ...agentWith(callPost()) });
  await first.submit('Approve this PR', 'run-5');
  const { id } = await firstAsk(first);
  await first.close();

  code.refuse = 'the body must cite a line';
  try {
    const second = await openSession({ dir, ...agentWith(fauxAssistantMessage('Could not post.')) });
    assert.equal((await second.submit('Approve this PR', 'run-5', true)).status, 'done');
    const http = await serve(second);
    assert.deepEqual((await http.json('/approvals')).body.approvals, []);
    const late = await http.json(`/approvals/${id}`, { decision: 'approve' });
    assert.equal(late.status, 409);
    assert.deepEqual(late.body.withdrawn, { reason: 'task_ended', supersededBy: null });
    assert.equal(late.body.detail, "the request was withdrawn: the call's task ended without acting on it");
    const row = await rowOf(second, id);
    assert.deepEqual([row.verdict, row.withdrawn?.reason], [null, 'task_ended'], 'the answer was not recorded');
    const events = currentEvents('root', { agent: 'a', version: 1, createdAt: 0 }, { approvals: { items: { [id]: row } }, runs: [], spend: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0, at: 0 }, spentMicros: 0n });
    assert.deepEqual(events.filter((e) => e.type.startsWith('ask.')).map((e) => e.type), ['ask.opened', 'ask.cancelled'], 'the catalog is told the ask is cancelled');
    assert.deepEqual(posted, []);
    await http.stop();
    await second.close();
  } finally {
    code.refuse = '';
  }
});

test('a rerun that reaches the gate with other arguments withdraws its approval, and the next call asks about the new ones', async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  const first = await openSession({ dir, ...agentWith(callPost()) });
  await first.submit('Approve this PR', 'run-6');
  const old = await firstAsk(first);
  await first.close();

  code.suffix = ' (edited)';
  try {
    let told = '';
    const second = await openSession({ dir, ...agentWith((context) => ((told = JSON.stringify(context.messages.at(-1))), callPost()), fauxAssistantMessage('Posted.')) });
    const asked = await firstAsk(second, [old.id]);
    assert.deepEqual(asked.args, { body: 'LGTM (edited)' });
    assert.match(told, /post_comment was not run: the request was withdrawn: the call's arguments changed\. Call post_comment again/);
    assert.equal((await rowOf(second, old.id)).withdrawn?.reason, 'arguments_changed');
    const http = await serve(second);
    assert.equal((await http.json(`/approvals/${old.id}`, { decision: 'approve' })).body.withdrawn.reason, 'arguments_changed');
    assert.equal((await http.json(`/approvals/${asked.id}`, { decision: 'approve' })).status, 200);
    assert.equal((await second.submit('Approve this PR', 'run-6', true)).status, 'done');
    assert.deepEqual(posted, ['LGTM (edited)'], 'only the arguments the person saw ran');
    await http.stop();
    await second.close();
  } finally {
    code.suffix = '';
  }
});

test('a waiting gate an extension withdraws returns at once without running, and leaves the standing frame', async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  let told = '';
  const session = await openSession({ dir, ...agentWith(callPost(), (context) => ((told = JSON.stringify(context.messages.at(-1))), fauxAssistantMessage('Superseded.'))) });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const ws = new WebSocket(`ws://127.0.0.1:${(server.address() as AddressInfo).port}/ws`);
  const shown = nextFrame(ws, (f) => f.type === 'standing' && f.approvals.length === 1);
  await session.submit('Approve this PR', 'run-7');
  const [{ id }] = (await shown).approvals;
  const cleared = nextFrame(ws, (f) => f.type === 'standing' && f.approvals.length === 0);
  await session.root.commit(async (tx) => { withdrawApprovals(await tx.doc(Approvals, session.root.id), (k) => k === id, { at: 5, reason: 'superseded', supersededBy: 'contract.spec:2' }); }, ctx);
  await cleared;
  assert.equal((await session.submit('Approve this PR', 'run-7', true)).status, 'done');
  assert.match(told, /post_comment was not run: the request was withdrawn: it was superseded by a newer request \(contract\.spec:2\)\./);
  const http = await serve(session);
  assert.deepEqual((await http.json(`/approvals/${id}`, { decision: 'approve' })).body.withdrawn, { reason: 'superseded', supersededBy: 'contract.spec:2' });
  assert.deepEqual(posted, []);
  ws.close();
  await http.stop();
  await new Promise((r) => server.close(r));
  await session.close();
});

// Rows written before withdrawal existed lack the field; a decided row whose call ended keeps its verdict as a record that never acts.
test('the sweep leaves old and consumed rows alone, and a verdict on an ended call stays recorded but is withdrawn', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  const session = await openSession({ dir, ...agentWith() });
  const verdict = (decision: 'approve' | 'reject') => ({ decision, reason: null, by: 'jf', at: 2 });
  const old = {
    done: { tool: 'post_comment', args: {}, requestedAt: 1, verdict: verdict('approve'), executing: true, done: true },
    rejected: { tool: 'post_comment', args: {}, requestedAt: 1, verdict: verdict('reject'), executing: false, done: false },
    approved: { tool: 'post_comment', args: {}, requestedAt: 1, verdict: verdict('approve'), executing: false, done: false },
  };
  await session.root.commit(async (tx) => { Object.assign((await tx.doc(Approvals, session.root.id)).items, JSON.parse(JSON.stringify(old))); }, ctx);
  const http = await serve(session);
  assert.deepEqual((await http.json('/approvals')).body.approvals, []);
  const items = (await session.harness.snapshot(Approvals, session.root.id, ctx))!.items;
  assert.deepEqual([items.done, items.rejected], [old.done, old.rejected], 'a consumed or rejected row is read as before');
  assert.deepEqual([items.approved!.verdict, items.approved!.withdrawn?.reason], [old.approved.verdict, 'task_ended']);
  const late = await http.json('/approvals/approved', { decision: 'reject' });
  assert.deepEqual([late.status, late.body.detail, late.body.withdrawn], [409, 'unknown or already decided', undefined], 'withdrawn after a verdict, it reads as decided');
  await http.stop();
  await session.close();
});

const STEERED = /post_comment was not run: the request was withdrawn: a person's message reached the run while it waited\. Answer the message, then call post_comment again to ask\./;
type WhenBusy = 'steer' | 'followUp';
// A person's message, as the UI, the CLI and GitHub's /fix send it.
const messageOf = (http: Awaited<ReturnType<typeof serve>>, input: string, whenBusy: WhenBusy = 'steer') => http.json('/invocations', { input, whenBusy, origin: 'person' });

for (const whenBusy of ['steer', 'followUp'] as const) test(`a message (${whenBusy}) withdraws a waiting approval before it is accepted, the agent answers it, and its next call asks afresh`, async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  let told = '';
  const session = await openSession({ dir, ...agentWith(callPost(), (context) => ((told = JSON.stringify(context.messages.slice(-2))), callPost()), fauxAssistantMessage('Posted.')) });
  const http = await serve(session);
  await session.submit('Approve this PR', 'run-s1');
  const old = await firstAsk(session);
  assert.equal((await messageOf(http, 'why LGTM?', whenBusy)).status, 202);
  const row = await rowOf(session, old.id);
  assert.deepEqual([row.verdict, row.withdrawn?.reason, row.withdrawn?.supersededBy], [null, 'steered', null], 'withdrawn by the time the steer is accepted');
  const late = await http.json(`/approvals/${old.id}`, { decision: 'approve' });
  assert.deepEqual([late.status, late.body.withdrawn], [409, { reason: 'steered', supersededBy: null }]);
  const fresh = await firstAsk(session, [old.id]);
  assert.match(told, STEERED);
  assert.match(told, /why LGTM\?/, 'the message reached the turn after the not-run result');
  assert.equal((await http.json(`/approvals/${fresh.id}`, { decision: 'approve' })).status, 200);
  assert.equal((await session.submit('Approve this PR', 'run-s1', true)).status, 'done');
  assert.deepEqual(posted, ['LGTM'], 'only the fresh ask ran');
  await http.stop();
  await session.close();
});

// Studio sends a follow-up while a judge runs inside submit_contract, before the approval opens; `racing` sends it as the gate opens.
const EARLY: Array<[WhenBusy, boolean]> = [['steer', false], ['followUp', false], ['followUp', true]];
for (const [whenBusy, racing] of EARLY) test(`a message (${whenBusy}) ${racing ? 'racing the gate' : 'queued while the run is still producing the gated call'} withdraws its approval the moment it opens`, async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  let started!: () => void;
  const running = new Promise<void>((r) => (started = r));
  code.before = () => (started(), held);
  try {
    let told = '';
    const session = await openSession({ dir, ...agentWith(callPost(), (context) => ((told = JSON.stringify(context.messages.slice(-2))), fauxAssistantMessage('Answered.'))) });
    const http = await serve(session);
    await session.submit('Approve this PR', 'run-s2');
    await running;
    // Racing, the gate's commit and the one queueing the message land in either order: the open-row read or the sweep sees the other.
    const [sent] = await Promise.all([racing ? messageOf(http, 'wait, which PR?', whenBusy) : messageOf(http, 'wait, which PR?', whenBusy).finally(release), racing && release()]);
    assert.equal(sent.status, 202);
    assert.equal((await session.submit('Approve this PR', 'run-s2', true)).status, 'done');
    const rows = Object.entries((await session.harness.snapshot(Approvals, session.root.id, ctx))!.items);
    assert.deepEqual(rows.map(([, a]) => [a.verdict, a.withdrawn?.reason]), [[null, 'steered']]);
    assert.match(told, STEERED);
    assert.match(told, /wait, which PR\?/);
    assert.deepEqual((await http.json(`/approvals/${rows[0]![0]}`, { decision: 'approve' })).body.withdrawn, { reason: 'steered', supersededBy: null });
    assert.deepEqual(posted, []);
    await http.stop();
    await session.close();
  } finally {
    code.before = undefined;
  }
});

test('a service input (a schedule\'s run, no origin) queues behind a waiting approval and leaves it pending, as does a person origin no principal vouches for', async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  const answer = () => fauxAssistantMessage('Done.');
  const session = await openSession({ dir, ...agentWith(callPost(), answer, answer) });
  const http = await serve(session);
  await session.submit('Approve this PR', 'run-s4');
  const { id } = await firstAsk(session);
  const scheduled = await http.json('/invocations', { input: 'nightly digest', whenBusy: 'followUp' });
  // Another session's call reaches the process with the launch token and no principal.
  const unvouched = await http.json('/invocations', { input: 'claims a person', whenBusy: 'followUp', origin: 'person' }, BEARER);
  assert.deepEqual([scheduled.status, scheduled.body.status, unvouched.body.status], [202, 'queued', 'queued']);
  const row = await rowOf(session, id);
  assert.deepEqual([row.verdict, row.withdrawn], [null, undefined], 'still waiting on the person');
  assert.equal((await http.json(`/approvals/${id}`, { decision: 'approve' })).status, 200);
  assert.equal((await (await session.harness.submission(scheduled.body.submissionId, ctx))!.wait(ctx)).status, 'done');
  assert.deepEqual(posted, ['LGTM']);
  await http.stop();
  await session.close();
});

test('in one tool round, a verdict committed before a steer wins and its call runs, while the call still waiting is withdrawn', async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);
  let told = '';
  const both = fauxAssistantMessage([fauxToolCall('post_comment', { body: 'A' }), fauxToolCall('post_comment', { body: 'B' })], { stopReason: 'toolUse' });
  const session = await openSession({ dir, ...agentWith(both, (context) => ((told = JSON.stringify(context.messages.slice(-3))), fauxAssistantMessage('Done.'))) });
  const http = await serve(session);
  await session.submit('Approve this PR', 'run-s3');
  const first = await firstAsk(session);
  const second = await firstAsk(session, [first.id]);
  const [a, b] = (first.args as { body: string }).body === 'A' ? [first, second] : [second, first];
  // Not through HTTP, so no sweep or other request runs between the verdict and the steer.
  assert.equal(await resolveApproval(session.root, a.id, { decision: 'approve', by: { kind: 'person', name: 'jf' } }, ctx), true);
  assert.equal((await messageOf(http, 'thanks')).status, 202);
  assert.deepEqual([(await rowOf(session, a.id)).withdrawn, (await rowOf(session, b.id)).withdrawn?.reason], [undefined, 'steered']);
  assert.equal((await session.submit('Approve this PR', 'run-s3', true)).status, 'done');
  assert.deepEqual(posted, ['A']);
  assert.match(told, /thanks/, 'the steer was read when the round ended');
  await http.stop();
  await session.close();
});
