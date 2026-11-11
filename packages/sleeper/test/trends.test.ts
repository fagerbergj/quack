import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, type FauxResponseStep } from '@earendil-works/pi-ai';
import { createSchedulesService } from '@quack/schedules';
import { Artifacts, primeSessionEndpoint, serveApp } from '@quack/sdk';
import { readSession } from '@quack/eval';
import { replayed } from './replayed.ts';
import { openSession } from '@quack/session';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { runWorkflow, workflowExtension } from '@quack/workflows';
import {
  buildSnapshot, buildTrendsCard, trendsFromEnv, buildTrendsFacts, createSleeperClient, createSleeperService, registerTrendsSchedules, trendsEvidence, trendsKey, trendsRubric, trendsWorkflow,
  type TrendsCard, type TrendsFacts,
} from '../src/index.ts';
import { MAX_SEARCHED } from '../src/trends/facts.ts';
import type { TrendsDecision } from '../src/trends/card.ts';
import { factsPrompt } from '../src/trends/workflow.ts';

type Fixtures = Record<string, any>;
const lineup = JSON.parse(readFileSync(new URL('./fixtures/lineup.json', import.meta.url), 'utf8')) as Fixtures;
const AT = Date.parse('2026-11-06T15:00:00Z');
const INPUT = { league: 'L1', week: 9, owner: 'mallard' };
const [TODAY, YESTERDAY] = ['2026-11-06', '2026-11-05'];

// Yesterday Rob Rowe was healthy and Will Wing second on the depth chart; 400 is an unrostered back on the rise, 401 a retired one.
const dumpOn = (day: 'today' | 'yesterday') => ({
  ...lineup['/v1/players/nfl'],
  '102': { ...lineup['/v1/players/nfl']['102'], injury_status: day === 'today' ? 'Questionable' : null },
  '104': { ...lineup['/v1/players/nfl']['104'], depth_chart_order: day === 'today' ? 1 : 2 },
  '400': { first_name: 'Fred', last_name: 'Fresh', team: 'KC', position: 'RB', injury_status: null },
  '401': { first_name: 'Rex', last_name: 'Retired', team: null, position: 'RB', injury_status: null },
  '402': { first_name: 'Lou', last_name: 'Lineman', team: 'KC', position: 'OL', injury_status: null },
});
const fixturesOn = (day: 'today' | 'yesterday'): Fixtures => ({
  ...lineup,
  '/v1/players/nfl': dumpOn(day),
  '/v1/players/nfl/trending/add': [{ player_id: '100', count: 900 }, { player_id: '401', count: 800 }, { player_id: '402', count: 700 }, { player_id: '400', count: 600 }],
});
const clientOf = (fixtures: Fixtures) => createSleeperClient({
  fetch: (async (input: string) => {
    const path = new URL(input).pathname;
    return path in fixtures ? new Response(JSON.stringify(fixtures[path])) : new Response('null');
  }) as unknown as typeof fetch,
  now: () => AT,
});
const row = async (day: 'today' | 'yesterday') => ({ league: 'L1', date: day === 'today' ? TODAY : YESTERDAY, takenAt: day === 'today' ? '2026-11-06T09:00:00Z' : '2026-11-05T09:00:00Z', data: await buildSnapshot(clientOf(fixturesOn(day)), 'L1') });
const history = async () => [{ date: TODAY, takenAt: '' }, { date: YESTERDAY, takenAt: '' }];
const snapshotOn = async (_league: string, date: string) => (date === YESTERDAY ? row('yesterday') : row('today'));

const item = { title: 'Rowe limited at practice', url: 'https://www.nfl.com/n', date: new Date(AT - 5 * 3_600_000).toISOString() };
const factsOf = async (news?: Parameters<typeof buildTrendsFacts>[0]['news'], past = history) =>
  buildTrendsFacts({ client: clientOf(fixturesOn('today')), history: past, snapshotOn, news }, await row('today'), INPUT, AT);

const decision: TrendsDecision = {
  items: [
    { kind: 'injury', playerId: '102', newsUrl: item.url, source: 'Ian Rapoport via NBC Sports', text: 'Rob Rowe is Questionable on Sleeper, but a report dated today has him limited; following the newer report.' },
    { kind: 'news', playerId: '104', text: 'Will Wing moved up to first on the depth chart.' },
    { kind: 'waiver', playerId: '400', text: 'Fred Fresh is the most added free agent no team here holds.' },
  ],
  source_note: 'Rob Rowe: Sleeper says Questionable, the newer report says limited; followed the report.',
};

