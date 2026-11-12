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
import { buildEvidence } from '@quack/judge';
import { createSchedulesService } from '@quack/schedules';
import { Artifacts, serveApp } from '@quack/sdk';
import { readSession } from '@quack/eval';
import { replayed } from './replayed.ts';
import { openSession } from '@quack/session';
import { runWorkflow, workflowExtension } from '@quack/workflows';
import {
  buildCard, buildFacts, createSleeperClient, liveLeague, createSleeperService, exaNews, lineupDeclaration, lineupInputsOf, lineupKey, lineupRubric, lineupRuns, lineupWorkflow, newsLink, registerLineupSchedules,
  type Facts, type LineupCard, type LineupDeps,
} from '../src/index.ts';
import { closeCalls } from '../src/lineup/facts.ts';
import { clefSubject, subjectNews } from '../src/lineup/news.ts';
import { bestLineup, numberSlots } from '../src/lineup/slots.ts';

const fixtures = JSON.parse(readFileSync(new URL('./fixtures/lineup.json', import.meta.url), 'utf8')) as Record<string, unknown>;
// Friday after the Thursday game has started and before Sunday's kickoffs.
const AT = Date.parse('2026-11-06T15:00:00Z');
const INPUT = { league: 'L1', week: 9, owner: 'mallard' };

const fetchOf = (world: () => Record<string, unknown>) => (async (input: string) => {
  const path = new URL(input).pathname;
  const fx = world();
  return path in fx ? new Response(JSON.stringify(fx[path])) : new Response('null');
}) as unknown as typeof fetch;
const client = (fx = fixtures) => createSleeperClient({ fetch: fetchOf(() => fx), now: () => AT });
const snapshot = (league: string, fx = fixtures) => liveLeague(client(fx), league);
const facts = async (fx = fixtures) => buildFacts({ client: client(fx) }, await snapshot('L1', fx), INPUT, AT);
// The same week after a projection update: a second run in one session decides from different facts.
const updated = { ...fixtures, '/v1/projections/nfl/regular/2026/9': { ...(fixtures['/v1/projections/nfl/regular/2026/9'] as object), 100: { pts_ppr: 21 } } };

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
};

async function goodCard(): Promise<{ f: Facts; card: LineupCard }> {
  const f = await facts();
  return { f, card: buildCard(f, decision) };
}

const gate = async (id: string, f: Facts, card: unknown) => {
  const check = lineupRubric.criteria.flatMap((c) => c.checks).find((c) => c.id === id)!;
  const content = JSON.stringify(card);
  return check.code!(content, (await buildEvidence(lineupDeclaration, lineupInputsOf(f, content))).evidence);
};
const GATES = ['every_slot_decided', 'no_locked_swap'];

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

test('the summary is written by code from the changes, and a card with none says so', async () => {
  const f = await facts();
  assert.equal(buildCard(f, decision).summary, 'Start Dan Dash over Rob Rowe at RB2.');
  const keep = buildCard(f, { slots: decision.slots.map((s) => ({ ...s, playerId: f.current[s.slot]! })) });
  assert.equal(keep.summary, 'No changes: the lineup stands.');
});

// Like a real model, each why is about the player picked for the slot.
const swapIn = (f: Facts, pairs: Record<string, string>) =>
  buildCard(f, { slots: decision.slots.map((s) => { const playerId = pairs[s.slot] ?? f.current[s.slot] ?? s.playerId; return { slot: s.slot, playerId, why: `Start ${f.players[playerId].name}.` }; }) });
const changes = (c: LineupCard) => c.starters.filter((r) => r.action === 'start').map((r) => [r.slot, r.player.id, r.replaces?.id]);

test('a WR2/FLEX permutation of the same starters is all keeps with no changes', async () => {
  const f = await facts();
  const card = swapIn(f, { WR2: f.current.FLEX!, FLEX: f.current.WR2! });
  assert.deepEqual(changes(card), []);
  assert.equal(card.summary, 'No changes: the lineup stands.');
  for (const r of card.starters.filter((x) => !x.locked)) assert.equal(r.why, `Start ${r.player.name}.`);
});

