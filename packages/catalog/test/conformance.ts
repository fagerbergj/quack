// The one conformance suite both catalog adapters pass: the store port contract's ten checks, on one shared corpus.
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { describe, test } from 'node:test';
import { escapeTerm } from '@quack/sdk';
import { CursorError, parseQuery, type Catalog, type CatalogEvent, type SessionCatalogStore } from '../src/index.ts';
import { reference, type Listed } from './reference.ts';

export type Fresh = () => Promise<{ store: SessionCatalogStore; catalog: Catalog; journalDir: string; dataDir: string }>;

export const T0 = Date.parse('2026-11-01T00:00:00Z');
const BIG = 2n ** 53n + 1n;

const CORE = new Set(['session.created', 'run.started', 'run.ended', 'turn.ended', 'ask.opened', 'ask.resolved', 'ask.cancelled', 'artifact.sealed', 'artifact.delivered', 'spend.recorded', 'title.set', 'state.changed']);
type Data<T extends CatalogEvent['type']> = Extract<CatalogEvent, { type: T }>['data'];
/** A session's event stream: seq counts up, `at` is seq seconds past T0 unless given. */
export function stream(key: string, incarnation = 'session') {
  let seq = 0;
  const events: CatalogEvent[] = [];
  const next = <T extends CatalogEvent['type']>(type: T, data: Data<T>, id = `${type}:${seq + 1}`, at = T0 + (seq + 1) * 1000) => {
    const e = { seq: ++seq, id, at, type, data, ...(!CORE.has(type) && { extension: true }) } as CatalogEvent;
    events.push(e);
    return e;
  };
  next('session.created', { agent: 'quack', createdAt: T0 }, incarnation);
  return { key, events, next, get seq() { return seq; } };
}

const action = (id: string) => ({ kind: 'action' as const, id });
const event = (id: string, seq: number) => ({ kind: 'event' as const, id, seq });
const zero = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
const usd = (amountMicros: bigint) => ({ currencyCode: 'USD', amountMicros });

async function snapshot(store: SessionCatalogStore, key: string) {
  const all = { kind: 'match' as const, field: 'session' as const, value: key };
  return {
    session: await store.getSession(key), watermark: await store.watermark(key),
    asks: (await store.listAsks({ where: all, orderBy: { field: 'createdAt', direction: 'oldest' }, pageSize: 1000 })).rows,
    artifacts: (await store.listArtifacts({ where: all, orderBy: { field: 'sealedAt', direction: 'newest' }, pageSize: 1000 })).rows,
  };
}

/** Pages a list to its end, failing on a repeated row. */
async function drain<T>(page: (cursor?: string) => Promise<{ rows: T[]; cursor?: string }>, id: (row: T) => string): Promise<T[]> {
  const out: T[] = [];
  let cursor: string | undefined;
  do {
    const p = await page(cursor);
    out.push(...p.rows);
    cursor = p.cursor;
  } while (cursor);
  assert.equal(new Set(out.map(id)).size, out.length, 'no row twice');
  return out;
}

// The shared corpus: keys with every reserved character, ties on every order, and text in every documented field.
export async function corpus(catalog: Catalog): Promise<string[]> {
  const keys: string[] = [];
  for (let i = 0; i < 24; i++) {
    const key = [`sleeper:platinum:${i}:lineup`, `review:monorepo/${i}`, `a/b#${i}@x.y_z-w`][i % 3]!;
    keys.push(key);
    const s = stream(key, `create:${i}`);
    s.next('title.set', { title: ['Lineup check matchday', 'Review the monorepo', 'Café ünïcode title'][i % 3]! + ` ${i}` });
    if (i % 4 !== 3) s.next('run.started', { run: 1 });
    if (i % 2 === 0) s.next('ask.opened', { id: `q${i}`, type: 'question', createdAt: T0 + 100 * (i % 5), prompt: { question: `Swap the tired starter number ${i}?`, options: ['keep', 'rotate'], freeText: false } });
    if (i % 3 === 0) s.next('ask.opened', { id: `a${i}`, type: 'approval', createdAt: T0 + 50 * (i % 4), toolCall: { tool: ['submit-lineup', 'merge_pr'][i % 2]!, args: { league: 'platinum', round: i } } });
    s.next('spend.recorded', { cost: usd(i === 5 ? BIG : BigInt(i * 1_250_000)), tokens: { input: i * 10, output: i, cacheRead: 0, cacheWrite: i % 2 } });
    if (i % 4 !== 3) s.next('run.ended', { run: 1, outcome: (['succeeded', 'failed', 'aborted'] as const)[i % 3]!, failedSteps: i % 3 === 1 ? [{ step: 'submit-lineup', error: 'deadline passed' }] : [], lastMessage: `done with lineup ${i % 2 ? 'rotation' : 'starters'}` }, `run:1:ended`, T0 + 60_000 * (i % 6));
    if (i % 4 === 1) s.next('state.changed', { state: 'idle' }, `state:${s.seq + 1}`);
    s.next('artifact.sealed', { kind: i % 2 ? 'sleeper.cards' : 'review.report', version: 1, hash: `h${i}`, content: `content ${i}`, sealedAt: T0 + 1000 * (i % 7), summary: `Matchday ${i} starter lineup ${i % 2 ? 'approve' : 'revise'}` });
    if (i % 3 === 0) s.next('artifact.delivered', { kind: i % 2 ? 'sleeper.cards' : 'review.report', version: 1, where: 'github', link: `https://x/${i}`, at: T0 + 5 });
    await catalog.publish(key, s.events);
    if (i % 4 === 2) await catalog.resolve(key, `q${i}`, { by: { kind: 'person', id: 'jason' }, choice: 'keep' });
    if (i % 4 === 3) await catalog.archive(key);
  }
  return keys;
}

