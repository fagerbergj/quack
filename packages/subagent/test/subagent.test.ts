import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { contentText, createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import type { AddressInfo } from 'node:net';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createSessionServer, openSession } from '@quack/session';
import { SlotGate, SlotTable, slotsSessionExtensions, type SlotClient } from '@quack/slots';
import { FakeClock } from '../../slots/test/fake-clock.ts';
import { MAX_CHILDREN, subagentExtension } from '../src/index.ts';

const root = await mkdtemp(join(tmpdir(), 'quack-subagent-'));
after(() => rm(root, { recursive: true, force: true }));

type Seen = { running: number; peak: number; tools: string[][]; hold?: Promise<void> };

// One scripted model for parent and children: a user message "parent" starts N subagent calls; "child-i" answers after
// `barrier` children are generating at once (never, when the calls are serialized, so a serial run hangs and fails the test).
function scripted(calls: Array<{ task: string }>, barrier: number) {
  const seen: Seen = { running: 0, peak: 0, tools: [] };
  let arrived = 0;
  let open!: () => void;
  let started!: () => void;
  const startedP = new Promise<void>((r) => (started = r));
  const gate = new Promise<void>((r) => (open = r));
  const step = async (c: { messages: Array<{ role: string; content: unknown }> }, o?: { signal?: AbortSignal }) => {
    const last = c.messages.at(-1)!;
    if (last.role === 'toolResult') return fauxAssistantMessage(c.messages.filter((m) => m.role === 'toolResult').map((m) => contentText(m.content as never)).join(' | '));
    const text = contentText(last.content as never);
    if (text === 'parent') return fauxAssistantMessage(calls.map((x) => fauxToolCall('subagent', x)), { stopReason: 'toolUse' });
    const system = c.messages[0] as unknown as { content: string; toolsAdded?: Array<{ name: string }> };
    seen.tools.push((system.toolsAdded ?? []).map((t) => t.name));
    seen.running += 1;
    seen.peak = Math.max(seen.peak, seen.running);
    started();
    if (++arrived >= barrier) open();
    await (seen.hold ? Promise.race([seen.hold, new Promise((_, no) => o?.signal?.addEventListener('abort', () => no(o.signal!.reason)))]) : gate);
    seen.running -= 1;
    return fauxAssistantMessage(`done ${text}`);
  };
  const faux = fauxProvider({ models: [{ id: 'm' }] });
  faux.setResponses(Array.from({ length: 40 }, () => step as never));
  const models = createModels();
  models.setProvider(faux.provider);
  const model = faux.getModel();
  return { seen, models, model: { provider: model.provider, modelId: model.id }, started: startedP };
}

let n = 0;
async function run(calls: Array<{ task: string }>, barrier: number) {
  const s = scripted(calls, barrier);
  const session = await openSession({ dir: join(root, `s${n++}`), models: s.models, agent: { model: s.model, extensions: [subagentExtension()] } });
  const result = await session.submit('parent', 'r1', true);
  return { ...s, session, result };
}

test('calls in one turn run in parallel, each child answers, and a child has no subagent tool', async () => {
  const { seen, result, session } = await run([{ task: 'child-1' }, { task: 'child-2' }, { task: 'child-3' }], 3);
  assert.equal(result.status, 'done');
  assert.equal(seen.peak, 3);
  assert.deepEqual((result as { answer: string }).answer.split(' | ').sort(), ['done child-1', 'done child-2', 'done child-3']);
  assert.ok(seen.tools.every((t) => !t.includes('subagent')));
  // The call's details name the child, so a viewer can attach to it.
  const tool = (await session.transcript()).filter((e) => e.kind === 'pi.tool-result');
  assert.equal(tool.length, 3);
  assert.ok(tool.every((e) => /conversationId/.test(JSON.stringify(e))));
  await session.close();
});

