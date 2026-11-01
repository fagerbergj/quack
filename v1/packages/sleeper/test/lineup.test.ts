import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer, type Server } from 'node:http';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, type FauxResponseStep } from '@earendil-works/pi-ai';
import { createSchedulesService } from '@quack/schedules';
import { Artifacts } from '@quack/sdk';
import { openSession } from '@quack/session';
import { runWorkflow, workflowExtension } from '@quack/workflows';
import {
  buildCard, buildFacts, buildSnapshot, createSleeperClient, createSleeperService, exaNews, lineupEvidence, lineupKey, lineupRubric, lineupRuns, lineupWorkflow, registerLineupSchedules,
  type Facts, type LineupCard, type LineupDeps,
} from '../src/index.ts';
import { closeCalls } from '../src/lineup/facts.ts';
import { bestLineup, numberSlots } from '../src/lineup/slots.ts';

const fixtures = JSON.parse(readFileSync(new URL('./fixtures/lineup.json', import.meta.url), 'utf8')) as Record<string, unknown>;
// Friday after the Thursday game has started and before Sunday's kickoffs.
const AT = Date.parse('2026-11-06T15:00:00Z');
const INPUT = { league: 'L1', week: 9, owner: 'mallard' };

const doFetch = (async (input: string) => {
  const path = new URL(input).pathname;
  return path in fixtures ? new Response(JSON.stringify(fixtures[path])) : new Response('null');
}) as unknown as typeof fetch;
const client = () => createSleeperClient({ fetch: doFetch, now: () => AT });
const snapshot = async (league: string) => ({ data: await buildSnapshot(client(), league) });
const facts = async () => buildFacts({ client: client() }, await snapshot('L1'), INPUT, AT);

// What a good model returns: the RB2 swap the baseline also makes, everything else kept.
const decision = {
  slots: [
    { slot: 'QB', playerId: '100', why: 'Projected 18.2 vs CHI; healthy.' },
    { slot: 'RB1', playerId: '101', why: 'Best healthy back, 15.4.' },
    { slot: 'RB2', playerId: '103', why: 'Dan Dash (11.0) over Rob Rowe (9.1, Questionable); the best-by-projection baseline agrees.' },
    { slot: 'WR1', playerId: '104', why: 'Projected 14.3.' },
    { slot: 'WR2', playerId: '105', why: 'Projected 12.8.' },
    { slot: 'TE', playerId: '107', why: 'Only tight end.' },
    { slot: 'FLEX', playerId: '106', why: 'Wade (10.0) beats Rowe (9.1, Questionable) for the flex.' },
    { slot: 'DEF', playerId: 'SEA', why: 'Only defense.' },
  ],
  summary: 'Start Dan Dash over Rob Rowe at RB2; every other slot is unchanged.',
};

async function goodCard(): Promise<{ f: Facts; card: LineupCard }> {
  const f = await facts();
  return { f, card: buildCard(f, decision) };
}

const gate = async (id: string, f: Facts, card: unknown) => {
  const check = lineupRubric.criteria.flatMap((c) => c.checks).find((c) => c.id === id)!;
  const content = JSON.stringify(card);
  return check.code!(content, await lineupEvidence(f)(null, { content }));
};
const GATES = ['every_slot_decided', 'one_player_per_slot', 'no_locked_swap', 'figures_match_tools', 'summary_names_changes'];

test('slots are numbered and the flex is solved with the strict slots filled first', () => {
  assert.deepEqual(numberSlots(['QB', 'RB', 'RB', 'FLEX']), ['QB', 'RB1', 'RB2', 'FLEX']);
  const pool = [
    { id: 'a', pos: 'RB', pts: 20 }, { id: 'b', pos: 'RB', pts: 12 }, { id: 'c', pos: 'WR', pts: 11 }, { id: 'd', pos: 'RB', pts: 10 },
  ];
  assert.deepEqual(bestLineup(['RB1', 'WR', 'FLEX', 'FLEX'], pool), ['a', 'c', 'b', 'd']);
});