export function conformance(name: string, fresh: Fresh, skip?: string | false) {
  describe(`catalog conformance: ${name}`, { skip }, () => {
    test('(1) every write lands exactly the row it documents, counts included', async () => {
      const { store } = await fresh();
      await store.migrate();
      await store.createSession(event('c', 1), { key: 'k', agent: 'quack', createdAt: T0 });
      assert.deepEqual(await store.getSession('k'), { key: 'k', agent: 'quack', state: 'idle', createdAt: T0, lastActivity: T0, cost: usd(0n), tokens: zero, pendingAsks: 0 });
      await store.setTitle(event('title', 2), { key: 'k', title: 'Default' });
      await store.transitionSession(event('r', 3), { key: 'k', to: 'running' });
      await store.openAsk(event('o1', 4), { key: 'k', id: 'a1', type: 'approval', createdAt: T0 + 5, toolCall: { tool: 'merge', args: { pr: 1 } } });
      await store.openAsk(event('o2', 5), { key: 'k', id: 'q1', type: 'question', createdAt: T0 + 3, prompt: { question: 'Which?', options: ['x', 'y'], freeText: true } });
      assert.deepEqual([(await store.getSession('k'))?.pendingAsks, (await store.getSession('k'))?.oldestAskAt], [2, T0 + 3]);
      const resolved = await store.resolveAsk(event('res', 6), { key: 'k', id: 'q1', by: { kind: 'tailnet' }, choice: 'x', text: 'because', resolvedAt: T0 + 9 });
      assert.deepEqual(resolved, { applied: true, ask: { session: 'k', id: 'q1', type: 'question', state: 'resolved', createdAt: T0 + 3, resolvedAt: T0 + 9, resolution: { by: { kind: 'tailnet' }, choice: 'x', text: 'because' }, prompt: { question: 'Which?', options: ['x', 'y'], freeText: true } } });
      await store.cancelAsk(event('can', 7), { key: 'k', id: 'a1' });
      assert.equal((await store.getAsk('k', 'a1'))?.state, 'cancelled');
      assert.deepEqual((await store.getAsk('k', 'a1'))?.toolCall, { tool: 'merge', args: { pr: 1 } }, 'a closed approval keeps its arguments');
      await store.recordSpend(event('spend', 8), { key: 'k', cost: usd(1_250_000n), tokens: { input: 5, output: 6, cacheRead: 7, cacheWrite: 8 } });
      await store.recordRunOutcome(event('end', 9), { key: 'k', outcome: 'failed', endedAt: T0 + 50, failedSteps: [{ step: 's', error: 'e' }], lastMessage: 'last words' });
      await store.transitionSession(event('idle', 10), { key: 'k', to: 'idle' });
      await store.sealArtifact(event('art', 11), { key: 'k', kind: 'review', version: 1, hash: 'h', content: 'body', sealedAt: T0 + 40, summary: 'sum' });
      await store.recordArtifactDelivery(event('delivered:1', 12), { key: 'k', kind: 'review', version: 1, where: 'github', link: 'https://x/1', at: T0 + 41 });
      await store.recordArtifactDelivery(event('delivered:1', 13), { key: 'k', kind: 'review', version: 1, where: 'github', link: 'https://x/1b', at: T0 + 42 });
      await store.recordArtifactDelivery(event('delivered:2', 14), { key: 'k', kind: 'review', version: 1, where: 'board', link: 'https://x/2', at: T0 + 43 });
      await store.recordArtifactDelivery(event('delivered:3', 15), { key: 'k', kind: 'review', version: 9, where: 'board', link: 'https://x/3', at: T0 + 44 });
      await store.setTitle(action('rename'), { key: 'k', title: 'Mine' });
      assert.deepEqual(await store.getSession('k'), {
        key: 'k', agent: 'quack', title: 'Mine', state: 'idle', createdAt: T0, lastActivity: T0 + 50, cost: usd(1_250_000n), tokens: { input: 5, output: 6, cacheRead: 7, cacheWrite: 8 },
        pendingAsks: 0, lastRun: { outcome: 'failed', endedAt: T0 + 50, failedSteps: [{ step: 's', error: 'e' }] }, lastMessage: 'last words',
      });
      await store.setTitle(action('restore'), { key: 'k', title: null });
      assert.equal((await store.getSession('k'))?.title, 'Default', 'a null rename restores the default');
      assert.deepEqual((await snapshot(store, 'k')).artifacts, [{
        session: 'k', kind: 'review', version: 1, hash: 'h', content: 'body', sealedAt: T0 + 40, summary: 'sum',
        deliveries: [{ where: 'github', link: 'https://x/1', at: T0 + 41 }, { where: 'board', link: 'https://x/2', at: T0 + 43 }],
      }], 'a delivery appends once per event id, and one for an absent version is a no-op');
      await store.advanceWatermark(event('ext', 16), { key: 'k' });
      assert.deepEqual((await snapshot(store, 'k')).session, await store.getSession('k'));
      assert.equal(await store.watermark('k'), 16, 'an event the catalog ignores still moves the watermark');
      assert.equal(await store.watermark('k'), 16, 'action writes never move the watermark');
      assert.equal(await store.getSession('nope'), undefined);
      assert.equal(await store.watermark('nope'), undefined);
    });

    test('(2) replay is a no-op, and a same-id event at a higher seq supersedes once', async () => {
      const { store, catalog } = await fresh();
      const s = stream('k');
      s.next('title.set', { title: 'first' }, 'title');
      s.next('spend.recorded', { cost: usd(10n), tokens: { ...zero, input: 1 } }, 'spend');
      s.next('ask.opened', { id: 'a', type: 'approval', createdAt: T0, toolCall: { tool: 't', args: null } });
      await catalog.publish('k', s.events);
      const once = await snapshot(store, 'k');
      assert.deepEqual(await catalog.publish('k', s.events), { acked: 4 });
      assert.deepEqual(await snapshot(store, 'k'), once, 'a replayed batch changes nothing');
      s.next('title.set', { title: 'second' }, 'title');
      s.next('spend.recorded', { cost: usd(20n), tokens: { ...zero, input: 2 } }, 'spend');
      await catalog.publish('k', s.events.slice(4));
      await store.setTitle(event('title', 2), { key: 'k', title: 'stale' });
      const row = await store.getSession('k');
      assert.deepEqual([row?.title, row?.cost.amountMicros, row?.tokens.input, await store.watermark('k')], ['second', 20n, 2, 6]);
      await store.archiveSession(action('x1'), { key: 'k' }).then(() => store.transitionSession(event('idle', 7), { key: 'k', to: 'idle' }));
      await store.archiveSession(action('x1'), { key: 'k' });
      assert.equal((await store.getSession('k'))?.state, 'idle', 'a repeated action id is a no-op');
      const quiet = await snapshot(store, 'k');
      for (const echo of ['stopped', 'archived'] as const) s.next('state.changed', { state: echo }, `state:echo:${echo}`);
      s.next('judge.failed', { kind: 'review', version: 1 } as never, 'judge:failed:1');
      assert.deepEqual(await catalog.publish('k', s.events.slice(-3)), { acked: s.seq }, 'echoes and extension events are applied past');
      assert.deepEqual({ ...(await snapshot(store, 'k')), watermark: quiet.watermark }, quiet, 'and change no row and no count');
      const asked = await store.resolveAsk(action('r1'), { key: 'k', id: 'a', by: { kind: 'person', id: 'p' }, choice: 'approve', resolvedAt: T0 + 1 });
      assert.equal(asked.applied, true);
      assert.deepEqual(await store.resolveAsk(action('r1'), { key: 'k', id: 'a', by: { kind: 'person', id: 'q' }, choice: 'reject', resolvedAt: T0 + 2 }), { applied: false, ask: asked.ask });
    });

    test('(3) the state machine applies, no-ops and refuses as documented; a refusal changes nothing', async () => {
      const { store } = await fresh();
      await store.migrate();
      const at = async (key: string, state: 'running' | 'idle' | 'stopped' | 'archived') => {
        await store.createSession(event(`c-${key}`, 1), { key, agent: 'a', createdAt: T0 });
        if (state !== 'idle') await store.transitionSession(event('r', 2), { key, to: 'running' });
        if (state === 'stopped') await store.transitionSession(action(`stop-${key}`), { key, to: 'stopped' });
        if (state === 'idle' || state === 'archived') await store.transitionSession(event('i', 3), { key, to: 'idle' });
        if (state === 'archived') await store.archiveSession(action(`arch-${key}`), { key });
        assert.equal((await store.getSession(key))?.state, state);
      };
      type Move = (key: string) => Promise<{ applied: boolean; state?: string }>;
      const moves: Record<string, Move> = {
        'event running': (key) => store.transitionSession(event('m', 10), { key, to: 'running' }),
        'event idle': (key) => store.transitionSession(event('m', 10), { key, to: 'idle' }),
        'action stopped': (key) => store.transitionSession(action('m'), { key, to: 'stopped' }),
        archive: (key) => store.archiveSession(action('m'), { key }),
        unarchive: (key) => store.unarchiveSession(action('m'), { key }),
      };
      // [from, move, resulting state, applied]
      const table: Array<[string, string, string, boolean]> = [
        ['running', 'event running', 'running', false], ['running', 'event idle', 'idle', true], ['running', 'action stopped', 'stopped', true], ['running', 'archive', 'running', false], ['running', 'unarchive', 'running', false],
        ['idle', 'event running', 'running', true], ['idle', 'event idle', 'idle', false], ['idle', 'action stopped', 'stopped', true], ['idle', 'archive', 'archived', true], ['idle', 'unarchive', 'idle', false],
        ['stopped', 'event running', 'running', true], ['stopped', 'event idle', 'idle', true], ['stopped', 'action stopped', 'stopped', false], ['stopped', 'archive', 'archived', true], ['stopped', 'unarchive', 'stopped', false],
        ['archived', 'event running', 'running', true], ['archived', 'event idle', 'archived', false], ['archived', 'action stopped', 'archived', false], ['archived', 'archive', 'archived', false], ['archived', 'unarchive', 'stopped', true],
      ];
      for (const [i, [from, move, to, applied]] of table.entries()) {
        const key = `s${i}`;
        await at(key, from as 'idle');
        const before = await store.getSession(key);
        const out = await moves[move]!(key);
        assert.deepEqual(out, { applied, state: to }, `${from} → ${move}`);
        if (!applied) assert.deepEqual(await store.getSession(key), before, `${from} → ${move} changes nothing`);
      }
      const before = await snapshot(store, 's0');
      assert.deepEqual(await store.recordSpend(event('sp', 20), { key: 's0', cost: { currencyCode: 'EUR', amountMicros: 5n }, tokens: zero }), { applied: false, refused: 'currency' });
      assert.deepEqual(await snapshot(store, 's0'), before, 'a refused spend holds the row and the watermark');
      assert.equal(await store.deleteSession(action('del'), { key: 's0' }), true);
      assert.deepEqual(await store.archiveSession(action('late'), { key: 's0' }), { applied: false }, 'an action on a tombstoned key is refused');
      await store.setTitle(action('late2'), { key: 's0', title: 'x' });
      await store.openAsk(event('late3', 30), { key: 's0', id: 'z', type: 'approval', createdAt: T0 });
      assert.deepEqual(await snapshot(store, 's0'), { session: undefined, watermark: undefined, asks: [], artifacts: [] }, 'nothing lands under a tombstone');

      await at('turns', 'running');
      await store.openAsk(event('o', 4), { key: 'turns', id: 'q', type: 'question', createdAt: T0 + 1, prompt: { question: 'Q?', options: ['y'], freeText: false } });
      await store.recordSpend(event('sp', 5), { key: 'turns', cost: usd(7n), tokens: { ...zero, input: 3 } });
      await store.recordRunOutcome(event('run:1:ended', 6), { key: 'turns', outcome: 'failed', endedAt: T0 + 10, failedSteps: [{ step: 's', error: 'e' }], lastMessage: 'm' });
      const prior = await snapshot(store, 'turns');
      await store.recordTurn(event('run:2:turn:1:ended', 7), { key: 'turns', at: T0 + 20 });
      assert.deepEqual(await snapshot(store, 'turns'), { ...prior, session: { ...prior.session!, lastActivity: T0 + 20 }, watermark: 7 }, 'recordTurn moves lastActivity and nothing else');
      // A redelivery at or below the watermark is a no-op, whatever it carries.
      await store.recordTurn(event('run:2:turn:1:ended', 7), { key: 'turns', at: T0 + 15 });
      assert.equal((await store.getSession('turns'))?.lastActivity, T0 + 20, 'a redelivered turn neither applies twice nor regresses');
      await store.recordRunOutcome(event('run:2:ended', 8), { key: 'turns', outcome: 'succeeded', endedAt: T0 + 30, failedSteps: [] });
      await store.recordTurn(event('run:3:turn:1:ended', 9), { key: 'turns', at: T0 + 40 });
      assert.deepEqual([(await store.getSession('turns'))?.lastActivity, (await store.getSession('turns'))?.lastRun?.endedAt], [T0 + 40, T0 + 30], 'the later of a run end and a turn end is the row\'s');
    });

    test('(4) a lifecycle write is one point of consistency: no torn row is visible', async () => {
      const { store, catalog } = await fresh();
      const s = stream('k');
      await catalog.publish('k', s.events);
      const writes: Promise<unknown>[] = [];
      for (let n = 1; n <= 40; n++) {
        s.next('ask.opened', { id: `a${n}`, type: 'approval', createdAt: T0 + n, toolCall: { tool: 't', args: n } });
        s.next('spend.recorded', { cost: usd(BigInt(n * 10)), tokens: { ...zero, input: n } }, 'spend');
        s.next('run.ended', { run: n, outcome: 'succeeded', failedSteps: [] }, `run:${n}:ended`, T0 + n * 7);
        if (n % 2) s.next('ask.cancelled', { id: `a${n}`, run: n });
        writes.push(catalog.publish('k', s.events.slice(-(n % 2 ? 4 : 3))));
      }
      const torn: string[] = [];
      const reads = (async () => {
        for (let i = 0; i < 60; i++) {
          const row = await store.getSession('k');
          if (!row) continue;
          if ((row.pendingAsks > 0) !== (row.oldestAskAt !== undefined)) torn.push(`counts ${row.pendingAsks}/${row.oldestAskAt}`);
          if (row.lastRun && row.lastRun.endedAt !== row.lastActivity) torn.push('run outcome');
          if (row.cost.amountMicros !== BigInt(row.tokens.input * 10)) torn.push('spend');
        }
      })();
      await Promise.all([...writes, reads]);
      assert.deepEqual(torn, []);
      const row = await store.getSession('k');
      assert.deepEqual([row?.pendingAsks, row?.oldestAskAt, row?.cost.amountMicros, row?.lastActivity], [20, T0 + 2, 400n, T0 + 280]);
    });

    test('(5) every order and a battery of ASTs page to the end exactly as the reference, stable under interleaved writes', async () => {
      const { store, catalog } = await fresh();
      const keys = await corpus(catalog);
      const sessions = (await Promise.all(keys.map((k) => store.getSession(k)))).filter((r) => !!r);
      const asks = (await Promise.all(keys.map((k) => snapshot(store, k)))).flatMap((s) => s.asks);
      const artifacts = (await Promise.all(keys.map((k) => snapshot(store, k)))).flatMap((s) => s.artifacts);
      const battery: Array<[Listed, string]> = [
        ['sessions', ''], ['sessions', String.raw`key:sleeper\:platinum\:*`], ['sessions', `key:${escapeTerm('review:monorepo/4')}`], ['sessions', String.raw`key:a\/b\#1?@*`],
        ['sessions', 'agent:quack'], ['sessions', 'agent:qu*'], ['sessions', 'agent:q?ack'], ['sessions', 'title:"Lineup check matchday 3"'], ['sessions', 'title:Review*'], ['sessions', 'title:*ünï*'],
        ['sessions', 'state:running'], ['sessions', '-state:archived'], ['sessions', 'state:idle OR state:stopped'], ['sessions', '"2026-11-01T00:00:30Z"'],
        ['sessions', 'createdAt:>="2026-11-01T00:00:00Z"'], ['sessions', 'lastActivity:>"2026-11-01T00:02:00Z"'], ['sessions', 'lastActivity:["2026-11-01T00:01:00Z" TO "2026-11-01T00:04:00Z"}'],
        ['sessions', 'cost.currencyCode:USD'], ['sessions', 'cost.amountMicros:>=10000000'], ['sessions', `cost.amountMicros:${BIG}`], ['sessions', 'cost.amountMicros:{0 TO 9007199254740993]'],
        ['sessions', 'tokens.input:<100'], ['sessions', 'tokens.output:>3'], ['sessions', 'tokens.cacheRead:0'], ['sessions', 'tokens.cacheWrite:1'],
        ['sessions', 'pendingAsks:>0'], ['sessions', 'pendingAsks:0'], ['sessions', 'oldestAskAt:<="2026-11-01T00:00:00.200Z"'], ['sessions', 'lastRun.outcome:failed'], ['sessions', '-lastRun.outcome:succeeded'],
        ['sessions', 'lineup'], ['sessions', 'rotation'], ['sessions', '"matchday 3"'], ['sessions', 'mono*'], ['sessions', 'st?rters'], ['sessions', 'café'], ['sessions', 'review AND NOT pendingAsks:0'],
        ['asks', ''], ['asks', 'state:pending'], ['asks', 'type:approval'], ['asks', String.raw`session:sleeper\:*`], ['asks', 'tool:merge?pr'], ['asks', 'tool:submit*'], ['asks', 'resolvedAt:>"2026-11-01T00:00:00Z"'],
        ['asks', 'createdAt:["2026-11-01T00:00:00Z" TO "2026-11-01T00:00:00.150Z"]'], ['asks', 'starter'], ['asks', '"tired starter"'], ['asks', 'merge'], ['asks', '-state:pending'],
        ['artifacts', ''], ['artifacts', 'kind:sleeper.cards'], ['artifacts', 'kind:review*'], ['artifacts', 'version:1'], ['artifacts', 'version:>1'], ['artifacts', 'sealedAt:>="2026-11-01T00:00:02Z"'],
        ['artifacts', String.raw`session:a\/b*`], ['artifacts', 'session:*monorepo*'], ['artifacts', 'sealedAt:<"2026-11-01T00:00:03Z"'], ['artifacts', 'approve'], ['artifacts', '"starter lineup"'], ['artifacts', 'match*'],
      ];
      const orders = { sessions: ['lastActivity:newest', 'createdAt:newest', 'oldestAskAt:oldest'], asks: ['createdAt:newest', 'createdAt:oldest'], artifacts: ['sealedAt:newest'] };
      const rows = { sessions, asks, artifacts };
      const listOf = { sessions: store.listSessions.bind(store), asks: store.listAsks.bind(store), artifacts: store.listArtifacts.bind(store) } as Record<Listed, (o: never) => Promise<{ rows: object[]; cursor?: string }>>;
      for (const [list, q] of battery) {
        for (const o of orders[list]) {
          const [field, direction] = o.split(':');
          const where = parseQuery(list, q);
          const want = reference(list, rows[list] as never, where, { field: field!, direction: direction! });
          const got = await drain((cursor) => listOf[list]({ where, orderBy: { field, direction }, pageSize: 3, cursor } as never), (r) => JSON.stringify(r, (_k, v) => (typeof v === 'bigint' ? `${v}` : v)));
          assert.deepEqual(got, want, `${list} q=${q} orderBy=${o}`);
        }
      }
      // Rows that exist before paging starts and keep their position all appear exactly once.
      const seen: string[] = [];
      let cursor: string | undefined;
      let n = 0;
      do {
        const page = await store.listSessions({ where: { kind: 'all' }, orderBy: { field: 'createdAt', direction: 'newest' }, pageSize: 4, cursor });
        seen.push(...page.rows.map((r) => r.key));
        cursor = page.cursor;
        const s = stream(`late-${n++}`);
        await catalog.publish(s.key, s.events);
        await catalog.rename(keys[n]!, `renamed ${n}`);
      } while (cursor);
      assert.deepEqual(seen.filter((k) => !k.startsWith('late-')).sort(), [...keys].sort());
    });

    test('(6) a cursor is bound to the query and order that issued it', async () => {
      const { store, catalog } = await fresh();
      await corpus(catalog);
      const where = parseQuery('sessions', 'state:running');
      const page = await store.listSessions({ where, orderBy: { field: 'lastActivity', direction: 'newest' }, pageSize: 2 });
      assert.ok(page.cursor);
      const refused = (o: Partial<Parameters<SessionCatalogStore['listSessions']>[0]>) =>
        assert.rejects(store.listSessions({ where, orderBy: { field: 'lastActivity', direction: 'newest' }, pageSize: 2, cursor: page.cursor, ...o }), CursorError);
      await refused({ where: parseQuery('sessions', 'state:idle') });
      await refused({ orderBy: { field: 'createdAt', direction: 'newest' } });
      await refused({ cursor: `${page.cursor}x` });
      await refused({ cursor: Buffer.from('["x",[1,"k"]]').toString('base64url') });
      await refused({ cursor: 'not base64 at all!' });
      await assert.rejects(store.listAsks({ where: { kind: 'all' }, orderBy: { field: 'createdAt', direction: 'oldest' }, pageSize: 2, cursor: page.cursor }), CursorError);
    });

    test('(7) bare terms and phrases search exactly the documented fields', async () => {
      const { store, catalog } = await fresh();
      await corpus(catalog);
      const keysOf = async (q: string) => (await store.listSessions({ where: parseQuery('sessions', q), orderBy: { field: 'createdAt', direction: 'newest' }, pageSize: 1000 })).rows.map((r) => r.key).sort();
      assert.deepEqual(await keysOf('quack'), [], 'agent is not a full-text field');
      assert.deepEqual(await keysOf('platinum'), (await keysOf(String.raw`key:sleeper\:*`)), 'the key is');
      assert.ok((await keysOf('rotation')).length > 0, 'the last message is');
      assert.deepEqual(await keysOf('"lineup check"'), await keysOf('title:Lineup*'), 'a phrase matches adjacent tokens of the title');
      assert.deepEqual(await keysOf('"check lineup"'), [], 'and only in order');
      const asks = async (q: string) => (await store.listAsks({ where: parseQuery('asks', q), orderBy: { field: 'createdAt', direction: 'oldest' }, pageSize: 1000 })).rows.length;
      assert.deepEqual([await asks('platinum'), await asks('keep')], [0, 0], 'tool arguments and options are not searched');
      const artifacts = async (q: string) => (await store.listArtifacts({ where: parseQuery('artifacts', q), orderBy: { field: 'sealedAt', direction: 'newest' }, pageSize: 1000 })).rows.length;
      assert.deepEqual([await artifacts('content'), await artifacts('revise') > 0], [0, true], 'content is not searched; the summary is');
    });

    test('(8) money stays integer', async () => {
      const { store, catalog } = await fresh();
      const keys = await corpus(catalog);
      const big = await store.getSession(keys[5]!);
      assert.equal(big?.cost.amountMicros, BIG);
      const exact = await store.listSessions({ where: parseQuery('sessions', `cost.amountMicros:${BIG}`), orderBy: { field: 'createdAt', direction: 'newest' }, pageSize: 10 });
      assert.deepEqual(exact.rows.map((r) => r.key), [keys[5]], 'compared exactly, not as a float');
      const below = await store.listSessions({ where: parseQuery('sessions', `cost.amountMicros:>${BIG - 1n}`), orderBy: { field: 'createdAt', direction: 'newest' }, pageSize: 10 });
      assert.deepEqual(below.rows.map((r) => r.key), [keys[5]]);
      const wm = await store.watermark(keys[5]!);
      assert.deepEqual(await store.recordSpend(event('spend', wm! + 1), { key: keys[5]!, cost: usd(BIG - 1n), tokens: zero }), { applied: false, refused: 'regressed' });
      assert.equal(await store.watermark(keys[5]!), wm);
      const all = (await store.listSessions({ where: { kind: 'all' }, orderBy: { field: 'createdAt', direction: 'newest' }, pageSize: 1000 })).rows;
      assert.equal(all.reduce((sum, r) => sum + r.cost.amountMicros, 0n), BIG + 1_250_000n * BigInt([...Array(24).keys()].filter((i) => i !== 5).reduce((a, b) => a + b, 0)));
    });

    test('(9) rebuild replays the log and the journal back to the live rows; a journal in the session directory is ignored', async () => {
      const { store, catalog, dataDir } = await fresh();
      const s = stream('k');
      s.next('title.set', { title: 'derived' }, 'title');
      s.next('run.started', { run: 1 });
      s.next('ask.opened', { id: 'a1', type: 'approval', createdAt: T0 + 1, toolCall: { tool: 'merge', args: { pr: 1 } } });
      s.next('ask.opened', { id: 'q1', type: 'question', createdAt: T0 + 2, prompt: { question: 'Q?', options: ['y', 'n'], freeText: false } });
      s.next('artifact.sealed', { kind: 'review', version: 1, hash: 'h1', content: 'c1' }, 'artifact:review:1');
      s.next('artifact.delivered', { kind: 'review', version: 1, where: 'github', link: 'https://x/1', at: T0 + 3 }, 'delivered:review:1');
      s.next('judge.cleared', { kind: 'review', version: 1 } as never, 'judge:cleared:1');
      s.next('spend.recorded', { cost: usd(5n), tokens: { ...zero, input: 1 } }, 'spend');
      await catalog.publish('k', s.events);
      await catalog.archive('k');
      await catalog.resolve('k', 'a1', { by: { kind: 'person', id: 'jason' }, choice: 'approve', reason: 'fine' });
      await catalog.rename('k', 'mine');
      s.next('run.ended', { run: 1, outcome: 'succeeded', failedSteps: [], lastMessage: 'bye' }, 'run:1:ended');
      s.next('ask.cancelled', { id: 'q1', run: 1 });
      s.next('state.changed', { state: 'idle' });
      await catalog.publish('k', s.events.slice(-3));
      await catalog.archive('k');
      await catalog.unarchive('k');
      s.next('run.started', { run: 2 });
      s.next('artifact.sealed', { kind: 'review', version: 1, hash: 'h1', content: 'c1', summary: 'filled' }, 'artifact:review:1');
      s.next('state.changed', { state: 'stopped' }, 'state:echo');
      s.next('spend.recorded', { cost: usd(9n), tokens: { ...zero, input: 3 } }, 'spend');
      await catalog.publish('k', s.events.slice(-4));
      await catalog.stopped('k');
      await catalog.rename('k', null);
      const live = await snapshot(store, 'k');
      assert.equal(live.session?.state, 'stopped');
      // A session can write only its own directory; a journal it plants there must not replay.
      const sessionDir = join(dataDir, 'sessions', 'k');
      await mkdir(sessionDir, { recursive: true });
      await writeFile(join(sessionDir, 'catalog-actions.jsonl'), `${JSON.stringify({ id: 'forged', method: 'archiveSession', args: { key: 'k' }, watermark: 99 })}\n`);
      await catalog.rebuild('k', s.events);
      assert.deepEqual(await snapshot(store, 'k'), live);
      // A delete replays too: the rebuilt key is tombstoned, so a late flush recreates nothing.
      await catalog.remove('k');
      await catalog.rebuild('k', s.events);
      assert.deepEqual(await snapshot(store, 'k'), { session: undefined, watermark: undefined, asks: [], artifacts: [] });
      assert.deepEqual(await catalog.publish('k', s.events), { acked: 0 });
      assert.equal(await store.getSession('k'), undefined);
    });

    test('(10) delete tombstones an incarnation: a late flush recreates nothing, and a new session under the key starts fresh', async () => {
      const { store, catalog } = await fresh();
      const old = stream('k', 'incarnation-1');
      old.next('title.set', { title: 'old' });
      old.next('ask.opened', { id: 'a', type: 'approval', createdAt: T0, toolCall: { tool: 't', args: {} } });
      await catalog.publish('k', old.events);
      assert.equal(await catalog.remove('k'), true);
      assert.equal(await catalog.remove('k'), false, 'a second delete finds nothing');
      assert.deepEqual(await catalog.publish('k', old.events), { acked: 0 }, 'the stopping process flushes its whole log again');
      await catalog.rename('k', 'ghost');
      assert.deepEqual(await snapshot(store, 'k'), { session: undefined, watermark: undefined, asks: [], artifacts: [] });
      const fresh2 = stream('k', 'incarnation-2');
      await catalog.publish('k', fresh2.events);
      assert.deepEqual(await store.getSession('k'), { key: 'k', agent: 'quack', state: 'idle', createdAt: T0, lastActivity: T0, cost: usd(0n), tokens: zero, pendingAsks: 0 });
    });
  });
}
