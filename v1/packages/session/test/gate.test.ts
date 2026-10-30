import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { gated } from '@quack/sdk';
import { createSessionServer, openSession } from '../src/index.ts';

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
        parameters: Type.Object({ body: Type.String() }),
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

async function serve(session: Awaited<ReturnType<typeof openSession>>) {
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const json = (path: string, body?: unknown) =>
    fetch(base + path, body === undefined ? undefined : { method: 'POST', body: JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json() }));
  return { json, stop: () => new Promise((r) => server.close(r)) };
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

test('a rejection reaches the model and the tool never runs', async () => {
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
  await http.json(`/approvals/${item.id}`, { decision: 'reject', reason: 'finding 2 is wrong', by: 'jf' });

  const done = await session.submit('Approve this PR', 'run-2', true);
  assert.equal(done.status, 'done');
  assert.deepEqual(posted, []);
  assert.match(sawRejection, /Rejected by jf: finding 2 is wrong/);
  await http.stop();
  await session.close();
});