test('the session serves a child conversation\'s messages by the id in the call\'s details, and not a fork or a stranger', async (t) => {
  const s = scripted([{ task: 'child-1' }], 1);
  let finish!: () => void;
  s.seen.hold = new Promise<void>((r) => (finish = r));
  const session = await openSession({ dir: join(root, `s${n++}`), models: s.models, agent: { model: s.model, extensions: [subagentExtension()] } });
  await session.submit('parent', 'r1', false);
  await s.started;
  // A decision fork is task-owned like a child, so only creation tells them apart; it is made while the call's task is live.
  const [id, fork] = await session.root.commit(async (tx) => {
    const child = (await tx.scanConversations({ ownerConversationId: session.root.id }, 1)).items[0]!;
    const forked = await tx.forkConversation(session.root.id, (await tx.latestEntryId(session.root.id))!, { ownership: { kind: 'task', taskId: child.owner!.taskId } });
    return [String(child.id), String(forked.id)];
  }, BACKGROUND_CONTEXT);
  finish();
  assert.equal((await session.submit('again', 'r2', true)).status, 'done');
  const server = createSessionServer(session).listen(0);
  t.after(async () => { await new Promise((r) => server.close(r)); await session.close(); });
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const child = await fetch(`${base}/messages?conversation=${id}`).then((r) => r.json()) as { messages: Array<{ role: string; text: string }> };
  assert.deepEqual(child.messages, [{ role: 'user', text: 'child-1' }, { role: 'assistant', text: 'done child-1' }]);
  const unknown = await fetch(`${base}/messages?conversation=nope`);
  const forked = await fetch(`${base}/messages?conversation=${fork}`);
  assert.deepEqual([unknown.status, forked.status], [404, 404]);
  assert.deepEqual(await forked.json(), await unknown.json());
  assert.equal((await fetch(`${base}/messages?conversation=${String(session.root.id)}`)).status, 200);
});

test('a restart mid-call reuses the same child and still returns its answer', async () => {
  const dir = join(root, `s${n++}`);
  const first = scripted([{ task: 'child-1' }], 1);
  const a = await openSession({ dir, models: first.models, agent: { model: first.model, extensions: [subagentExtension()] } });
  // The first process dies while the child is generating: its model call never answers.
  const stuck = new Promise<void>(() => {});
  first.seen.hold = stuck;
  await a.submit('parent', 'r1', false);
  await first.started;
  await a.close();
  const second = scripted([{ task: 'child-1' }], 1);
  const b = await openSession({ dir, models: second.models, agent: { model: second.model, extensions: [subagentExtension()] } });
  const result = await b.submit('parent', 'r1', true);
  assert.equal((result as { answer: string }).answer, 'done child-1');
  assert.equal(second.seen.tools.length, 1, 'one child ran again, not a second one');
  await b.close();
});

test('the call past the fixed limit is refused and the others still answer', async () => {
  const calls = Array.from({ length: MAX_CHILDREN + 1 }, (_, i) => ({ task: `child-${i}` }));
  const { result, session } = await run(calls, 1);
  const answer = (result as { answer: string }).answer;
  assert.match(answer, new RegExp(`already started ${MAX_CHILDREN} subagents`));
  assert.equal(answer.match(/done child-\d+/g)?.length, MAX_CHILDREN);
  await session.close();
});

test('parallel children each take a model slot, so a session never holds more than the provider gives', async () => {
  const clock = new FakeClock();
  const table = new SlotTable({ dbPath: ':memory:', limits: { faux: 2 }, ttlMs: 30_000, now: clock.now });
  let peakHeld = 0;
  const client: SlotClient = {
    acquire: async (e, h) => { const r = table.acquire(e, h); peakHeld = Math.max(peakHeld, table.snapshot()[0].held.length); return r; },
    heartbeat: async (e, h) => table.heartbeat(e, h),
    release: async (e, h) => void table.release(e, h),
  };
  const gate = new SlotGate({ client, holder: 's', clock });
  const s = scripted([{ task: 'child-1' }, { task: 'child-2' }, { task: 'child-3' }], 2);
  const session = await openSession({ dir: join(root, `s${n++}`), models: s.models, agent: { model: s.model, extensions: [...slotsSessionExtensions({}, gate), subagentExtension()] } });
  let result: Awaited<ReturnType<typeof session.submit>> | undefined;
  const running = session.submit('parent', 'r1', true).then((r) => (result = r));
  // The third child polls for a slot on the fake clock, so time moves only here; the bound turns a wedge into a failure.
  for (let polls = 0; !result && polls < 2_000; polls++) await clock.advance(1_000);
  assert.ok(result, 'the run settled');
  await running;
  assert.equal(result.status, 'done');
  assert.equal(s.seen.tools.length, 3);
  assert.equal(s.seen.peak, 2);
  assert.equal(peakHeld, 2);
  await session.close();
  await gate.release();
  table.close();
});