test('facts diff today against the last earlier snapshot and list unrostered free agents on the rise', async () => {
  const f = await factsOf();
  assert.deepEqual([f.date, f.since], [TODAY, YESTERDAY]);
  assert.deepEqual(f.changes, [{ playerId: '102', field: 'injury', from: null, to: 'Questionable' }, { playerId: '104', field: 'depth', from: 2, to: 1 }]);
  assert.deepEqual(f.trending, [{ playerId: '400', adds: 600 }], 'a rostered player, a retired one and a lineman are not free agents on the rise');
  assert.deepEqual(f.players['400'], { id: '400', name: 'Fred Fresh', pos: 'RB', team: 'KC', inj: null, depth: null });
  assert.deepEqual(Object.keys(f.news), []);
});

test('the first snapshot has nothing to diff, and a card says so', async () => {
  const f = await factsOf(undefined, async () => [{ date: TODAY, takenAt: '' }]);
  assert.deepEqual([f.since, f.changes], [null, []]);
  assert.equal(buildTrendsCard(f, { items: [], source_note: 'No source conflicts.' }).summary, 'First snapshot: nothing to compare with yet.');
});

test('news is searched for players whose fields moved, at most MAX_SEARCHED of them', async () => {
  const searched: string[] = [];
  const f = await factsOf(async (p) => (searched.push(p.name), [item]));
  assert.deepEqual(searched.sort(), ['Rob Rowe', 'Will Wing']);
  assert.deepEqual(Object.keys(f.news).sort(), ['102', '104']);
  assert.ok(MAX_SEARCHED >= 1);
  const many = { players: Object.fromEntries(Array.from({ length: 15 }, (_, i) => [`p${i}`, { name: `P${i}`, injury: 'Out' }])) };
  const prev = { players: Object.fromEntries(Object.keys(many.players).map((id) => [id, { name: id, injury: null }])) };
  const seen: string[] = [];
  const deps = { client: clientOf(fixturesOn('today')), history, snapshotOn: async () => ({ data: prev }), news: async (p: { name: string }) => (seen.push(p.name), []) };
  await buildTrendsFacts(deps, { date: TODAY, data: { ...many, rosters: [] } }, INPUT, AT);
  assert.equal(seen.length, MAX_SEARCHED);
});

test('the card takes times, sources and links from the facts, and Sleeper-only items are sourced to Sleeper', async () => {
  const c = buildTrendsCard(await factsOf(async () => [item]), decision);
  assert.deepEqual(c.items.map((i) => [i.time, i.source, i.kind, i.link, i.player?.name]), [
    ['2026-11-06 (Fri)', 'Ian Rapoport via NBC Sports', 'injury', item.url, 'Rob Rowe'],
    ['2026-11-06 (Fri)', 'Sleeper', 'news', undefined, 'Will Wing'],
    ['2026-11-06 (Fri)', 'Sleeper', 'waiver', undefined, 'Fred Fresh'],
  ]);
  assert.equal(c.summary, '3 items since 2026-11-05.');
});

test('a bad decision fails the card step instead of reaching the judge', async () => {
  const f = await factsOf(async () => [item]);
  const one = decision.items[0];
  const build = (i: Partial<typeof one>) => buildTrendsCard(f, { ...decision, items: [{ ...one, ...i }] });
  assert.throws(() => build({ playerId: '999' }), /not in the facts/);
  assert.throws(() => build({ newsUrl: 'https://www.nfl.com/other' }), /that link is not news for 102 Rob Rowe/);
  assert.throws(() => buildTrendsCard({ ...f, news: { ...f.news, '103': [{ ...item, url: 'https://x.test/other' }] }, players: { ...f.players, '103': { ...f.players['102'], id: '103', name: 'Rob Rowan' } } }, { ...decision, items: [{ ...one, newsUrl: 'https://x.test/other' }] }), /that link is news for 103 Rob Rowan, not 102 Rob Rowe/);
  assert.throws(() => build({ source: undefined }), /for 102 Rob Rowe without naming its source/);
  assert.throws(() => build({ playerId: undefined }), /without naming a player/);
});

const gate = async (id: string, f: TrendsFacts, c: unknown) => {
  const check = trendsRubric.criteria.flatMap((k) => k.checks).find((k) => k.id === id)!;
  const content = JSON.stringify(c);
  return check.code!(content, await trendsEvidence(f)(null, { content }));
};

test('each code gate passes the good card', async () => {
  const f = await factsOf(async () => [item]);
  const c = buildTrendsCard(f, decision);
  for (const id of ['item_shape', 'no_bare_ids']) assert.equal((await gate(id, f, c)).met, 1, id);
});

