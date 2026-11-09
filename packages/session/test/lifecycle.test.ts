import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { defineExtension, defineTool, type Extension } from '@earendil-works/pi-durable';
import { obj } from '@earendil-works/pi-schema';
import { createCatalog, eventsRoute, fileJournal, sqliteCatalog } from '@quack/catalog';
import { EVENT_MAX_BYTES, eventBytes, inProcessFetch } from '@quack/sdk';
import { currentEvents, LifecycleStream, openSession, readStream, revisions, sessionFacts, startLifecycleSender } from '../src/index.ts';

const cleanups: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

const KEY = 'chat:lifecycle';
const facts = { agent: 'quack', version: 2, createdAt: Date.parse('2026-11-06T09:00:00Z') };
const none = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0, at: 0 };

test('the stream is the documents at their latest revision, and only a change appends a revision', () => {
  const docs = {
    approvals: { items: { a1: { tool: 'merge', args: { pr: 1 }, requestedAt: 10, verdict: { decision: 'approve' as const, reason: null, by: { kind: 'person' as const, name: 'jason' }, at: 20 }, executing: false, done: true } } },
    questions: { items: { q1: { question: 'Which?', options: ['x'], evidence: null, askedAt: 30, answer: null, closed: true } } },
    artifacts: { kinds: { review: { versions: [{ kind: 'review', version: 1, hash: 'h', content: 'c', sealedAt: 40, verdict: null }] } } },
    failures: { items: {} }, runs: [{ n: 1, startedAt: 5, endedAt: 50, failedSteps: [], lastMessage: 'bye', cancelled: [] }], spend: { ...none, input: 3, cost: 0.5, at: 45 }, spentMicros: 0n,
  };
  const events = currentEvents('root-1', { ...facts, title: 'Hello' }, docs);
  assert.deepEqual(events.map((e) => `${e.type} ${e.id}`), [
    'session.created session:root-1', 'title.set title', 'run.started run:1:started', 'run.ended run:1:ended', 'state.changed state:run:1:idle',
    'ask.opened ask:open:a1', 'ask.resolved ask:resolve:a1', 'ask.opened ask:open:q1', 'ask.cancelled ask:cancel:q1', 'artifact.sealed artifact:review:1', 'spend.recorded spend',
  ]);
  assert.deepEqual(events.find((e) => e.type === 'ask.resolved')?.data, { id: 'a1', choice: 'approve', by: { kind: 'person', id: 'jason' } });
  assert.deepEqual(events.at(-1)?.data, { cost: { currencyCode: 'USD', amountMicros: '500000' }, tokens: { input: 3, output: 0, cacheRead: 0, cacheWrite: 0 } });
  const stream = { events: [] as ReturnType<typeof revisions>, signatures: {} as Record<string, string>, acked: 0, runs: [] };
  const first = revisions(stream, events);
  assert.deepEqual(first.map((e) => e.seq), events.map((_, i) => i + 1));
  for (const e of first) { stream.events.push(e); stream.signatures[e.id] = JSON.stringify([e.type, e.at, e.data]); }
  assert.deepEqual(revisions(stream, events), [], 'nothing changed, nothing appended');
  docs.artifacts.kinds.review.versions[0]!.verdict = { passed: true } as never;
  (docs.artifacts.kinds.review.versions[0] as { links?: unknown[] }).links = [{ label: 'github', url: 'https://x/pr/1' }];
  docs.spend.cost = 0.25;
  const next = revisions(stream, currentEvents('root-1', { ...facts, title: 'Hello' }, { ...docs, spentMicros: 500000n }));
  assert.deepEqual(next.map((e) => [e.seq, e.type, e.data]), [[12, 'artifact.delivered', { kind: 'review', version: 1, where: 'github', link: 'https://x/pr/1', at: new Date(40).toISOString() }]],
    'a verdict is not core, so it appends nothing; a delivery does; a lower price never reports less spend');
  assert.deepEqual(events.find((e) => e.type === 'artifact.sealed')?.data, { kind: 'review', version: 1, hash: 'h', content: 'c', sealedAt: new Date(40).toISOString() }, 'the seal is the core deliverable only');
});

