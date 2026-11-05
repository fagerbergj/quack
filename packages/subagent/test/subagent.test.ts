import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { contentText, createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import type { AddressInfo } from 'node:net';
import { createSessionServer, openSession } from '@quack/session';
import { SlotGate, SlotTable, slotsSessionExtensions, type SlotClient } from '@quack/slots';
import { subagentExtension, type ResolveAgent } from '../src/index.ts';

const root = await mkdtemp(join(tmpdir(), 'quack-subagent-'));
after(() => rm(root, { recursive: true, force: true }));

type Seen = { running: number; peak: number; tools: string[][]; prompts: string[]; hold?: Promise<void> };

// One scripted model for parent and children: a user message "parent" starts N subagent calls; "child-i" answers after
// `barrier` children are generating at once (never, when the calls are serialized, so a serial run hangs and fails the test).
function scripted(calls: Array<{ task: string; agent?: string }>, barrier: number) {
  const seen: Seen = { running: 0, peak: 0, tools: [], prompts: [] };
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
    seen.prompts.push(JSON.stringify(system));
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
async function run(calls: Array<{ task: string; agent?: string }>, opts: { barrier: number; limits?: { maxConcurrent?: number; maxTotal?: number }; resolve?: ResolveAgent }) {
  const s = scripted(calls, opts.barrier);
  const session = await openSession({ dir: join(root, `s${n++}`), models: s.models, agent: { model: s.model, extensions: [subagentExtension(opts.limits, opts.resolve)] } });
  const result = await session.submit('parent', 'r1', true);
  return { ...s, session, result };
}

test('calls in one turn run in parallel, each child answers, and a child has no subagent tool', async () => {
  const { seen, result, session } = await run([{ task: 'child-1' }, { task: 'child-2' }, { task: 'child-3' }], { barrier: 3 });
  assert.equal(result.status, 'done');
  assert.equal(seen.peak, 3);
  assert.deepEqual((result as { answer: string }).answer.split(' | ').sort(), ['done child-1', 'done child-2', 'done child-3']);
  assert.ok(seen.tools.every((t) => !t.includes('subagent')));
  // The call's details name the child, so a viewer can attach to it.
  const tool = (await session.transcript())!.filter((e) => e.kind === 'pi.tool-result');
  assert.equal(tool.length, 3);
  assert.ok(tool.every((e) => /conversationId/.test(JSON.stringify(e))));
  await session.close();
});

test('the session serves a child conversation\'s transcript by the id in the call\'s details, and only its own', async () => {
  const { session } = await run([{ task: 'child-1' }], { barrier: 1 });
  const results = (await session.transcript())!.filter((e) => e.kind === 'pi.tool-result');
  const id = (results[0].model as Array<{ details: { conversationId: string } }>)[0].details.conversationId;
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const child = await fetch(`${base}/transcript?conversation=${id}`).then((r) => r.json()) as { transcript: Array<{ kind: string }> };
  assert.deepEqual(child.transcript.map((e) => e.kind), ['pi.user', 'pi.assistant']);
  assert.equal((await fetch(`${base}/transcript?conversation=nope`)).status, 404);
  await new Promise((r) => server.close(r));
  await session.close();
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

test('maxConcurrent queues the rest instead of exceeding it', async () => {
  const { seen, result, session } = await run([{ task: 'child-1' }, { task: 'child-2' }, { task: 'child-3' }], { barrier: 1, limits: { maxConcurrent: 1 } });
  assert.equal(result.status, 'done');
  assert.equal(seen.peak, 1);
  assert.equal(seen.tools.length, 3);
  await session.close();
});

test('maxTotal refuses the call past it and the others still answer', async () => {
  const { result, session } = await run([{ task: 'child-1' }, { task: 'child-2' }], { barrier: 1, limits: { maxTotal: 1 } });
  const answer = (result as { answer: string }).answer;
  assert.match(answer, /already started 1 subagents/);
  assert.match(answer, /done child-\d/);
  await session.close();
});

test('an agent name is resolved for the child\'s prompt and an unknown one is refused', async () => {
  const resolve: ResolveAgent = async (name) => {
    if (name !== 'researcher') throw new Error(`no agent named ${name}`);
    return { model: { provider: 'faux', modelId: 'm' }, instructions: 'You are the researcher.' };
  };
  const { seen, result, session } = await run([{ task: 'child-1', agent: 'researcher' }, { task: 'child-2', agent: 'ghost' }], { barrier: 1, resolve });
  assert.match((result as { answer: string }).answer, /no agent named ghost/);
  assert.equal(seen.prompts.length, 1);
  assert.match(seen.prompts[0], /You are the researcher\./);
  await session.close();
});

test('a session that cannot look agents up refuses a named one', async () => {
  const { result, session } = await run([{ task: 'child-1', agent: 'researcher' }], { barrier: 1 });
  assert.match((result as { answer: string }).answer, /cannot look up other agents/);
  await session.close();
});

test('parallel children each take a model slot, so a session never holds more than the provider gives', async () => {
  const table = new SlotTable({ dbPath: ':memory:', limits: { faux: 2 }, ttlMs: 30_000, now: Date.now });
  let peakHeld = 0;
  const client: SlotClient = {
    acquire: async (e, h) => { const r = table.acquire(e, h); peakHeld = Math.max(peakHeld, table.snapshot()[0].held.length); return r; },
    heartbeat: async (e, h) => table.heartbeat(e, h),
    release: async (e, h) => void table.release(e, h),
  };
  const gate = new SlotGate({ client, holder: 's', pollMs: 5 });
  const s = scripted([{ task: 'child-1' }, { task: 'child-2' }, { task: 'child-3' }], 2);
  const session = await openSession({ dir: join(root, `s${n++}`), models: s.models, agent: { model: s.model, extensions: [...slotsSessionExtensions({}, gate), subagentExtension()] } });
  const result = await session.submit('parent', 'r1', true);
  assert.equal(result.status, 'done');
  assert.equal(s.seen.tools.length, 3);
  assert.equal(s.seen.peak, 2);
  assert.equal(peakHeld, 2);
  await session.close();
  await gate.release();
  table.close();
});
