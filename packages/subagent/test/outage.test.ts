import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { contentText, createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { LiveDoc } from '@earendil-works/pi-durable';
import { agentSpec, openSession, parseDefinition, parseDeployment } from '@quack/session';
import { FakeClock } from '../../capacity/test/fake-clock.ts';
import { subagentExtension } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-subagent-outage-'));
after(() => rm(dir, { recursive: true, force: true }));
const HOUR = 3_600_000;

// The parent hands each task to a child; a child whose task is in `down` gets that error, as from an endpoint refusing it.
function endpoint(provider?: string) {
  const down = new Map<string, string>();
  const tries = new Map<string, number>();
  let hold: Promise<void> | undefined;
  const step = async (c: { messages: Array<{ role: string; content: unknown }> }) => {
    const last = c.messages.at(-1)!;
    if (last.role === 'toolResult') return fauxAssistantMessage(c.messages.filter((m) => m.role === 'toolResult').map((m) => contentText(m.content as never)).sort().join(' | '));
    const text = contentText(last.content as never);
    if (text.startsWith('parent ')) return fauxAssistantMessage(text.split(' ').slice(1).map((task) => fauxToolCall('subagent', { task })), { stopReason: 'toolUse' });
    tries.set(text, (tries.get(text) ?? 0) + 1);
    if (down.has(text)) return fauxAssistantMessage('', { stopReason: 'error', errorMessage: down.get(text)! });
    await hold;
    return fauxAssistantMessage(`done ${text}`);
  };
  const faux = fauxProvider({ ...(provider && { provider }), models: [{ id: 'm' }] });
  faux.setResponses(Array.from({ length: 400 }, () => step as never));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  return { down, tries, models, model: { provider: m.provider, modelId: m.id }, holdWith: (p?: Promise<void>) => (hold = p) };
}

type Endpoint = ReturnType<typeof endpoint>;
let n = 0;
async function open(e: Endpoint, clock: FakeClock, name = `s${n++}`, outageHours = 6) {
  const session = await openSession({ dir: join(dir, name), models: e.models, agent: { model: e.model, extensions: [subagentExtension()] }, now: () => new Date(clock.t), setTimer: clock.setTimer, outageHours });
  const said = async (text: string) => (await session.messages(10)).some((x) => x.role === 'assistant' && x.text === text);
  const children = () => session.root.commit(async (tx) => (await tx.scanConversations({ ownerConversationId: session.root.id }, 10)).items.map((c) => c.id), BACKGROUND_CONTEXT);
  return { session, said, children, waiting: async () => (await session.standing()).waiting };
}

// Moves a minute at a time until `done`, up to `limitMs` of Harness time.
async function until(clock: FakeClock, done: () => Promise<boolean>, limitMs = HOUR) {
  for (let waited = 0; waited <= limitMs; waited += 60_000) {
    if (await done()) return true;
    await clock.advance(60_000);
  }
  return done();
}

test('a subagent child whose model refuses connections shows the parent waiting to retry its provider, until the child answers', async () => {
  const clock = new FakeClock();
  const e = endpoint();
  e.down.set('a', 'Connection error.');
  const { session, said, waiting } = await open(e, clock);
  await session.submit('parent a', 'r1');
  assert.ok(await until(clock, async () => (e.tries.get('a') ?? 0) >= 3));
  assert.deepEqual([(await waiting())?.provider, (await waiting())?.error], [e.model.provider, 'Connection error.']);
  assert.equal((await session.standing()).note, `Waiting to retry ${e.model.provider}: Connection error.`);
  e.down.delete('a');
  assert.ok(await until(clock, () => said('done a')));
  assert.equal(await waiting(), null);
  await session.close();
});

test('two children parked at once are one wait: the oldest child first, then the other once the first answers', async () => {
  const clock = new FakeClock();
  const e = endpoint();
  e.down.set('a', 'Connection error.');
  e.down.set('b', '502 no route to host');
  const { session, said, waiting } = await open(e, clock);
  await session.submit('parent a b', 'r1');
  assert.ok(await until(clock, async () => (e.tries.get('a') ?? 0) >= 3 && (e.tries.get('b') ?? 0) >= 3));
  const both = await waiting();
  assert.equal(both?.error, 'Connection error.');
  e.down.delete('a');
  assert.ok(await until(clock, async () => (await waiting())?.error === '502 no route to host'));
  assert.equal((await waiting())?.since, both?.since, 'the earliest streak still leads');
  e.down.delete('b');
  assert.ok(await until(clock, () => said('done a | done b')));
  assert.equal(await waiting(), null);
  await session.close();
});

test('a parked child whose next try starts clears the wait while that try is still answering', async () => {
  const clock = new FakeClock();
  const e = endpoint();
  e.down.set('a', 'Connection error.');
  let release!: () => void;
  e.holdWith(new Promise<void>((r) => (release = r)));
  const { session, said, waiting } = await open(e, clock);
  await session.submit('parent a', 'r1');
  assert.ok(await until(clock, async () => (await waiting()) !== null));
  e.down.delete('a');
  const tried = e.tries.get('a')!;
  assert.ok(await until(clock, async () => e.tries.get('a')! > tried));
  assert.equal(await waiting(), null);
  release();
  assert.ok(await until(clock, () => said('done a')));
  await session.close();
});

test('a child that ends past its outage window drops out of the wait, and the parent hears it failed', async () => {
  const clock = new FakeClock();
  const e = endpoint();
  e.down.set('a', 'Connection error.');
  const { session, said, children, waiting } = await open(e, clock, undefined, 0.1);
  await session.submit('parent a', 'r1');
  assert.ok(await until(clock, async () => (await waiting()) !== null));
  const [child] = await children();
  assert.ok(await until(clock, async () => (await session.harness.snapshot(LiveDoc, child!, BACKGROUND_CONTEXT))?.runFailure?.reason === 'model_error'));
  assert.equal(await waiting(), null);
  assert.ok(await until(clock, async () => (await session.messages(10)).some((m) => m.role === 'assistant' && /the subagent ended without an answer/.test(m.text))));
  assert.equal(await said('done a'), false);
  await session.close();
});

test('a session reopened while a child is parked is waiting at open, and answers once the child is back', async () => {
  const clock = new FakeClock();
  const e = endpoint();
  e.down.set('a', 'Connection error.');
  const first = await open(e, clock, 'reopen');
  await first.session.submit('parent a', 'r1');
  assert.ok(await until(clock, async () => (e.tries.get('a') ?? 0) >= 3));
  await first.session.close();
  const again = await open(e, clock, 'reopen');
  assert.equal((await again.waiting())?.provider, e.model.provider);
  e.down.delete('a');
  assert.ok(await until(clock, () => again.said('done a')));
  assert.equal(await again.waiting(), null);
  await again.session.close();
});

test('a stop while a child is parked aborts the child: it tries the model no more and the wait clears', async () => {
  const clock = new FakeClock();
  const e = endpoint();
  e.down.set('a', 'Connection error.');
  const { session, children, waiting } = await open(e, clock);
  const sent = await session.submit('parent a', 'r1');
  assert.ok(await until(clock, async () => (e.tries.get('a') ?? 0) >= 3));
  assert.notEqual(await waiting(), null);
  const stopped = await session.abort('service');
  assert.deepEqual(stopped.aborted, [sent.submissionId]);
  const [child] = await children();
  const live = await session.harness.snapshot(LiveDoc, child!, BACKGROUND_CONTEXT);
  assert.deepEqual([live?.run, live?.runFailure?.reason], [undefined, 'aborted']);
  assert.equal(await waiting(), null);
  const tries = e.tries.get('a');
  await clock.advance(HOUR);
  assert.equal(e.tries.get('a'), tries);
  await session.close();
});

test('a session whose pick is on another provider than its agent\'s shows a parked child waiting on the picked provider', async () => {
  const clock = new FakeClock();
  const e = endpoint('picked');
  const agentSide = fauxProvider({ provider: 'agent-side', models: [{ id: 'm' }] });
  e.models.setProvider(agentSide.provider);
  const deployment = parseDeployment({ providers: { 'agent-side': { baseUrl: 'http://a/v1', models: { m: { contextWindow: 4096 } } }, picked: { baseUrl: 'http://p/v1', models: { m: { contextWindow: 4096 } } } } });
  const spec = await agentSpec(parseDefinition({ name: 'x', version: 1, model: 'agent-side/m', extensions: {} }, deployment, {}, undefined, { session: 'picked/m' }), {});
  e.down.set('a', 'Connection error.');
  const { session, said, waiting } = await open({ ...e, model: spec.model }, clock);
  await session.submit('parent a', 'r1');
  assert.ok(await until(clock, async () => (e.tries.get('a') ?? 0) >= 3));
  assert.deepEqual([(await waiting())?.provider, (await waiting())?.error], ['picked', 'Connection error.']);
  assert.equal(agentSide.state.callCount, 0, 'the agent\'s own provider is never called');
  e.down.delete('a');
  assert.ok(await until(clock, () => said('done a')));
  await session.close();
});
