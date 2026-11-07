import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { num, obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool, LiveDoc } from '@earendil-works/pi-durable';
import { gated, onWaiting } from '@quack/sdk';
import { createSessionServer, openSession } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));
const tempDir = async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-abort-queue-'));
  dirs.push(dir);
  return dir;
};

type Context = { messages: Array<{ role: string; content: unknown }> };
const usersOf = (c: Context) => c.messages.filter((m) => m.role === 'user').map((m) => (typeof m.content === 'string' ? m.content : JSON.stringify(m.content)));

// The first model call parks on a gated tool, as the prod run did on an approval; every later call answers.
function parkedAgent(calls: string[][]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  const respond = (c: Context) => {
    calls.push(usersOf(c));
    return calls.length === 1 ? fauxAssistantMessage(fauxToolCall('submit_contract', {}), { stopReason: 'toolUse' }) : fauxAssistantMessage(`answer ${calls.length}`);
  };
  faux.setResponses(Array.from({ length: 8 }, () => respond) as never);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const tool = gated(defineTool({ name: 'submit_contract', description: 'put a version up', parameters: obj({}, { open: true }), execute: async () => ({ output: [{ type: 'text', text: 'ok' }] }) }));
  return { models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'gate', tools: [tool] })] } };
}

const parked = () => new Promise<void>((resolve) => { const off = onWaiting((n) => n > 0 && (off(), resolve())); });

// Parks a run, then queues each text as a follow-up behind it.
async function parkAndQueue(dir: string, calls: string[][], texts: string[], lines: string[] = []) {
  const session = await openSession({ dir, key: 'contracts:test', log: (l) => lines.push(l), ...parkedAgent(calls) });
  const waiting = parked();
  await session.submit('write the contract');
  await waiting;
  const ids: string[] = [];
  for (const text of texts) {
    const r = await session.submit(text, undefined, false, 'followUp');
    assert.equal(r.status, 'queued');
    ids.push(r.submissionId);
  }
  return { session, ids };
}

const settle = async (session: Awaited<ReturnType<typeof openSession>>, id: string) => (await session.harness.submission(id as never, ctx))!.wait(ctx);

// The call that took the follow-ups holds each once, in submission order, and no other call holds any.
function deliveredOnce(calls: string[][], texts: string[]) {
  const took = calls.filter((users) => texts.some((t) => users.some((u) => u.includes(t))));
  assert.equal(took.length, 1, `one model call took the follow-ups: ${JSON.stringify(calls)}`);
  const at = texts.map((t) => took[0].findIndex((u) => u.includes(t)));
  assert.ok(at.every((i) => i >= 0) && at.every((i, n) => n === 0 || i > at[n - 1]), `in order: ${JSON.stringify(took[0])}`);
  for (const t of texts) assert.equal(took[0].filter((u) => u.includes(t)).length, 1, `exactly once: ${t}`);
}