test('one real swap shows one changed row replacing the benched player', async () => {
  const f = await facts();
  const card = swapIn(f, { RB2: '103' });
  assert.deepEqual(changes(card), [['RB2', '103', f.current.RB2]]);
  assert.equal(card.summary, 'Start Dan Dash over Rob Rowe at RB2.');
});

test('a rotation that also starts a bench player shows one change', async () => {
  const f = await facts();
  const card = swapIn(f, { WR1: f.current.WR2!, WR2: f.current.FLEX!, FLEX: f.current.WR1!, RB2: '103' });
  assert.deepEqual(changes(card), [['RB2', '103', f.current.RB2]]);
  assert.equal(card.summary, 'Start Dan Dash over Rob Rowe at RB2.');
});

test('every_slot_decided fails a card with no FLEX', async () => {
  const { f, card } = await goodCard();
  const r = await gate('every_slot_decided', f, { ...card, starters: card.starters.filter((s) => s.slot !== 'FLEX') });
  assert.ok(r.met < 1);
  assert.match(r.detail!, /FLEX/);
});

test('no_locked_swap fails a card that starts a player whose game has locked', async () => {
  const { f, card } = await goodCard();
  // The model may pick any rostered player for an open slot; one whose game has locked is caught here, not by the card builder.
  const dash = f.players['103'];
  const r = await gate('no_locked_swap', { ...f, players: { ...f.players, '103': { ...dash, game: { ...dash.game!, locked: true } } } }, card);
  assert.ok(r.met < 1);
  assert.match(r.detail!, /RB2/);
});

// One run per world, in one session; the model decides when asked for the lineup and answers the judge's questions otherwise.
async function runLineup(news?: LineupDeps['news'], worlds = [fixtures]) {
  const judged: string[] = [];
  const responses: FauxResponseStep[] = Array.from({ length: 40 * worlds.length }, () => (c: { messages: Array<{ content: unknown }> }) => {
    const asked = String(c.messages.at(-1)!.content);
    if (!asked.startsWith('Decision:')) return fauxAssistantMessage(JSON.stringify(decision));
    judged.push(asked);
    return fauxAssistantMessage(JSON.stringify({ choice: /bench a clearly better/.test(asked) ? 'no' : 'yes', confidence: 1, reason: 'ok' }));
  });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-lineup-'));
  // The client's cache clock moves a day between runs, so a later run reads its own world.
  let world = worlds[0];
  let cacheClock = AT;
  const workflow = lineupWorkflow({ client: createSleeperClient({ fetch: fetchOf(() => world), now: () => cacheClock }), now: () => AT, news });
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'You are the lineup analyst.', extensions: [workflowExtension([workflow])] } });
  try {
    for (world of worlds) {
      const outcome = await runWorkflow(s.harness, s.root, workflow, JSON.stringify(INPUT));
      assert.equal(outcome.status, 'completed', JSON.stringify(outcome));
      cacheClock += 86_400_000;
    }
    const versions = (await s.harness.snapshot(Artifacts, s.root.id, ctx))!.kinds['sleeper.cards'].versions;
    const sealed = versions[0];
    await s.close();
    return { record: await readSession(dir), sealed, versions, judged, verdict: sealed.verdict as { passed: boolean; rubric: string; gatesFailed: string[]; notes: string[]; checks: Array<{ id: string; units: number; met: unknown }> } };
  } finally {
    await s.close();
    await rm(dir, { recursive: true, force: true });
  }
}