test('facts hold the lock filter, candidates, the baseline and the game join from the recorded week', async () => {
  const f = await facts();
  assert.deepEqual(f.slots, ['QB', 'RB1', 'RB2', 'WR1', 'WR2', 'TE', 'FLEX', 'K', 'DEF']);
  assert.ok(!f.open.includes('K'), 'the Thursday kicker is locked');
  assert.equal(f.players['108'].game?.locked, true);
  assert.equal(f.players['101'].game?.locked, false);
  assert.deepEqual(f.players['101'].game, { opponent: 'ARI', home: true, kickoff: '2026-11-08T21:00:00.000Z', locked: false, status: 'pre_game' });
  assert.deepEqual(f.candidates.RB2, ['101', '103', '102']);
  assert.equal(f.candidates.K, undefined);
  assert.deepEqual(f.baseline.map((b) => `${b.slot}:${b.id}`), ['QB:100', 'RB1:101', 'RB2:103', 'WR1:104', 'WR2:105', 'TE:107', 'FLEX:106', 'K:109', 'DEF:SEA']);
  assert.equal(f.oppProj, 35.5);
  assert.equal(f.team, 'Mallard Mayhem');
  assert.equal(f.opponent, 'teal');
});

test('a locked slot keeps its player and the card carries the tools figures', async () => {
  const { card } = await goodCard();
  const k = card.starters.find((r) => r.slot === 'K')!;
  assert.deepEqual([k.player.id, k.locked, k.action], ['108', true, 'keep']);
  const rb2 = card.starters.find((r) => r.slot === 'RB2')!;
  assert.deepEqual([rb2.player.id, rb2.action, rb2.replaces?.id, rb2.proj], ['103', 'start', '102', 11]);
  assert.equal(card.my_proj, 18.2 + 15.4 + 11 + 14.3 + 12.8 + 8.5 + 10 + 7 + 6.5);
  assert.deepEqual(card.bench.map((b) => b.player.id).sort(), ['102', '109']);
});

test('each code gate passes the good card', async () => {
  const { f, card } = await goodCard();
  for (const id of GATES) assert.equal((await gate(id, f, card)).met, 1, id);
});

test('a card with no changes passes the summary gate only when it says so', async () => {
  const f = await facts();
  const keep = buildCard(f, { slots: decision.slots.map((s) => ({ ...s, playerId: f.current[s.slot]! })), summary: 'No changes: the lineup stands.' });
  assert.equal((await gate('summary_names_changes', f, keep)).met, 1);
  assert.equal((await gate('summary_names_changes', f, { ...keep, summary: 'All set.' })).met, 0);
});

// Like a real model, each why is about the player picked for the slot.
const swapIn = (f: Facts, pairs: Record<string, string>, summary = 'No change.') =>
  buildCard(f, { slots: decision.slots.map((s) => { const playerId = pairs[s.slot] ?? f.current[s.slot] ?? s.playerId; return { slot: s.slot, playerId, why: `Start ${f.players[playerId].name}.` }; }), summary });
const changes = (c: LineupCard) => c.starters.filter((r) => r.action === 'start').map((r) => [r.slot, r.player.id, r.replaces?.id]);

test('a WR2/FLEX permutation of the same starters is all keeps with no changes', async () => {
  const f = await facts();
  const card = swapIn(f, { WR2: f.current.FLEX!, FLEX: f.current.WR2! }, 'Start Walt Wade over Wes West at WR2.');
  assert.deepEqual(changes(card), []);
  assert.equal(card.summary, 'No changes: the lineup stands.');
  assert.equal((await gate('summary_names_changes', f, card)).met, 1);
  for (const r of card.starters.filter((x) => !x.locked)) assert.equal(r.why, `Start ${r.player.name}.`);
});

test('one real swap shows one changed row replacing the benched player', async () => {
  const f = await facts();
  const card = swapIn(f, { RB2: '103' }, 'Start Dan Dash over Rob Rowe at RB2.');
  assert.deepEqual(changes(card), [['RB2', '103', f.current.RB2]]);
  assert.equal((await gate('summary_names_changes', f, card)).met, 1);
});