test('item_shape fails a time, source or sentence past its cap, an empty one, and an unknown kind', async () => {
  const f = await factsOf(async () => [item]);
  const c = buildTrendsCard(f, decision);
  const with0 = (patch: object) => ({ ...c, items: [{ ...c.items[0], ...patch }, ...c.items.slice(1)] });
  for (const patch of [{ time: 'x'.repeat(41) }, { source: 'x'.repeat(61) }, { text: 'x'.repeat(161) }, { text: '' }, { kind: 'rumor' }]) {
    const r = await gate('item_shape', f, with0(patch));
    assert.ok(r.met < 1, JSON.stringify(patch).slice(0, 30));
    assert.match(r.detail!, /miss a time, source/);
  }
});

test('no_bare_ids fails an item whose text carries a player id, not one with an add count or a name', async () => {
  const f = await factsOf(async () => [item]);
  const c = buildTrendsCard(f, decision);
  const text = (t: string) => gate('no_bare_ids', f, { ...c, items: [{ ...c.items[0], text: t }, ...c.items.slice(1)] });
  assert.ok((await text('102 Rob Rowe is Questionable.')).met < 1);
  assert.match((await text('Rob Rowe (102) is Questionable.')).detail!, /raw player id/);
  for (const ok of ['Rob Rowe has 1,102 yards and 4102 adds.', 'Rob Rowe is Questionable.', 'Rob Rowe has 1023 adds.']) assert.equal((await text(ok)).met, 1, ok);
});

test('the prompt gives the model each id in a lookup, never beside a name in the changes', async () => {
  const f = await factsOf(async () => [item]);
  const p = factsPrompt(f);
  const [body, lookup] = p.split('Values for `playerId`:');
  assert.ok(!/\b10[24]\b|\b400\b/.test(body), 'ids stay out of the change and rising lines');
  assert.match(lookup, /- Rob Rowe \(.*\): 102\n/);
});

test('the lookup tells apart two players who share name, position and team', async () => {
  const f = await factsOf(async () => [item]);
  const twin = { ...f.players['102'], id: '9102' };
  const p = factsPrompt({ ...f, players: { ...f.players, '9102': twin } });
  assert.match(p, /- Rob Rowe \(.*\) #1: 102\n/);
  assert.match(p, /- Rob Rowe \(.*\) #2: 9102\n?/);
});

test('the judge evidence pairs an item with its designation and news, and picks out the conflicts and sourced rows', async () => {
  const f = await factsOf(async () => [item]);
  const ev = await trendsEvidence(f)(null, { content: JSON.stringify(buildTrendsCard(f, decision)) });
  const first = ev.row[0].state as unknown as { designations: Record<string, string>; news: Array<{ url: string }> };
  assert.deepEqual([first.designations, first.news.map((n) => n.url)], [{ 'Rob Rowe': 'Questionable' }, [item.url]]);
  assert.deepEqual([ev.row.length, ev.conflict.map((u) => u.id), ev.sourced.map((u) => u.id)], [3, ['item1'], ['item1']]);
});

async function runTrends(d: TrendsDecision, news?: Parameters<typeof buildTrendsFacts>[0]['news']) {
  const judged: string[] = [];
  const responses: FauxResponseStep[] = [
    () => fauxAssistantMessage(JSON.stringify(d)),
    ...Array.from({ length: 40 }, () => (c: { messages: Array<{ content: unknown }> }) => {
      const asked = String(c.messages.at(-1)!.content);
      judged.push(asked);
      return fauxAssistantMessage(JSON.stringify({ choice: /aggregator the only source/.test(asked) ? 'no' : 'yes', confidence: 1, reason: 'ok' }));
    }),
  ];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-trends-'));
  const workflow = trendsWorkflow({ client: clientOf(fixturesOn('today')), snapshot: async () => row('today'), history, snapshotOn, now: () => AT, news });
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'You are the trend scout.', extensions: [workflowExtension([workflow])] } });
  try {
    const outcome = await runWorkflow(s.harness, s.root, workflow, JSON.stringify(INPUT));
    assert.equal(outcome.status, 'completed', JSON.stringify(outcome));
    const sealed = (await s.harness.snapshot(Artifacts, s.root.id, ctx))!.kinds['sleeper.cards'].versions[0];
    await s.close();
    return { record: await readSession(dir), sealed, judged, verdict: sealed.verdict as { passed: boolean; rubric: string; gatesFailed: string[]; notes: string[]; checks: Array<{ id: string }> } };
  } finally {
    await s.close();
    await rm(dir, { recursive: true, force: true });
  }
}

