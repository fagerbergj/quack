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
import { gated } from '@quack/sdk';
import { createSessionServer, openSession, PRINCIPAL_HEADER, principalHeader } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

const posted: string[] = [];
const extension = defineExtension({
  name: 'test-github',
  tools: [
    gated(
      defineTool({
        name: 'post_comment',
        description: 'Post a comment to the pull request',
        parameters: obj({ body: str() }, { open: true }),
        execute: async (args) => {
          posted.push(args.body);
          return { output: [{ type: 'text', text: 'posted' }] };
        },
      }),
    ),
  ],
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

// Waits for a state change; the deadline only matters when the code is broken, so it is far above any runner's latency.
async function until<T>(probe: () => Promise<T | undefined>, ms = 15_000): Promise<T> {
  const deadline = Date.now() + ms;
  for (;;) {
    const v = await probe();
    if (v !== undefined) return v;
    if (Date.now() > deadline) throw new Error('timed out');
    await new Promise((r) => setTimeout(r, 10));
  }
}

test('a gated call waits across a restart, then runs once when approved', async () => {
  posted.length = 0;
  const dir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  dirs.push(dir);

  const first = await openSession({ dir, ...agentWith(callPost()) });
  await first.submit('Approve this PR', 'run-1');
  const pending = await until(async () => {
    const list = await serve(first).then(async (h) => { const r = await h.json('/approvals'); await h.stop(); return r.body.approvals; });
    return list.length ? list : undefined;
  });
  assert.equal(pending[0].tool, 'post_comment');
  assert.deepEqual(pending[0].args, { body: 'LGTM' });
  assert.deepEqual(posted, []);
  await first.close();

  const second = await openSession({ dir, ...agentWith(fauxAssistantMessage('Posted the review.')) });
  const http = await serve(second);
  assert.equal((await http.json('/approvals')).body.approvals.length, 1, 'the approval survived the restart');

  const bad = await http.json(`/approvals/${pending[0].id}`, { decision: 'maybe', by: 'jf' });
  assert.equal(bad.status, 400);
  const ok = await http.json(`/approvals/${pending[0].id}`, { decision: 'approve', by: 'jf' });
  assert.equal(ok.status, 200);
  const again = await http.json(`/approvals/${pending[0].id}`, { decision: 'approve', by: 'jf' });
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
  const [item] = await until(async () => { const l = (await http.json('/approvals')).body.approvals; return l.length ? l : undefined; });
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
  const child = spawn(process.execPath, [fileURLToPath(new URL('./fixtures/gated-session.ts', import.meta.url))], { env: { ...process.env, QUACK_SESSION_DIR: dir, SIDE_EFFECTS: effects }, stdio: ['inherit', 'inherit', 'inherit', 'ipc'] });
  const exited = new Promise((r) => child.once('exit', r));
  try {
    const base = `http://127.0.0.1:${await new Promise<number>((r) => child.once('message', (m) => r((m as { port: number }).port)))}`;
    await fetch(`${base}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'Approve this PR', requestId: 'run-3' }) });
    const [item] = await until(async () => { const l = (await (await fetch(`${base}/approvals`)).json()).approvals; return l.length ? l : undefined; });
    await fetch(`${base}/approvals/${item.id}`, { method: 'POST', body: JSON.stringify({ decision: 'approve', by: 'jf' }) });
    await until(() => readFile(effects, 'utf8').then((t) => (t ? t : undefined), () => undefined));
  } finally {
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
  const [item] = await until(async () => { const l = (await http.json('/approvals')).body.approvals; return l.length ? l : undefined; });
  for (const headers of [{ 'x-authentik-username': 'jason', [PRINCIPAL_HEADER]: principalHeader({ kind: 'tailnet' }) }, BEARER, { ...BEARER, [PRINCIPAL_HEADER]: 'forged' }] as Array<Record<string, string>>) {
    assert.notEqual((await http.json(`/approvals/${item.id}`, { decision: 'approve' }, headers)).status, 200);
  }
  assert.equal((await http.json('/approvals')).body.approvals.length, 1, 'still pending');
  await http.stop();
  await session.close();
});