test('a rotation that also starts a bench player shows one change', async () => {
  const f = await facts();
  const card = swapIn(f, { WR1: f.current.WR2!, WR2: f.current.FLEX!, FLEX: f.current.WR1!, RB2: '103' }, 'Start Dan Dash over Rob Rowe at RB2.');
  assert.deepEqual(changes(card), [['RB2', '103', f.current.RB2]]);
  assert.equal((await gate('summary_names_changes', f, card)).met, 1);
});

test('every_slot_decided fails a card with no FLEX', async () => {
  const { f, card } = await goodCard();
  const r = await gate('every_slot_decided', f, { ...card, starters: card.starters.filter((s) => s.slot !== 'FLEX') });
  assert.ok(r.met < 1);
  assert.match(r.detail!, /FLEX/);
});

test('no_locked_swap fails a card that benches the locked kicker', async () => {
  const { f, card } = await goodCard();
  const k = f.players['109'];
  const swapped = card.starters.map((r) => (r.slot === 'K' ? { ...r, player: { ...r.player, id: k.id, name: k.name }, proj: k.proj, game: k.game, locked: false, action: 'start' as const } : r));
  const r = await gate('no_locked_swap', f, { ...card, starters: swapped });
  assert.ok(r.met < 1);
  assert.match(r.detail!, /K/);
});

test('figures_match_tools fails a projection or a kickoff that differs from the tools', async () => {
  const { f, card } = await goodCard();
  const wrongProj = { ...card, starters: card.starters.map((r) => (r.slot === 'QB' ? { ...r, proj: 24 } : r)) };
  assert.match((await gate('figures_match_tools', f, wrongProj)).detail!, /QB/);
  const wrongGame = { ...card, starters: card.starters.map((r) => (r.slot === 'WR1' ? { ...r, game: { ...r.game!, kickoff: '2026-11-08T13:00:00.000Z' } } : r)) };
  assert.match((await gate('figures_match_tools', f, wrongGame)).detail!, /WR1/);
});

test('summary_names_changes fails a summary that misses a change', async () => {
  const { f, card } = await goodCard();
  assert.equal((await gate('summary_names_changes', f, { ...card, summary: 'Start Dan Dash at RB2.' })).met, 0);
  assert.equal((await gate('summary_names_changes', f, { ...card, summary: 'No changes.' })).met, 0);
});

async function runLineup(news?: LineupDeps['news']) {
  const judged: string[] = [];
  const responses: FauxResponseStep[] = [
    () => fauxAssistantMessage(JSON.stringify(decision)),
    ...Array.from({ length: 40 }, () => (c: { messages: Array<{ content: unknown }> }) => {
      const asked = String(c.messages.at(-1)!.content);
      judged.push(asked);
      return fauxAssistantMessage(JSON.stringify({ choice: /bench a clearly better|mention weather/.test(asked) ? 'no' : 'yes', confidence: 1, reason: 'ok' }));
    }),
  ];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-lineup-'));
  const workflow = lineupWorkflow({ client: client(), snapshot, now: () => AT, news });
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'You are the lineup analyst.', extensions: [workflowExtension([workflow])] } });
  try {
    const outcome = await runWorkflow(s.harness, s.root, workflow, JSON.stringify(INPUT));
    assert.equal(outcome.status, 'completed', JSON.stringify(outcome));
    const sealed = (await s.harness.snapshot(Artifacts, s.root.id, ctx))!.kinds['sleeper.cards'].versions[0];
    return { sealed, judged, verdict: sealed.verdict as { passed: boolean; rubric: string; gatesFailed: string[]; notes: string[]; checks: Array<{ id: string }> } };
  } finally {
    await s.close();
    await rm(dir, { recursive: true, force: true });
  }
}