test('the workflow seals a trends card; with no news source the news checks are skipped and the verdict says so', async () => {
  const bare: TrendsDecision = { ...decision, items: decision.items.slice(1) };
  const { sealed, verdict, record } = await runTrends(bare);
  await replayed(record, sealed);
  assert.deepEqual((JSON.parse(sealed.content) as TrendsCard).items.map((i) => i.player?.name), ['Will Wing', 'Fred Fresh']);
  assert.deepEqual([verdict.rubric, verdict.gatesFailed, verdict.passed, verdict.notes], ['sleeper.trends@2', [], true, ['source conflicts and original sources not checked: no news source']]);
  assert.deepEqual(verdict.checks.map((k) => k.id).filter((id) => !['item_shape', 'no_bare_ids'].includes(id)), []);
});

test('with a news source the conflict, original-source and aggregator checks are scored', async () => {
  const { sealed, verdict, judged, record } = await runTrends(decision, async () => [item]);
  assert.deepEqual(await replayed(record, sealed), await trendsEvidence(await factsOf(async () => [item]))(null, sealed), 'the recording rebuilds the evidence the judge read');
  assert.equal((JSON.parse(sealed.content) as TrendsCard).items[0].link, item.url);
  assert.deepEqual([verdict.rubric, verdict.gatesFailed, verdict.passed, verdict.notes], ['sleeper.trends@2', [], true, []]);
  assert.deepEqual(verdict.checks.map((k) => k.id).sort(), ['aggregator_only', 'conflict_stated', 'item_shape', 'no_bare_ids', 'original_source_named']);
  assert.ok(judged.some((q) => /disagree, does the item say so/.test(q)));
});

test('the schedules service lists one daily run after the snapshot, replaced by the next week and removed outside the season', async () => {
  const schedules = createSchedulesService({ distributorUrl: 'http://unused', dbPath: ':memory:' });
  const schedulesServer = serveApp(schedules).listen(0);
  await new Promise((r) => schedulesServer.once('listening', r));
  const url = `http://127.0.0.1:${(schedulesServer.address() as AddressInfo).port}`;
  const list = async () => (await (await fetch(url)).json()).schedules;
  const stateOf = (state: object) => createSleeperClient({ fetch: (async () => new Response(JSON.stringify(state))) as unknown as typeof fetch });
  try {
    const regular = (week: number) => stateOf({ week, season: '2026', season_type: 'regular' });
    assert.deepEqual(await registerTrendsSchedules({ url }, regular(9), 'L1', 'mallard'), ['sleeper/trends/L1']);
    await registerTrendsSchedules({ url }, regular(10), 'L1', 'mallard');
    const listed = await list();
    assert.equal(listed.length, 1);
    assert.partialDeepStrictEqual(listed[0], { id: 'sleeper/trends/L1', agent: 'sleeper-trends', cron: '30 4 * * *', tz: 'America/New_York', sessionKey: 'sleeper:L1:10:trends', input: JSON.stringify({ league: 'L1', week: 10, owner: 'mallard' }) });
    assert.deepEqual(await registerTrendsSchedules({ url }, stateOf({ week: 1, season_type: 'off' }), 'L1', 'mallard'), []);
    assert.deepEqual(await list(), []);
    assert.deepEqual(await registerTrendsSchedules({ url }, stateOf({ week: 1, season_type: 'off' }), 'L1', 'mallard'), [], 'nothing to remove is fine');
    assert.equal(trendsKey('L1', 9), 'sleeper:L1:9:trends');
  } finally {
    schedulesServer.close();
    schedules.stop();
  }
});

test('the trends job starts on demand under the week session key, and a snapshot is read by date', async () => {
  const calls: Array<{ url: string; body: { input: string } }> = [];
  const fake = (async (u: string, init: { body: string }) => (calls.push({ url: u, body: JSON.parse(init.body) }), new Response('{}', { status: 202 }))) as unknown as typeof fetch;
  const service = serveApp(createSleeperService({ dbPath: ':memory:', client: clientOf(fixturesOn('today')), distributorUrl: 'http://d', owner: 'mallard', league: 'L1', fetch: fake, now: () => new Date(AT) }));
  await new Promise<void>((r) => service.listen(0, r));
  const call = (path: string, method = 'GET') => fetch(`http://127.0.0.1:${(service.address() as AddressInfo).port}${path}`, { method, headers: { authorization: 'Bearer t' } });
  try {
    const started = await call('/trends', 'POST');
    assert.deepEqual([started.status, await started.json()], [202, { sessionKey: 'sleeper:L1:9:trends', status: 202 }]);
    assert.equal(calls[0].url, 'http://d/sessions/sleeper%3AL1%3A9%3Atrends/invocations?agent=sleeper-trends');
    assert.deepEqual(JSON.parse(calls[0].body.input), INPUT);
    assert.equal((await call('/trends')).status, 405);
    await call('/leagues/L1/snapshots', 'POST');
    assert.equal((await (await call('/leagues/L1/snapshots/2026-11-06')).json() as { date: string }).date, '2026-11-06');
    assert.equal((await call('/leagues/L1/snapshots/2026-11-05')).status, 404);
  } finally {
    service.stop();
  }
});