test('the workflow seals a lineup card from the recorded week and the judge passes it, with no news to score close calls on', async () => {
  const { sealed, judged, verdict, record } = await runLineup();
  assert.deepEqual(await replayed(record, sealed), lineupInputsOf(await facts(), sealed.content), 'the recording rebuilds the inputs the judge read');
  const card = JSON.parse(sealed.content) as LineupCard;
  assert.equal(card.kind, 'lineup');
  assert.equal(card.starters.length, 9);
  assert.equal(card.starters.find((r) => r.slot === 'RB2')!.player.name, 'Dan Dash');
  assert.equal(card.starters.find((r) => r.slot === 'K')!.locked, true);
  assert.deepEqual([verdict.rubric, verdict.gatesFailed, verdict.passed], ['sleeper.lineup@6', [], true]);
  assert.deepEqual(verdict.notes, []);
  assert.deepEqual(verdict.checks.filter((c) => c.id === 'close_call_news').map((c) => [c.units, c.met]), [[0, null]]);
  assert.ok(!judged.some((q) => /Questionable starter/.test(q)));
});

test('a session that ran the job twice replays each card on the facts of the run that sealed it', async () => {
  const { versions, record } = await runLineup(undefined, [fixtures, updated]);
  assert.equal(versions.length, 2);
  const [first, second] = [await facts(), await facts(updated)];
  assert.notDeepEqual(first, second, 'the runs decided from different facts');
  assert.deepEqual(await replayed(record, versions[0]), lineupInputsOf(first, versions[0].content));
  assert.deepEqual(await replayed(record, versions[1]), lineupInputsOf(second, versions[1].content));
});

test('with a news source wired the close calls are scored for news', async () => {
  const { judged, verdict, record, sealed } = await runLineup(async () => [{ title: 'Practice report', url: 'https://www.nfl.com/n', date: hoursAgo(5) }]);
  await replayed(record, sealed);
  assert.deepEqual(verdict.notes, []);
  assert.ok(verdict.checks.some((c) => c.id === 'close_call_news'));
  assert.ok(judged.some((q) => /Questionable starter/.test(q)));
});

test('a player started in two slots fails the card step, so the model is asked again', async () => {
  const f = await facts();
  const dup = { ...decision, slots: decision.slots.map((s) => (s.slot === 'FLEX' ? { ...s, playerId: '101' } : s)) };
  assert.throws(() => buildCard(f, dup), /two slots/);
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
  const schedules = createSchedulesService({ distributorUrl: 'http://unused', dbPath: ':memory:' });
  const schedulesServer = serveApp(schedules).listen(0);
  await new Promise((r) => schedulesServer.once('listening', r));
  const url = `http://127.0.0.1:${(schedulesServer.address() as AddressInfo).port}`;
  try {
    await registerLineupSchedules({ url }, client(), 'L1', 'mallard');
    const ids = await registerLineupSchedules({ url }, client(), 'L1', 'mallard');
    assert.deepEqual(ids, ['sleeper/lineup/L1/9/wed', 'sleeper/lineup/L1/9/sun']);
    const { schedules: listed } = await (await fetch(url)).json();
    assert.equal(listed.length, 2);
    assert.equal(lineupKey('L1', 9), 'sleeper:L1:9:lineup');
    assert.partialDeepStrictEqual(listed[1], {
      id: 'sleeper/lineup/L1/9/wed', agent: 'sleeper-lineup', times: ['2026-11-04T22:00:00.000Z'], deadline: '2026-11-06T00:15:00.000Z', sessionKey: 'sleeper:L1:9:lineup', input: JSON.stringify(INPUT),
    });
    assert.partialDeepStrictEqual(listed[0], { id: 'sleeper/lineup/L1/9/sun', times: ['2026-11-08T14:00:00.000Z'], deadline: '2026-11-08T17:00:00.000Z', sessionKey: 'sleeper:L1:9:lineup' });
  } finally {
    schedulesServer.close();
    schedules.stop();
  }
});