test('the workflow seals a lineup card from the recorded week and the judge passes it, saying close-call news was not checked', async () => {
  const { sealed, judged, verdict } = await runLineup();
  const card = JSON.parse(sealed.content) as LineupCard;
  assert.equal(card.kind, 'lineup');
  assert.equal(card.starters.length, 9);
  assert.equal(card.starters.find((r) => r.slot === 'RB2')!.player.name, 'Dan Dash');
  assert.equal(card.starters.find((r) => r.slot === 'K')!.locked, true);
  assert.deepEqual([verdict.rubric, verdict.gatesFailed, verdict.passed], ['sleeper.lineup@3', [], true]);
  assert.deepEqual(verdict.notes, ['close-call news not checked: no news source']);
  assert.ok(!verdict.checks.some((c) => c.id === 'close_call_news'));
  assert.ok(!judged.some((q) => /Questionable or limited-practice/.test(q)));
});

test('with a news source wired the close calls are scored for news', async () => {
  const { judged, verdict } = await runLineup(async () => [{ title: 'Practice report', url: 'https://www.nfl.com/n', date: hoursAgo(5) }]);
  assert.deepEqual(verdict.notes, []);
  assert.ok(verdict.checks.some((c) => c.id === 'close_call_news'));
  assert.ok(judged.some((q) => /Questionable or limited-practice/.test(q)));
});

test('a player started in two slots fails the card step and the one_player_per_slot gate', async () => {
  const f = await facts();
  const dup = { ...decision, slots: decision.slots.map((s) => (s.slot === 'FLEX' ? { ...s, playerId: '101' } : s)) };
  assert.throws(() => buildCard(f, dup), /two slots/);
  const { card } = await goodCard();
  const twice = { ...card, starters: card.starters.map((r) => (r.slot === 'FLEX' ? { ...r, player: card.starters[1].player } : r)) };
  const r = await gate('one_player_per_slot', f, twice);
  assert.ok(r.met < 1);
  assert.match(r.detail!, /101/);
});

test('the lineup runs are after Wednesday and Sunday morning, each ending an hour before its kickoff', () => {
  const thursday = Date.parse('2026-11-06T01:15:00Z');
  const sunday = Date.parse('2026-11-08T18:00:00Z');
  const runs = lineupRuns([sunday, thursday]).map((r) => [r.id, new Date(r.at).toISOString(), new Date(r.deadline).toISOString()]);
  assert.deepEqual(runs, [
    ['wed', '2026-11-04T22:00:00.000Z', '2026-11-06T00:15:00.000Z'],
    ['sun', '2026-11-08T14:00:00.000Z', '2026-11-08T17:00:00.000Z'],
  ]);
  // A Sunday-opening week has both runs end at the same kickoff.
  assert.deepEqual(lineupRuns([sunday]).map((r) => r.deadline), [sunday - 3_600_000, sunday - 3_600_000]);
  assert.deepEqual(lineupRuns([]), []);
});

test('the schedules service lists the week with the lineup session key and deadlines, once however often it registers', async () => {
  const schedules = createSchedulesService({ serviceToken: 's', distributorUrl: 'http://unused', dbPath: ':memory:' });
  await new Promise<void>((r) => schedules.listen(0, r));
  const url = `http://127.0.0.1:${(schedules.address() as AddressInfo).port}`;
  try {
    await registerLineupSchedules({ url, token: 's' }, client(), 'L1', 'mallard');
    const ids = await registerLineupSchedules({ url, token: 's' }, client(), 'L1', 'mallard');
    assert.deepEqual(ids, ['sleeper/lineup/L1/9/wed', 'sleeper/lineup/L1/9/sun']);
    const { schedules: listed } = await (await fetch(`${url}/schedules`, { headers: { authorization: 'Bearer s' } })).json();
    assert.equal(listed.length, 2);
    assert.equal(lineupKey('L1', 9), 'sleeper:L1:9:lineup');
    assert.partialDeepStrictEqual(listed[1], {
      id: 'sleeper/lineup/L1/9/wed', agent: 'sleeper-lineup', times: ['2026-11-04T22:00:00.000Z'], deadline: '2026-11-06T00:15:00.000Z', sessionKey: 'sleeper:L1:9:lineup', input: JSON.stringify(INPUT),
    });
    assert.partialDeepStrictEqual(listed[0], { id: 'sleeper/lineup/L1/9/sun', times: ['2026-11-08T14:00:00.000Z'], deadline: '2026-11-08T17:00:00.000Z', sessionKey: 'sleeper:L1:9:lineup' });
  } finally {
    schedules.stop();
  }
});