test('the trends workflow reads the service through the endpoint primed when it was built', async (t) => {
  primeSessionEndpoint({ url: 'http://service', token: 'tok' });
  const seen: string[] = [];
  const api = createSleeperClient({ fetch: (async (u: string) => new Response(JSON.stringify(new URL(u).pathname.includes('trending') ? [] : lineup[new URL(u).pathname] ?? null))) as unknown as typeof fetch, now: () => AT });
  const snap = { date: TODAY, data: await buildSnapshot(api, 'L1') };
  t.mock.method(globalThis, 'fetch', async (u: string, init?: RequestInit) => {
    const { host, pathname } = new URL(u);
    if (host === 'api.sleeper.app') return new Response(JSON.stringify(new URL(u).pathname.includes('trending') ? [] : lineup[pathname] ?? null));
    seen.push(`${init?.method ?? 'GET'} ${u} ${new Headers(init?.headers).get('authorization')}`);
    if (pathname.endsWith('/players')) return new Response(JSON.stringify(lineup['/v1/players/nfl']));
    return new Response(JSON.stringify(init?.method === 'POST' ? snap : { snapshots: [] }));
  });
  // The client binds global fetch when built, so build the workflow after the mock or it reaches the real API.
  const workflow = trendsFromEnv();
  primeSessionEndpoint(undefined);
  const facts = await (workflow.steps[0] as { run(c: never): unknown }).run({ input: JSON.stringify(INPUT), api: { now: () => AT } } as never);
  assert.equal((facts as { date: string }).date, TODAY);
  // The players dump comes from the Sleeper app's day-long copy, not from Sleeper.
  assert.deepEqual(seen, ['POST http://service/apps/sleeper/leagues/L1/snapshots Bearer tok', 'GET http://service/apps/sleeper/leagues/L1/snapshots Bearer tok', 'GET http://service/apps/sleeper/players Bearer tok']);
});

test('the page routes answer the configured league and start its lineup job', async () => {
  const calls: string[] = [];
  const listing = { schedules: [{ id: 'sleeper/waivers/L1', nextRun: '2026-11-03T20:00:00.000Z' }, { id: 'sleeper/digest/L1/9/recap', nextRun: '2026-11-03T06:15:00.000Z' }, { id: 'sleeper/waivers/OTHER', nextRun: '2026-11-03T01:00:00.000Z' }, { id: 'sleeper/lineup/L1/9/wed', nextRun: null }] };
  const fake = (async (u: string) => (calls.push(u), new Response(u === 'http://sched' ? JSON.stringify(listing) : '{}', { status: 202 }))) as unknown as typeof fetch;
  const service = serveApp(createSleeperService({ dbPath: ':memory:', client: clientOf(fixturesOn('today')), distributorUrl: 'http://d', owner: 'mallard', league: 'L1', schedules: { url: 'http://sched', fetch: fake }, fetch: fake, now: () => new Date(AT) }));
  await new Promise<void>((r) => service.listen(0, r));
  const call = (path: string, method = 'GET') => fetch(`http://127.0.0.1:${(service.address() as AddressInfo).port}${path}`, { method, headers: { authorization: 'Bearer t' } });
  try {
    assert.deepEqual(await (await call('/')).json(), { league: 'L1' });
    const started = await call('/lineup?week=8', 'POST');
    assert.deepEqual([started.status, await started.json()], [202, { sessionKey: 'sleeper:L1:8:lineup', status: 202 }]);
    assert.equal(calls.length, 1);
    assert.deepEqual(await (await call('/schedule')).json(), { week: 9, runs: [{ job: 'digest', at: '2026-11-03T06:15:00.000Z' }, { job: 'waivers', at: '2026-11-03T20:00:00.000Z' }] });
    const waivers = await call('/waivers?week=8', 'POST');
    assert.deepEqual([waivers.status, ((await waivers.json()) as { sessionKey: string }).sessionKey], [202, 'sleeper:L1:8:waivers']);
    assert.equal((await call('/lineup?week=x', 'POST')).status, 400);
    assert.equal((await call('/lineup')).status, 405);
  } finally {
    service.stop();
  }
});