test('an over-cap run end is cut with a marker until it fits; other events are left whole', () => {
  const huge = 'w'.repeat(EVENT_MAX_BYTES);
  const run = { n: 1, startedAt: 5, endedAt: 50, failedSteps: [{ step: 's', error: huge }], lastMessage: huge, cancelled: [] };
  const [, , ended] = revisions({ events: [], signatures: {}, acked: 0, runs: [] }, currentEvents('r', facts, { runs: [run], spend: none, spentMicros: 0n }));
  assert.equal(ended?.type, 'run.ended');
  assert.ok(eventBytes(ended!.id, ended!.type, ended!.data) <= EVENT_MAX_BYTES);
  const data = ended!.data as { lastMessage: string; failedSteps: Array<{ error: string }> };
  assert.ok(data.lastMessage.endsWith(' … [truncated]') || data.failedSteps[0]!.error.endsWith(' … [truncated]'));
});

type Setup = { responses?: Parameters<ReturnType<typeof fauxProvider>['setResponses']>[0]; extensions?: Extension[]; now?: () => Date };

async function harness(key = KEY, setup: Setup = {}) {
  const dir = await mkdtemp(join(tmpdir(), 'quack-lifecycle-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  await writeFile(join(dir, 'agent'), 'quack');
  await writeFile(join(dir, 'created'), String(facts.createdAt));
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(setup.responses ?? [fauxAssistantMessage('All done here.'), fauxAssistantMessage('Second answer.')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: setup.extensions }, ...(setup.now && { now: setup.now }) });
  const store = sqliteCatalog(join(dir, 'catalog.sqlite'), { currency: 'USD' });
  const catalog = createCatalog({ store, journal: fileJournal(join(dir, 'journal')), log: () => {} });
  const route = eventsRoute(catalog);
  // The channel as the distributor serves it, behind a switch that plays the distributor's outages.
  const gate = { mode: 'up' as 'up' | 'down' | 'unauthorized' | 'small', posts: [] as string[] };
  const served = inProcessFetch(async (req, res) => void ((await route(req, res, key)) || res.writeHead(404).end()));
  const fetcher: typeof fetch = async (url, init) => {
    gate.posts.push(String(init?.body));
    if (gate.mode === 'down') return new Response('{}', { status: 503 });
    if (gate.mode === 'unauthorized') return new Response('{"detail":"revoked"}', { status: 401 });
    if (gate.mode === 'small' && JSON.parse(String(init?.body)).events.length > 2) return new Response('{}', { status: 413 });
    return served(url, init);
  };
  const timers: Array<() => void> = [];
  const fire = () => timers.splice(0).forEach((fn) => fn());
  const sender = await startLifecycleSender(session, {
    url: 'http://distributor', token: 't', key, facts: () => sessionFacts(dir), fetch: fetcher, setTimer: (fn) => (timers.push(fn), () => {}),
    lastMessage: async () => (await session.messages(4)).filter((x) => x.role === 'assistant').at(-1)?.text, ...(setup.now && { now: () => +setup.now!() }),
  });
  cleanups.push(async () => { await sender.stop(); await session.close(); await store.close(); });
  const stream = () => session.harness.snapshot(LifecycleStream, session.root.id, ctx);
  // Settles the sender's watch-driven flushes without a wall-clock wait.
  const settle = async () => { for (let i = 0; i < 50; i++) { await sender.flush(); await new Promise((r) => setImmediate(r)); } };
  return { dir, session, catalog, store, gate, fire, timers, sender, stream, settle, restart: () => startLifecycleSender(session, { url: 'http://distributor', token: 't', key, facts: () => sessionFacts(dir), fetch: fetcher, setTimer: () => () => {} }) };
}

test('a session reports its creation, runs and last message, and the catalog acks them', async () => {
  const h = await harness();
  assert.equal((await h.catalog.getSession(KEY))?.agent, 'quack');
  await h.session.submit('hello', undefined, true);
  await h.settle();
  const row = await h.catalog.getSession(KEY);
  assert.deepEqual([row?.state, row?.lastRun?.outcome, row?.lastMessage, row?.title], ['idle', 'succeeded', 'All done here.', 'hello']);
  const doc = await h.stream();
  assert.equal(doc?.acked, doc?.events.length, 'everything recorded is acked');
  assert.deepEqual(doc?.events.map((e) => e.seq), doc?.events.map((_, i) => i + 1), 'the stream is contiguous');
});

test('while the distributor is down events wait in the log, and the retry delivers them', async () => {
  const h = await harness();
  h.gate.mode = 'down';
  await h.session.submit('hello', undefined, true);
  await h.settle();
  const waiting = await h.stream();
  assert.ok(waiting!.events.length > waiting!.acked, 'unsent events are in the log');
  assert.ok(h.timers.length > 0, 'a retry is armed');
  h.gate.mode = 'up';
  h.fire();
  await h.settle();
  assert.equal((await h.catalog.getSession(KEY))?.lastRun?.outcome, 'succeeded');
  assert.equal((await h.stream())?.acked, (await h.stream())?.events.length);
});

test('a reset catalog is refilled from the log: the lower ack re-sends from there', async () => {
  const h = await harness();
  await h.session.submit('hello', undefined, true);
  await h.settle();
  const before = await h.catalog.getSession(KEY);
  await h.store.resetSession({ key: KEY });
  await h.session.submit('again', undefined, true);
  await h.settle();
  const after = await h.catalog.getSession(KEY);
  assert.deepEqual([after?.createdAt, after?.title, after?.lastMessage], [before?.createdAt, before?.title, 'Second answer.']);
});

test('a 413 splits the batch; a 401 is not retried with the same body', async () => {
  const h = await harness();
  await h.store.resetSession({ key: KEY });
  h.gate.mode = 'small';
  await h.session.submit('hello', undefined, true);
  await h.settle();
  assert.equal((await h.catalog.getSession(KEY))?.lastRun?.outcome, 'succeeded', 'split batches deliver everything');
  h.gate.mode = 'unauthorized';
  await h.store.resetSession({ key: KEY });
  await h.session.submit('again', undefined, true);
  await h.settle();
  const tried = h.gate.posts.length;
  await h.sender.flush();
  assert.equal(h.gate.posts.length, tried, 'the refused body is not sent again');
});

test('the stream a stopped session derives is the one it then sends, so a backfill and the session agree', async () => {
  const h = await harness();
  await h.session.submit('hello', undefined, true);
  await h.settle();
  const recorded = (await h.stream())!.events;
  const again = await readStream(h.session.harness, h.session.root, await sessionFacts(h.dir), () => undefined, ctx);
  assert.deepEqual([again.recorded, again.fresh], [true, []], 'nothing left to append');
  assert.deepEqual(again.doc.events, recorded);
});

test("deciders recorded by today's code, a {name} or a bare string, reach the channel as the port's {id} Principal", () => {
  const answered = (by: unknown) => ({ question: 'Q?', options: ['y'], evidence: null, askedAt: 1, answer: { choice: 'y', by: by as never, at: 2 }, closed: false });
  const events = currentEvents('r', facts, { questions: { items: { q1: answered({ kind: 'person', name: 'jason' }), q2: answered('alice'), q3: answered('tailnet'), q4: answered({ kind: 'tailnet' }) } }, runs: [], spend: none, spentMicros: 0n });
  assert.deepEqual(events.filter((e) => e.type === 'ask.resolved').map((e) => e.data.by), [{ kind: 'person', id: 'jason' }, { kind: 'person', id: 'alice' }, { kind: 'tailnet' }, { kind: 'tailnet' }]);
});

test('a process that comes up without a run reports idle, after the distributor recorded it stopped', async () => {
  const h = await harness();
  await h.catalog.stopped(KEY);
  assert.equal((await h.catalog.getSession(KEY))?.state, 'stopped');
  await h.sender.stop();
  const again = await h.restart();
  await again.flush();
  await again.stop();
  assert.equal((await h.catalog.getSession(KEY))?.state, 'idle');
  assert.deepEqual((await h.stream())?.events.filter((e) => e.id.startsWith('state:boot:')).map((e) => e.id), ['state:boot:1', 'state:boot:2']);
});

test('a process that comes up mid-run reports running again, after the distributor recorded it stopped', async () => {
  let release!: () => void;
  let reached!: () => void;
  const held = new Promise<void>((r) => (release = r));
  const started = new Promise<void>((r) => (reached = r));
  const wait = defineTool({ name: 'wait', description: 'waits', parameters: obj({}, { open: true }), replay: 'safe', execute: async () => (reached(), await held, { output: [{ type: 'text', text: 'ok' }] }) });
  const h = await harness(KEY, { responses: [fauxAssistantMessage(fauxToolCall('wait', {}), { stopReason: 'toolUse' }), fauxAssistantMessage('Done.')], extensions: [defineExtension({ name: 'waits', tools: [wait] })] });
  await h.session.submit('go', undefined, false);
  await started;
  await h.settle();
  // The idle sweep stopped the process while its run waited (on a person, or a slow tool); a request started it again.
  await h.catalog.stopped(KEY);
  await h.sender.stop();
  const again = await h.restart();
  await again.flush();
  assert.equal((await h.catalog.getSession(KEY))?.state, 'running');
  release();
  await h.session.root.waitForIdle(ctx);
  for (let i = 0; i < 50; i++) { await again.flush(); await new Promise((r) => setImmediate(r)); }
  await again.stop();
  const row = await h.catalog.getSession(KEY);
  assert.deepEqual([row?.state, row?.lastRun?.outcome], ['idle', 'succeeded']);
  assert.deepEqual((await h.stream())?.events.filter((e) => e.type === 'state.changed').map((e) => e.id), ['state:boot:1', 'state:run:1:resume:1', 'state:run:1:idle']);
});

test('a session mid-run with two ended turns shows lastActivity at the second turn\'s end', async () => {
  let clock = Date.parse('2026-11-06T10:00:00Z');
  let release!: () => void;
  let reached!: () => void;
  const held = new Promise<void>((r) => (release = r));
  const third = new Promise<void>((r) => (reached = r));
  let calls = 0;
  // Each round takes a minute on the fake clock; the third holds the run open.
  const step = defineTool({
    name: 'step', description: 'one step', parameters: obj({}, { open: true }), replay: 'safe',
    execute: async () => {
      clock += 60_000;
      if (++calls === 3) { reached(); await held; }
      return { output: [{ type: 'text', text: 'ok' }] };
    },
  });
  const round = () => fauxAssistantMessage(fauxToolCall('step', {}), { stopReason: 'toolUse' });
  const h = await harness(KEY, { responses: [round(), round(), round(), fauxAssistantMessage('Done.')], extensions: [defineExtension({ name: 'steps', tools: [step] })], now: () => new Date(clock) });
  await h.session.submit('go', undefined, false);
  await third;
  await h.settle();
  const row = await h.catalog.getSession(KEY);
  assert.deepEqual([row?.state, row?.lastActivity, row?.lastRun], ['running', Date.parse('2026-11-06T10:02:00Z'), undefined]);
  release();
  await h.session.root.waitForIdle(ctx);
  await h.settle();
  const turns = (await h.stream())!.events.filter((e) => e.type === 'turn.ended');
  assert.deepEqual(turns.map((e) => [e.id, e.data]), [1, 2, 3].map((index) => [`run:1:turn:${index}:ended`, { run: 1, index }]), 'one event per tool round, one-based');
  assert.equal((await h.catalog.getSession(KEY))?.lastActivity, (await h.catalog.getSession(KEY))?.lastRun?.endedAt, 'the run end, later in the channel, is the row\'s');
});