test('a lineup registered after its Wednesday run time but before the deadline still fires, once', async () => {
  const fired: string[] = [];
  let at = Date.parse('2026-11-05T12:00:00Z');
  const fake = (async (u: string) => (fired.push(decodeURIComponent(u)), new Response('{}'))) as unknown as typeof fetch;
  const schedules = createSchedulesService({ serviceToken: 's', distributorUrl: 'http://d', dbPath: ':memory:', now: () => new Date(at), fetch: fake });
  await new Promise<void>((r) => schedules.listen(0, r));
  const url = `http://127.0.0.1:${(schedules.address() as AddressInfo).port}`;
  try {
    await registerLineupSchedules({ url, token: 's' }, client(), 'L1', 'mallard');
    await schedules.tick();
    await registerLineupSchedules({ url, token: 's' }, client(), 'L1', 'mallard');
    await schedules.tick();
    assert.deepEqual(fired, ['http://d/sessions/sleeper:L1:9:lineup/invocations?agent=sleeper-lineup']);
  } finally {
    schedules.stop();
  }
});

test('the lineup job starts on demand under the week session key, each click its own submission', async () => {
  const calls: Array<{ url: string; body: { input: string; requestId: string } }> = [];
  const fake = (async (u: string, init: { body: string }) => (calls.push({ url: u, body: JSON.parse(init.body) }), new Response('{}', { status: 202 }))) as unknown as typeof fetch;
  let tick = 0;
  const service = createSleeperService({ serviceToken: 't', dbPath: ':memory:', client: client(), distributorUrl: 'http://d', owner: 'mallard', fetch: fake, now: () => new Date(AT + tick++) });
  const bare = createSleeperService({ serviceToken: 't', dbPath: ':memory:', client: client() });
  await Promise.all([service, bare].map((x) => new Promise<void>((r) => x.listen(0, r))));
  const post = (x: typeof service, path: string) => fetch(`http://127.0.0.1:${(x.address() as AddressInfo).port}${path}`, { method: 'POST', headers: { authorization: 'Bearer t' } });
  try {
    const first = await post(service, '/leagues/L1/jobs/lineup');
    assert.equal(first.status, 202);
    assert.deepEqual(await first.json(), { sessionKey: 'sleeper:L1:9:lineup', status: 202 });
    await post(service, '/leagues/L1/jobs/lineup?week=9');
    assert.equal(calls[0].url, 'http://d/sessions/sleeper%3AL1%3A9%3Alineup/invocations?agent=sleeper-lineup');
    assert.deepEqual(JSON.parse(calls[0].body.input), INPUT);
    assert.notEqual(calls[0].body.requestId, calls[1].body.requestId);
    assert.equal((await post(service, '/leagues/L1/jobs/lineup?week=x')).status, 400);
    assert.equal((await post(bare, '/leagues/L1/jobs/lineup')).status, 501);
  } finally {
    service.stop();
    bare.stop();
  }
});

const hoursAgo = (h: number) => new Date(AT - h * 3_600_000).toISOString();
const servers: Server[] = [];
after(() => servers.forEach((s) => s.close()));