test('POST /abort stops the run and the follow-ups queued behind it start the next run, in order, exactly once', async (t) => {
  const calls: string[][] = [];
  const lines: string[] = [];
  const { session, ids } = await parkAndQueue(await tempDir(), calls, ['also cover pagination', 'and rate limits'], lines);
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  t.after(async () => { await server.shutdown(); await session.close(); });
  const res = await fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/abort`, { method: 'POST' });
  assert.equal(res.status, 200);
  for (const id of ids) assert.equal((await settle(session, id)).status, 'done', 'the queued follow-up was answered, not withdrawn');
  deliveredOnce(calls, ['also cover pagination', 'and rate limits']);
  assert.deepEqual(await session.queued(), []);
  const line = JSON.parse(lines.find((l) => l.includes('"session.abort"'))!);
  assert.equal(line.key, 'contracts:test');
  assert.deepEqual(line.caller, { kind: 'tailnet' });
  assert.deepEqual(line.kept, ids);
  assert.equal(line.queue, 'started the next run');
});

test('a withdrawn follow-up is gone after an abort; the one still queued starts the next run', async () => {
  const calls: string[][] = [];
  const lines: string[] = [];
  const { session, ids } = await parkAndQueue(await tempDir(), calls, ['drop this one', 'keep this one'], lines);
  assert.equal(await session.withdraw(ids[0], false, { kind: 'person', name: 'jason' }), true);
  await session.abort({ kind: 'tailnet' });
  assert.equal((await settle(session, ids[0])).status, 'unanswered');
  assert.equal((await settle(session, ids[1])).status, 'done');
  deliveredOnce(calls, ['keep this one']);
  assert.ok(calls.every((users) => !users.some((u) => u.includes('drop this one'))), 'the withdrawn follow-up never reached the model');
  const withdrawn = JSON.parse(lines.find((l) => l.includes('"session.withdraw"'))!);
  assert.deepEqual([withdrawn.caller, withdrawn.submission, withdrawn.queue], [{ kind: 'person', name: 'jason' }, ids[0], 'removed']);
  await session.close();
});

test('an abort with nothing queued leaves the session idle without starting a run', async () => {
  const calls: string[][] = [];
  const { session } = await parkAndQueue(await tempDir(), calls, []);
  await session.abort('service');
  assert.equal((await session.harness.snapshot(LiveDoc, session.root.id, ctx))?.run, undefined);
  assert.equal(calls.length, 1, 'no model call after the abort');
  await session.close();
});

test('follow-ups an abort left queued when the process stopped start the next run on reopen, exactly once', async () => {
  const dir = await tempDir();
  const calls: string[][] = [];
  const { session, ids } = await parkAndQueue(dir, calls, ['first follow-up', 'second follow-up']);
  // The run's tasks end, then the process stops before the queue is started: what a crash mid-abort leaves.
  const top = (await session.harness.inspect(ctx)).tasks.filter((x) => x.record.owner === undefined);
  for (const x of top) await session.harness.abortTask(x.record.id, ctx);
  for (const x of top) await session.harness.waitForTask(x.record.id, ctx);
  assert.equal((await session.queued()).length, 2);
  await session.close();
  const again = await openSession({ dir, ...parkedAgent(calls) });
  for (const id of ids) assert.equal((await settle(again, id)).status, 'done');
  deliveredOnce(calls, ['first follow-up', 'second follow-up']);
  await again.close();
});

test('the run ceiling stops the run and keeps the follow-ups queued behind it for the next run', async () => {
  let release!: () => void;
  let started!: () => void;
  const held = new Promise<void>((r) => (release = r));
  const running = new Promise<void>((r) => (started = r));
  const probe = defineTool({
    name: 'probe', description: 'Looks something up', parameters: obj({ n: num() }, { open: true }),
    execute: async (a) => { if (a.n === 0) { started(); await held; } return { output: [{ type: 'text', text: `result ${a.n}` }] }; },
  });
  const calls: string[][] = [];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  // Distinct calls and results keep the loop guard quiet; past the ceiling, calls answer.
  faux.setResponses(Array.from({ length: 12 }, () => (c: Context) => {
    calls.push(usersOf(c));
    return calls.length <= 3 ? fauxAssistantMessage(fauxToolCall('probe', { n: calls.length - 1 }), { stopReason: 'toolUse' }) : fauxAssistantMessage('Done.');
  }) as never);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const lines: string[] = [];
  const session = await openSession({ dir: await tempDir(), models, log: (l) => lines.push(l), agent: { model: { provider: m.provider, modelId: m.id }, turnCeiling: 2, extensions: [defineExtension({ name: 'probe', tools: [probe] })] } });
  const first = await session.submit('go');
  await running;
  const ids = [(await session.submit('one more thing', undefined, false, 'followUp')).submissionId, (await session.submit('and another', undefined, false, 'followUp')).submissionId];
  release();
  assert.equal((await settle(session, first.submissionId)).status, 'unanswered', 'the ceiling ended the first run');
  for (const id of ids) assert.equal((await settle(session, id)).status, 'done');
  deliveredOnce(calls, ['one more thing', 'and another']);
  const line = JSON.parse(lines.find((l) => l.includes('"session.abort"'))!);
  assert.deepEqual([line.caller, line.kept], ['run-ceiling', ids]);
  await session.close();
});