test('a lineup registered after its Wednesday run time but before the deadline still fires, once', async () => {
  const fired: string[] = [];
  let at = Date.parse('2026-11-05T12:00:00Z');
  const fake = (async (u: string) => (fired.push(decodeURIComponent(u)), new Response('{}'))) as unknown as typeof fetch;
  const schedules = createSchedulesService({ distributorUrl: 'http://d', dbPath: ':memory:', now: () => new Date(at), fetch: fake });
  const schedulesServer = serveApp(schedules).listen(0);
  await new Promise((r) => schedulesServer.once('listening', r));
  const url = `http://127.0.0.1:${(schedulesServer.address() as AddressInfo).port}`;
  try {
    await registerLineupSchedules({ url }, client(), 'L1', 'mallard');
    await schedules.tick();
    await registerLineupSchedules({ url }, client(), 'L1', 'mallard');
    await schedules.tick();
    assert.deepEqual(fired, ['http://d/sessions/sleeper:L1:9:lineup/invocations?agent=sleeper-lineup']);
  } finally {
    schedulesServer.close();
    schedules.stop();
  }
});

test('the lineup job starts on demand under the week session key, each click its own submission', async () => {
  const calls: Array<{ url: string; body: { input: string; requestId: string } }> = [];
  const fake = (async (u: string, init: { body: string }) => (calls.push({ url: u, body: JSON.parse(init.body) }), new Response('{}', { status: 202 }))) as unknown as typeof fetch;
  let tick = 0;
  const service = serveApp(createSleeperService({ dbPath: ':memory:', client: client(), distributorUrl: 'http://d', owner: 'mallard', league: 'L1', fetch: fake, now: () => new Date(AT + tick++) }));
  const bare = serveApp(createSleeperService({ dbPath: ':memory:', client: client(), league: 'L1' }));
  await Promise.all([service, bare].map((x) => new Promise<void>((r) => x.listen(0, r))));
  const post = (x: typeof service, path: string) => fetch(`http://127.0.0.1:${(x.address() as AddressInfo).port}${path}`, { method: 'POST', headers: { authorization: 'Bearer t' } });
  try {
    const first = await post(service, '/lineup');
    assert.equal(first.status, 202);
    assert.deepEqual(await first.json(), { sessionKey: 'sleeper:L1:9:lineup', status: 202 });
    await post(service, '/lineup?week=9');
    assert.equal(calls[0].url, 'http://d/sessions/sleeper%3AL1%3A9%3Alineup/invocations?agent=sleeper-lineup');
    assert.deepEqual(JSON.parse(calls[0].body.input), INPUT);
    assert.notEqual(calls[0].body.requestId, calls[1].body.requestId);
    assert.equal((await post(service, '/lineup?week=x')).status, 400);
    assert.equal((await post(bare, '/lineup')).status, 501);
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
        { title: 'Rob Rowe practice report', url: 'https://www.nfl.com/news/fresh', publishedDate: hoursAgo(5) },
        { title: 'Injury report', url: 'https://www.nfl.com/news/other', publishedDate: hoursAgo(5), highlights: ['Dan Dash is a full participant.'] },
        { title: 'Team injury roundup', url: 'https://www.nfl.com/news/roundup', publishedDate: hoursAgo(5), highlights: ['Other players are listed.'] },
        { title: 'Depth chart notes', url: 'https://www.nfl.com/news/depth', publishedDate: hoursAgo(6), highlights: [{ text: `# Depth chart notes\n\n${'Team news. '.repeat(40)}\n\nRob Rowe takes first-team reps.` }] },
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
  const search = exaNews({ apiKey: 'k', url: exa.url });
  const f = await buildFacts({ client: client(), news: (p, at) => search(p, at, async () => true) }, await snapshot('L1'), INPUT, AT);
  const close = closeCalls(f).map((p) => p.id).sort();
  assert.ok(close.includes('102'));
  assert.deepEqual(Object.keys(f.news).sort(), close);
  assert.ok(!('100' in f.news));
  assert.equal(exa.seen.length, close.length);
  assert.ok(exa.seen.every((r) => r.key === 'k' && r.body.numResults === 3 && r.body.startPublishedDate === hoursAgo(72) && r.body.includeDomains.includes('nfl.com')));
  assert.match(exa.seen.map((r) => r.body.query).join('\n'), /Rob Rowe/);
  // A title that names the player stands alone; otherwise a short window of the highlight that names him rides along, into the prompt too.
  const [fresh, depth] = f.news['102'];
  assert.deepEqual(fresh, { title: 'Rob Rowe practice report', url: 'https://www.nfl.com/news/fresh', date: hoursAgo(5) });
  assert.deepEqual([depth.title, depth.url, depth.date], ['Depth chart notes', 'https://www.nfl.com/news/depth', hoursAgo(6)]);
  assert.ok(depth.excerpt!.endsWith('Rob Rowe takes first-team reps.') && depth.excerpt!.length <= 280 && !/[\n#]/.test(depth.excerpt!), depth.excerpt);
  assert.equal(newsLink(depth), `[Depth chart notes](https://www.nfl.com/news/depth), ${hoursAgo(6).slice(0, 16)}: "${depth.excerpt}"`);
});

// Reports the judges caught on the wrong player: each names him only inside another subject's news.
const LOVELAND = { name: 'Colston Loveland', team: 'CHI', pos: 'TE' };
const offSubject = [
  { player: LOVELAND, title: 'Injury Update: T.J. Edwards set to return Monday night in Seattle', text: 'Edwards returns as Colston Loveland and the Bears face Seattle.' },
  { player: LOVELAND, title: 'NFL Week 9 Injury Report: Jeremiyah Love hits IR, Lamar Jackson likely done for season, and more - NBC Sports', text: 'Bears: Colston Loveland is off the report.' },
  { player: { name: 'TreVeyon Henderson', team: 'NE', pos: 'RB' }, title: 'Rhamondre (knee) could miss short period of time - NBC Sports', text: 'TreVeyon Henderson remains sidelined with an illness.' },
  { player: { name: 'Malik Nabers', team: 'NYG', pos: 'WR' }, title: 'Fantasy Football Week 9 WR preview: Mike Evans eyes return with WR1 upside - CBS Sports', text: 'Malik Nabers and Mike Evans headline the week.' },
];

async function exaWith(results: Array<{ title: string; url: string; text?: string }>) {
  const url = await serve((req, res) => req.resume().on('end', () => res.writeHead(200).end(JSON.stringify({
    results: results.map((r) => ({ title: r.title, url: r.url, publishedDate: hoursAgo(5), highlights: r.text ? [r.text] : [] })),
  }))));
  return exaNews({ apiKey: 'k', url });
}

test('a report about someone else that names the player is dropped when the subject check says no, or gives no answer', async () => {
  for (const [i, x] of offSubject.entries()) {
    const search = await exaWith([{ title: x.title, url: `https://www.nbcsports.com/n${i}`, text: x.text }]);
    const asked: Array<[string, string, string | undefined]> = [];
    assert.deepEqual(await search(x.player, AT, async (p, n) => (asked.push([p.name, n.title, n.excerpt]), false)), [], x.title);
    assert.deepEqual(asked, [[x.player.name, x.title, x.text]]);
    assert.deepEqual(await search(x.player, AT), [], 'without a check, a title that does not name him drops the report');
    assert.deepEqual(await search(x.player, AT, async () => undefined), []);
  }
});

test('a title naming the player keeps the report unasked; the subject check decides the rest, once per report', async () => {
  const search = await exaWith([
    { title: 'Colston Loveland off injury report, to play SEA - NBC Sports', url: 'https://www.nbcsports.com/a' },
    { title: 'Bears TE Colston Loveland active for MNF', url: 'https://sports.yahoo.com/b', text: 'Colston Loveland is active.' },
    { title: 'Week 10 Injury Report: Bears at Giants', url: 'https://www.chicagobears.com/c', text: 'TE Colston Loveland (ankle) - LP' },
  ]);
  const asked: string[] = [];
  const about = async (_p: unknown, n: { url: string }) => (asked.push(n.url), true);
  const urls = async (at: number) => (await search(LOVELAND, at, about)).map((n) => n.url);
  assert.deepEqual(await urls(AT), ['https://www.nbcsports.com/a', 'https://sports.yahoo.com/b', 'https://www.chicagobears.com/c']);
  assert.deepEqual(await urls(AT + 1), ['https://www.nbcsports.com/a', 'https://sports.yahoo.com/b', 'https://www.chicagobears.com/c']);
  assert.deepEqual(asked, ['https://www.chicagobears.com/c'], 'one question for the team report, and its answer is reused');
});

test('a surname alone never stands for the player: another Williams in the title is asked about, and an unknown answer is asked again', async () => {
  const search = await exaWith([
    { title: 'Kyren Williams Week 10 Outlook - Rams at Cardinals', url: 'https://www.fantasypros.com/k', text: 'Kyren Williams leads a run game; Javonte Williams faces the 49ers.' },
    { title: "NOTEBOOK: Williams' dynamic play impacting Lions' offense", url: 'https://www.detroitlions.com/j', text: 'Jameson Williams has three catches of 40 yards.' },
  ]);
  const javonte = { name: 'Javonte Williams', team: 'DAL', pos: 'RB' };
  const jameson = { name: 'Jameson Williams', team: 'DET', pos: 'WR' };
  const asked: string[] = [];
  const about = (yes: boolean | undefined) => async (p: { name: string }, n: { url: string }) => (asked.push(`${p.name} ${n.url}`), yes);
  assert.deepEqual(await search(javonte, AT, about(false)), []);
  assert.deepEqual(await search(jameson, AT, about(undefined)), []);
  assert.deepEqual((await search(jameson, AT, about(true))).map((n) => n.url), ['https://www.detroitlions.com/j']);
  assert.deepEqual(asked, ['Javonte Williams https://www.fantasypros.com/k', 'Jameson Williams https://www.detroitlions.com/j', 'Jameson Williams https://www.detroitlions.com/j']);
});

test('the decision model is asked one yes/no question per report, about the player by team and position, over its title and excerpt', async () => {
  const seen: Array<{ state: unknown; questions: Record<string, { instructions: string }> }> = [];
  const classifier = { id: 'clef', type: 'classifier' };
  const api = (probability: number | Error, models = [classifier]) => ({
    models: {
      getAllModels: () => models,
      classify: async (_m: unknown, req: (typeof seen)[number]) => {
        seen.push(req);
        if (probability instanceof Error) throw probability;
        return { stopReason: 'stop', answers: { q0: { type: 'bool', probability } } };
      },
    },
  }) as never;
  const ref = { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 };
  const report = { title: offSubject[0].title, url: 'https://www.chicagobears.com/e', date: hoursAgo(5), excerpt: offSubject[0].text };
  assert.equal(await clefSubject(api(0.01), ref)(LOVELAND, report), false);
  assert.equal(await clefSubject(api(0.92), ref)(LOVELAND, report), true);
  assert.deepEqual(seen[0].state, { player: 'Colston Loveland (CHI TE)', report: { title: report.title, excerpt: report.excerpt } });
  assert.match(seen[0].questions.q0.instructions, /about Colston Loveland \(CHI TE\) himself/);
  assert.equal(await clefSubject(api(new Error('down')), ref)(LOVELAND, report), undefined);
  assert.equal(await clefSubject(api(0.9, []), ref)(LOVELAND, report), undefined);
  await clefSubject(api(0.9), ref)({ name: 'New Orleans Saints', team: 'NO', pos: 'DEF' }, report);
  assert.equal((seen.at(-1)!.state as { player: string }).player, 'the New Orleans Saints defense');
});

test('a job binds its decision model as the subject check, and without one the title alone decides', async () => {
  const got: unknown[] = [];
  const search = async (_p: unknown, _at: number, about?: unknown) => (got.push(about), []);
  await subjectNews(search, {} as never, { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 })!(LOVELAND, AT);
  await subjectNews(search, {} as never, undefined)!(LOVELAND, AT);
  assert.deepEqual(got.map((a) => typeof a), ['function', 'undefined']);
  assert.equal(subjectNews(undefined, {} as never, undefined), undefined);
});

test('a failing search leaves a close call with no news rather than failing the lineup', async () => {
  const url = await serve((_req, res) => res.writeHead(500).end('boom'));
  assert.deepEqual(await exaNews({ apiKey: 'k', url })((await facts()).players['102'], AT), []);
});

test('with Exa news wired the card is judged on close_call_news', async () => {
  const exa = await fakeExa();
  const { verdict, judged } = await runLineup(exaNews({ apiKey: 'k', url: exa.url }));
  assert.deepEqual([verdict.rubric, verdict.notes], ['sleeper.lineup@6', []]);
  assert.ok(verdict.checks.some((c) => c.id === 'close_call_news'));
  assert.ok(judged.some((q) => /Questionable starter/.test(q)));
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
  const ev = (await buildEvidence(lineupDeclaration, lineupInputsOf(f, JSON.stringify(card)))).evidence;
  const rb2 = ev.news.find((u) => u.id === 'news RB2')!.state as unknown as { row: Record<string, unknown>; news: Array<{ url: string }>; designations: Record<string, string> };
  // The news check keeps the evidence it had before the port: these fields at the top, the card's row whole, the news once.
  assert.deepEqual(Object.keys(rb2), ['slot', 'week', 'row', 'incumbent', 'baseline', 'news', 'designations']);
  assert.ok(!('news' in rb2.row) && rb2.row.why === card.starters.find((r) => r.slot === 'RB2')!.why);
  assert.deepEqual(rb2.news.map((n) => n.url), [item.url]);
  assert.deepEqual(rb2.designations, { 'Dan Dash': 'none', 'Rob Rowe': 'Questionable' });
  assert.deepEqual(ev.news.map((u) => u.id), ['news RB2']);
  // The locked kicker is asked nothing, and the card agrees with the baseline everywhere, so no row is asked to name the baseline it beats.
  assert.ok(!Object.values(ev).flat().some((u) => / K$/.test(u.id)));
  assert.deepEqual(ev.differs, []);
  const other = buildCard(f, { ...decision, slots: decision.slots.map((s) => (s.slot === 'RB2' ? { ...s, playerId: '101' } : s.slot === 'RB1' ? { ...s, playerId: '103' } : s)) });
  assert.deepEqual((await buildEvidence(lineupDeclaration, lineupInputsOf(f, JSON.stringify(other)))).evidence.differs, []);
});

// Week 9's FLEX row, as prod sealed it: its why cites Hubbard's trade-candidate report, which the card attaches to RB2.
test('a row\'s news check sees the reports of every player its why names, each once', async () => {
  const game = { opponent: 'DEN', home: true, kickoff: '2026-11-08T18:00:00.000Z', locked: false, status: 'pre_game' };
  const hubbard = { id: '7594', name: 'Chuba Hubbard', pos: 'RB', team: 'CAR', inj: null, proj: 16.8, game };
  const irving = { id: '11584', name: 'Bucky Irving', pos: 'RB', team: 'TB', inj: null, proj: 17.56, game: { ...game, opponent: 'CHI', home: false } };
  const report = (title: string, url: string) => ({ title, url, date: '2026-11-05T02:00:00.000Z' });
  const trade = report('Chuba Hubbard Floated as Possible Trade Candidate For Panthers - Yahoo Sports', 'https://sports.yahoo.com/articles/chuba-hubbard-floated-possible-trade-193509466.html');
  const volume = report("Chuba Hubbard's bankable volume makes him matchup-proof in fantasy - Yahoo Sports", 'https://sports.yahoo.com/video/chuba-hubbards-bankable-volume-makes-him-matchup-proof-in-fantasy-024709520.html');
  const ap = report("Irving and the Buccaneers visit Swift and the Bears in a matchup of the NFL's top rushing teams", 'https://apnews.com/article/buccaneers-bears-nfl-caleb-williams-6a273030b470edec89611a313def9122');
  const f: Facts = {
    league: 'L1', week: 9, team: 't', record: '', opponent: null, slots: ['RB2', 'FLEX'], current: { RB2: hubbard.id, FLEX: irving.id }, open: ['RB2', 'FLEX'],
    players: { [hubbard.id]: hubbard, [irving.id]: irving }, candidates: { RB2: [irving.id, hubbard.id], FLEX: [irving.id, hubbard.id] },
    baseline: [{ slot: 'RB2', id: hubbard.id, name: hubbard.name, pos: 'RB', proj: 16.8 }, { slot: 'FLEX', id: irving.id, name: irving.name, pos: 'RB', proj: 17.56 }],
    oppProj: null, news: { [hubbard.id]: [volume, trade], [irving.id]: [ap] },
  };
  const card = buildCard(f, { slots: [
    { slot: 'RB2', playerId: hubbard.id, why: "16.8 proj, best remaining RB/WR/TE after dedicated slots filled; 'bankable volume' (Yahoo); no designation; vs DEN." },
    { slot: 'FLEX', playerId: irving.id, why: '17.56 proj over Hubbard 16.8; Bucs top NFL rushing (168.6 yd/g, AP); Irving primary RB, Hubbard trade-candidate news (Yahoo); matches baseline.' },
  ] });
  const ev = (await buildEvidence(lineupDeclaration, lineupInputsOf(f, JSON.stringify(card)))).evidence;
  const shown = (slot: string) => (ev.news.find((u) => u.id === `news ${slot}`)!.state as { news: Array<{ player: string; url: string }> }).news.map((n) => [n.player, n.url]);
  assert.deepEqual(shown('FLEX'), [['Bucky Irving', ap.url], ['Chuba Hubbard', volume.url], ['Chuba Hubbard', trade.url]]);
  // RB2's why names only Hubbard, whose reports the row already carries.
  assert.deepEqual(shown('RB2'), [['Chuba Hubbard', volume.url], ['Chuba Hubbard', trade.url]]);
});

test('a start the baseline does not make is asked to name the baseline player, with the incumbent it benches', async () => {
  const real = await facts();
  // A baseline that keeps Rowe at RB2, so the card's Dash swap goes against it.
  const f = { ...real, baseline: real.baseline.map((b) => (b.slot === 'RB2' ? { ...b, id: '102', name: real.players['102'].name, proj: real.players['102'].proj } : b)) };
  const card = buildCard(f, decision);
  const ev = (await buildEvidence(lineupDeclaration, lineupInputsOf(f, JSON.stringify(card)))).evidence;
  const rows = ev.differs.map((u) => ({ id: u.id, ...(u.state.row as { incumbent: { name: string }; baseline: { name: string } }) }));
  assert.deepEqual(rows.map((r) => [r.id, r.incumbent.name, r.baseline.name]), [['differs RB2', 'Rob Rowe', 'Rob Rowe']]);
});

test('the lineup judge asks the configured decision model', () => {
  const ref = { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 };
  const step = lineupWorkflow({ client: client(), decisionModel: ref }).steps.find((s) => s.name === 'card')!;
  assert.equal(step.kind, 'judged');
  const { options } = (step as Extract<typeof step, { kind: 'judged' }>).judge({ prior: { facts: {} } } as never);
  assert.deepEqual(options.decisionModel, ref);
});