async function serve(handler: Parameters<typeof createServer>[1]) {
  const server = createServer(handler).listen(0);
  servers.push(server);
  await new Promise((r) => server.once('listening', r));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

// A fake Exa: records each request and answers every query with a fresh item, a stale one and an undated one.
async function fakeExa() {
  const seen: Array<{ key: unknown; body: { query: string; numResults: number; startPublishedDate: string; includeDomains: string[] } }> = [];
  const url = await serve((req, res) => {
    let raw = '';
    req.on('data', (c) => (raw += c)).on('end', () => {
      seen.push({ key: req.headers['x-api-key'], body: JSON.parse(raw) });
      res.writeHead(200).end(JSON.stringify({ results: [
        { title: 'Practice report', url: 'https://www.nfl.com/news/fresh', publishedDate: hoursAgo(5) },
        { title: 'Content farm', url: 'https://stannsw.org/news/fresh', publishedDate: hoursAgo(4) },
        { title: 'Lookalike host', url: 'https://notnfl.com/news/fresh', publishedDate: hoursAgo(4) },
        { title: 'Old news', url: 'https://www.espn.com/stale', publishedDate: hoursAgo(73) },
        { title: 'Undated', url: 'https://www.espn.com/undated' },
      ] }));
    });
  });
  return { seen, url };
}

test('close-call players get dated news inside 72 hours, and nobody else is searched', async () => {
  const exa = await fakeExa();
  const f = await buildFacts({ client: client(), news: exaNews({ apiKey: 'k', url: exa.url }) }, await snapshot('L1'), INPUT, AT);
  const close = closeCalls(f).map((p) => p.id).sort();
  assert.ok(close.includes('102'));
  assert.deepEqual(Object.keys(f.news).sort(), close);
  assert.ok(!('100' in f.news));
  assert.equal(exa.seen.length, close.length);
  assert.ok(exa.seen.every((r) => r.key === 'k' && r.body.numResults === 3 && r.body.startPublishedDate === hoursAgo(72) && r.body.includeDomains.includes('nfl.com')));
  assert.match(exa.seen.map((r) => r.body.query).join('\n'), /Rob Rowe/);
  assert.deepEqual(f.news['102'], [{ title: 'Practice report', url: 'https://www.nfl.com/news/fresh', date: hoursAgo(5) }]);
});

test('a failing search leaves a close call with no news rather than failing the lineup', async () => {
  const url = await serve((_req, res) => res.writeHead(500).end('boom'));
  assert.deepEqual(await exaNews({ apiKey: 'k', url })((await facts()).players['102'], AT), []);
});

test('with Exa news wired the card is judged on close_call_news', async () => {
  const exa = await fakeExa();
  const { verdict, judged } = await runLineup(exaNews({ apiKey: 'k', url: exa.url }));
  assert.deepEqual([verdict.rubric, verdict.notes], ['sleeper.lineup@3', []]);
  assert.ok(verdict.checks.some((c) => c.id === 'close_call_news'));
  assert.ok(judged.some((q) => /Questionable or limited-practice/.test(q)));
});

const item = { title: 'Practice report', url: 'https://www.nfl.com/n', date: hoursAgo(5) };

test('a row carries the news of its starter and of the player replaced, attached by code', async () => {
  const f = { ...(await facts()), news: { '103': [item], '102': [{ ...item, url: 'https://www.espn.com/m' }], '101': [item] } };
  const card = buildCard(f, decision);
  assert.deepEqual(card.starters.find((r) => r.slot === 'RB2')!.news.map((n) => [n.player, n.url]), [['Dan Dash', item.url], ['Rob Rowe', 'https://www.espn.com/m']]);
  assert.deepEqual(card.starters.find((r) => r.slot === 'RB1')!.news.map((n) => n.player), [f.players['101'].name]);
});

test('a row\'s judge evidence carries its news and each player\'s designation, and baseline-named is asked of changed rows only', async () => {
  const f = { ...(await facts()), news: { '103': [item] } };
  const card = buildCard(f, decision);
  const ev = await lineupEvidence(f)(null, { content: JSON.stringify(card) });
  const rb2 = ev.row.find((u) => u.id === 'RB2')!.state as unknown as { news: Array<{ url: string }>; designations: Record<string, string> };
  assert.deepEqual(rb2.news.map((n) => n.url), [item.url]);
  assert.deepEqual(rb2.designations, { 'Dan Dash': 'none', 'Rob Rowe': 'Questionable' });
  assert.deepEqual(ev.close.map((u) => u.id), ['RB2']);
  // The card agrees with the baseline everywhere, so no row is asked to name the baseline it beats.
  assert.deepEqual(ev.differs, []);
  const other = buildCard(f, { ...decision, slots: decision.slots.map((s) => (s.slot === 'RB2' ? { ...s, playerId: '101' } : s.slot === 'RB1' ? { ...s, playerId: '103' } : s)) });
  assert.deepEqual((await lineupEvidence(f)(null, { content: JSON.stringify(other) })).differs, []);
});
