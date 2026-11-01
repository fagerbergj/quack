import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, type FauxResponseStep } from '@earendil-works/pi-ai';
import { createSchedulesService } from '@quack/schedules';
import { Artifacts } from '@quack/sdk';
import { openSession } from '@quack/session';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { runWorkflow, workflowExtension } from '@quack/workflows';
import {
  buildFinderCard, buildSnapshot, buildTradeCard, buildTradeFacts, createSleeperClient, createSleeperService, finderKey, finderRun, finderWorkflow, registerFinderSchedules, tradeEvidence, tradeKey, tradeRubric, tradeWorkflow,
  type FinderCard, type TradeCard, type TradeFacts,
} from '../src/index.ts';
import type { FinderDecision, TradeDecision } from '../src/trades/card.ts';
import { fetchNews } from '../src/trades/facts.ts';

type Fixtures = Record<string, any>;
const fixtures = JSON.parse(readFileSync(new URL('./fixtures/trades.json', import.meta.url), 'utf8')) as Fixtures;
const AT = Date.parse('2026-11-06T15:00:00Z');
const FINDER = { league: 'L1', week: 9, owner: 'mallard' };
// Give Bo Bench (WR, surplus) to Pintail Pride for Tom Tail (TE, surplus): each fills the other's need.
const OFFER = { ...FINDER, partner: '3', give: ['110'], get: ['411'] };

const clientOf = (fx: Fixtures = fixtures) => createSleeperClient({
  fetch: (async (input: string) => {
    const path = new URL(input).pathname;
    return path in fx ? new Response(JSON.stringify(fx[path])) : new Response('null');
  }) as unknown as typeof fetch,
  now: () => AT,
});
const snapshotOf = (fx: Fixtures = fixtures) => async (league: string) => ({ data: await buildSnapshot(clientOf(fx), league) });
const factsOf = async (input: Parameters<typeof buildTradeFacts>[2] = FINDER) => buildTradeFacts({ client: clientOf() }, await snapshotOf()('L1'), input);

const item = { title: 'Depth chart report', url: 'https://www.nfl.com/n', date: new Date(AT - 5 * 3_600_000).toISOString() };
const finderDecision: FinderDecision = {
  suggestions: [{ partner: 3, give: '110', get: '411', partner_need: 'needs WR', note: 'Pintail Pride is 2-6 and short at WR; Tom Tail is their third tight end.' }],
  summary: 'Offer Bo Bench to Pintail Pride for Tom Tail.',
};
const tradeDecision: TradeDecision = { verdict: 'send', why: 'Pintail Pride is thin at WR and has a spare TE; you need TE depth.', flips_if: 'Tom Tail is ruled Out for the season.', summary: 'Send it: it fills a need on both sides.' };

const gate = async (id: string, f: TradeFacts, c: unknown) => {
  const check = tradeRubric.criteria.flatMap((k) => k.checks).find((k) => k.id === id)!;
  const content = JSON.stringify(c);
  return check.code!(content, await tradeEvidence(f)(null, { content }));
};

test('the partner table holds record, standing, stance, needs and surplus from the recorded week', async () => {
  const f = await factsOf();
  assert.deepEqual([f.teams, f.playoffTeams, f.offer], [3, 2, null]);
  assert.deepEqual([f.mine.rank, f.mine.stance, f.mine.surplus, f.mine.needs], [2, 'contender', ['110'], { TE: 1 }]);
  assert.deepEqual(f.partners.map((p) => [p.rosterId, p.record, p.rank, p.stance, p.surplus]), [[2, '6-2', 1, 'contender', ['400']], [3, '2-6', 3, 'rebuilder', ['411']]]);
  assert.deepEqual(f.partners[1].needs, { RB: 3, WR: 2, K: 1, DEF: 1 });
  assert.equal(f.partners[0].players['400'].role, 'bench');
});

test('an analysis resolves the partner by roster id, owner or team name, and rejects players off the rosters', async () => {
  const f = await factsOf(OFFER);
  assert.deepEqual([f.partners.map((p) => p.rosterId), f.offer], [[3], { partner: 3, give: ['110'], get: ['411'] }]);
  for (const partner of ['pintail', 'Pintail Pride']) assert.equal((await factsOf({ ...OFFER, partner })).partners[0].rosterId, 3);
  await assert.rejects(factsOf({ ...OFFER, partner: '1' }), /no partner team/);
  await assert.rejects(factsOf({ ...OFFER, partner: 'nobody' }), /no partner team/);
  await assert.rejects(factsOf({ ...OFFER, give: ['200'] }), /200 is not on your roster/);
  await assert.rejects(factsOf({ ...OFFER, get: ['110'] }), /110 is not on Pintail Pride/);
  await assert.rejects(factsOf({ ...OFFER, give: [] }), /both sides/);
});

test('the finder card carries the tools figures and the swing, with news attached by code for both players', async () => {
  const f = await factsOf();
  const news = { '110': [item], '411': [] };
  const c = buildFinderCard(f, finderDecision, news);
  assert.deepEqual(c.suggestions.map((s) => [s.rank, s.partner.team, s.give.id, s.give.proj, s.get.id, s.get.proj, s.delta, s.partner_need]), [[1, 'Pintail Pride', '110', 3.1, '411', 2.8, { you: -0.3, partner: 0.3 }, 'needs WR']]);
  assert.deepEqual(c.suggestions[0].news.map((n) => [n.player, n.url]), [['Bo Bench', item.url]]);
  assert.equal(buildFinderCard(f, { suggestions: [], summary: 'x' }, {}).summary, 'No deals: no partner has a surplus player this team needs.');
});

test('a bad finder decision fails the card step instead of reaching the judge', async () => {
  const f = await factsOf();
  const s = finderDecision.suggestions[0];
  const build = (suggestions: FinderDecision['suggestions']) => buildFinderCard(f, { ...finderDecision, suggestions }, {});
  assert.throws(() => build([s, { ...s, give: '104' }, { ...s, give: '105' }, { ...s, give: '106' }]), /4 deals, at most 3/);
  assert.throws(() => build([s, { ...s, partner: 2, get: '400' }]), /same player in two deals/);
  assert.throws(() => build([{ ...s, partner: 9 }]), /not a partner/);
  assert.throws(() => build([{ ...s, get: '203' }]), /not surplus on Pintail Pride/);
  assert.throws(() => build([{ ...s, give: '200' }]), /200, who is not on your team/);
});

test('the trade card carries both sides figures, a counter, and news for every traded player', async () => {
  const f = await factsOf(OFFER);
  const send = buildTradeCard(f, tradeDecision, { '110': [item], '411': [item] });
  assert.deepEqual([send.partner.stance, send.offers[0].verdict, send.offers[0].delta, send.offers[0].counter, send.offers[0].news.map((n) => n.player)], ['rebuilder', 'send', { you: -0.3, partner: 0.3 }, null, ['Bo Bench', 'Tom Tail']]);
  const counter = buildTradeCard(f, { ...tradeDecision, verdict: 'counter', counter: { give: ['110'], get: ['410'] } }, {}).offers[0];
  assert.deepEqual([counter.counter!.get.map((p) => p.name), counter.counter!.delta], [['Tess Tight'], { you: 4.1, partner: -4.1 }]);
  assert.throws(() => buildTradeCard(f, { ...tradeDecision, verdict: 'counter' }, {}), /without a counter-offer/);
  assert.throws(() => buildTradeCard(f, { ...tradeDecision, verdict: 'counter', counter: { give: ['110'], get: ['200'] } }, {}), /200, who is not on Pintail Pride/);
  assert.equal(buildTradeCard(f, { ...tradeDecision, counter: { give: ['110'], get: ['410'] } }, {}).offers[0].counter, null, 'a counter on a non-counter verdict is dropped');
});

test('each code gate passes the good cards', async () => {
  const ff = await factsOf();
  const finder = buildFinderCard(ff, finderDecision, {});
  const ft = await factsOf(OFFER);
  const trade = buildTradeCard(ft, tradeDecision, {});
  for (const [f, c] of [[ff, finder], [ft, trade]] as const) for (const id of ['verdict_one_of', 'give_get_from_my_side']) assert.equal((await gate(id, f, c)).met, 1, id);
});

test('verdict_one_of fails an unknown verdict, a counter without one, too many deals and unranked deals', async () => {
  const ft = await factsOf(OFFER);
  const trade = buildTradeCard(ft, tradeDecision, {});
  const withOffer = (o: object) => ({ ...trade, offers: [{ ...trade.offers[0], ...o }] });
  assert.ok((await gate('verdict_one_of', ft, withOffer({ verdict: 'maybe' }))).met < 1);
  assert.ok((await gate('verdict_one_of', ft, withOffer({ verdict: 'counter' }))).met < 1);
  const ff = await factsOf();
  const finder = buildFinderCard(ff, finderDecision, {});
  const row = finder.suggestions[0];
  assert.equal((await gate('verdict_one_of', ff, { ...finder, suggestions: [{ ...row, rank: 2 }] })).met, 0);
  assert.equal((await gate('verdict_one_of', ff, { ...finder, suggestions: [row, row, row, row].map((r, i) => ({ ...r, rank: i + 1 })) })).met, 0);
});

test('give_get_from_my_side fails a wrong projection, a player on the wrong side, and a different trade than the one asked', async () => {
  const ft = await factsOf(OFFER);
  const trade = buildTradeCard(ft, tradeDecision, {});
  const offer = trade.offers[0];
  const edit = (o: object) => ({ ...trade, offers: [{ ...offer, ...o }] });
  const wrongProj = await gate('give_get_from_my_side', ft, edit({ get: [{ ...offer.get[0], proj: 9.9 }] }));
  assert.match(wrongProj.detail!, /offer1/);
  assert.ok(wrongProj.met < 1);
  assert.ok((await gate('give_get_from_my_side', ft, edit({ give: offer.get, get: offer.give }))).met < 1, 'give and get swapped');
  assert.match((await gate('give_get_from_my_side', ft, edit({ get: [] }))).detail!, /offer1/);
  const other = buildTradeCard(await factsOf({ ...OFFER, get: ['410'] }), tradeDecision, {});
  assert.match((await gate('give_get_from_my_side', ft, other)).detail!, /not the trade that was asked/);
  const ff = await factsOf();
  const finder = buildFinderCard(ff, finderDecision, {});
  const moved = { ...finder, suggestions: [{ ...finder.suggestions[0], partner: { ...finder.suggestions[0].partner, rosterId: 2 } }] };
  assert.match((await gate('give_get_from_my_side', ff, moved)).detail!, /suggestion1/);
});

test('a row\'s judge evidence carries news, designations of both sides and the partner table; a multi-for-one is its own unit', async () => {
  const f = await factsOf({ ...OFFER, give: ['110', '106'], get: ['411', '410'] });
  const c = buildTradeCard(f, tradeDecision, { '110': [item] });
  const ev = await tradeEvidence(f)(null, { content: JSON.stringify(c) });
  const row = ev.row[0].state as unknown as { news: unknown[]; designations: Record<string, string>; league_teams: number; partner: { stance: string; surplus: string[] } };
  assert.deepEqual([row.news.length, Object.keys(row.designations), row.league_teams, row.partner.stance, row.partner.surplus], [1, ['Bo Bench', 'Walt Wade', 'Tom Tail', 'Tess Tight'], 3, 'rebuilder', ['411']]);
  assert.deepEqual(ev.consolidation.map((u) => u.id), ['offer1']);
  assert.equal((await tradeEvidence(await factsOf(OFFER))(null, { content: JSON.stringify(buildTradeCard(await factsOf(OFFER), tradeDecision, {})) })).consolidation.length, 0);
});

test('news is searched once per player and skipped when there is no source', async () => {
  const f = await factsOf(OFFER);
  const searched: string[] = [];
  const players = [f.mine.players['110'], f.partners[0].players['411'], f.mine.players['110']];
  assert.deepEqual(Object.keys(await fetchNews(async (p) => (searched.push(p.name), [item]), players, AT)), ['110', '411']);
  assert.deepEqual(searched, ['Bo Bench', 'Tom Tail']);
  assert.deepEqual(await fetchNews(undefined, players, AT), {});
});

async function run<D>(workflow: ReturnType<typeof tradeWorkflow>, input: object, decision: D) {
  const judged: string[] = [];
  const responses: FauxResponseStep[] = [
    () => fauxAssistantMessage(JSON.stringify(decision)),
    ...Array.from({ length: 40 }, () => (c: { messages: Array<{ content: unknown }> }) => {
      const asked = String(c.messages.at(-1)!.content);
      judged.push(asked);
      return fauxAssistantMessage(JSON.stringify({ choice: /collusion|12-team value chart/.test(asked) ? 'no' : 'yes', confidence: 1, reason: 'ok' }));
    }),
  ];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-trades-'));
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'You are the trade analyst.', extensions: [workflowExtension([workflow])] } });
  try {
    const outcome = await runWorkflow(s.harness, s.root, workflow, JSON.stringify(input));
    assert.equal(outcome.status, 'completed', JSON.stringify(outcome));
    const sealed = (await s.harness.snapshot(Artifacts, s.root.id, ctx))!.kinds['sleeper.cards'].versions[0];
    return { sealed, judged, verdict: sealed.verdict as { passed: boolean; rubric: string; gatesFailed: string[]; notes: string[]; checks: Array<{ id: string }> } };
  } finally {
    await s.close();
    await rm(dir, { recursive: true, force: true });
  }
}
const deps = (news?: (p: { name: string }) => Promise<unknown[]>) => ({ client: clientOf(), snapshot: snapshotOf(), now: () => AT, news: news as never });

test('the trade workflow seals a card whose news the code searched for each traded player, and the judge passes it', async () => {
  const searched: string[] = [];
  const { sealed, verdict, judged } = await run(tradeWorkflow(deps(async (p) => (searched.push(p.name), [item]))), OFFER, tradeDecision);
  const c = JSON.parse(sealed.content) as TradeCard;
  assert.deepEqual([c.kind, c.partner.team, c.offers[0].verdict, c.offers[0].news.length, searched], ['trade', 'Pintail Pride', 'send', 2, ['Bo Bench', 'Tom Tail']]);
  assert.deepEqual([verdict.rubric, verdict.gatesFailed, verdict.passed, verdict.notes], ['sleeper.trade@1', [], true, []]);
  assert.ok(verdict.checks.some((k) => k.id === 'news_per_traded_player'));
  assert.ok(judged.some((q) => /one line item that decides/.test(q)));
});

test('without a news source the trade verdict says so and does not score news', async () => {
  const { verdict } = await run(tradeWorkflow(deps()), OFFER, tradeDecision);
  assert.deepEqual([verdict.passed, verdict.notes], [true, ['news not checked: no news source']]);
  assert.ok(!verdict.checks.some((k) => k.id === 'news_per_traded_player'));
});

test('the finder workflow searches news only for the players the model picked and seals a ranked card', async () => {
  const searched: string[] = [];
  const { sealed, verdict } = await run(finderWorkflow(deps(async (p) => (searched.push(p.name), [item]))), FINDER, finderDecision);
  const c = JSON.parse(sealed.content) as FinderCard;
  assert.deepEqual([c.kind, c.suggestions.map((s) => s.get.name), searched], ['trade-finder', ['Tom Tail'], ['Bo Bench', 'Tom Tail']]);
  assert.deepEqual([verdict.rubric, verdict.gatesFailed, verdict.passed], ['sleeper.trade@1', [], true]);
});

test('the weekly finder run starts two hours after waivers process and stays useful a week; a league without a processing day has none', () => {
  const real = { waiver_day_of_week: 2, daily_waivers: 0 };
  const run = finderRun(AT, real)!;
  assert.deepEqual([new Date(run.at).toISOString(), new Date(run.deadline).toISOString()], ['2026-11-11T10:00:00.000Z', '2026-11-18T10:00:00.000Z']);
  assert.equal(finderRun(AT, {}), null);
  assert.equal(finderRun(AT, undefined), null);
  assert.equal(finderKey('L1', 9), 'sleeper:L1:9:trade-finder');
  assert.equal(tradeKey('L1', 9, { partner: '3', give: ['b', 'a'], get: ['c'] }), 'sleeper:L1:9:trade:3:a+b:c');
});

test('the schedules service lists the finder run once however often it registers, under the week session key', async () => {
  const schedules = createSchedulesService({ serviceToken: 's', distributorUrl: 'http://unused', dbPath: ':memory:' });
  await new Promise<void>((r) => schedules.listen(0, r));
  const url = `http://127.0.0.1:${(schedules.address() as AddressInfo).port}`;
  try {
    const now = () => AT;
    await registerFinderSchedules({ url, token: 's' }, clientOf(), 'L1', 'mallard', now);
    const ids = await registerFinderSchedules({ url, token: 's' }, clientOf(), 'L1', 'mallard', now);
    assert.deepEqual(ids, ['sleeper/trade-finder/L1/2026-11-11']);
    const { schedules: listed } = await (await fetch(`${url}/schedules`, { headers: { authorization: 'Bearer s' } })).json();
    assert.equal(listed.length, 1);
    assert.partialDeepStrictEqual(listed[0], { id: ids[0], agent: 'sleeper-trade-finder', times: ['2026-11-11T10:00:00.000Z'], deadline: '2026-11-18T10:00:00.000Z', sessionKey: 'sleeper:L1:9:trade-finder', input: JSON.stringify(FINDER) });
    const noDay = { ...fixtures, '/v1/league/L1': { ...fixtures['/v1/league/L1'], settings: { waiver_type: 0 } } };
    assert.deepEqual(await registerFinderSchedules({ url, token: 's' }, clientOf(noDay), 'L1', 'mallard', now), []);
    const offseason = { ...fixtures, '/v1/state/nfl': { ...fixtures['/v1/state/nfl'], season_type: 'off' } };
    assert.deepEqual(await registerFinderSchedules({ url, token: 's' }, clientOf(offseason), 'L1', 'mallard', now), []);
  } finally {
    schedules.stop();
  }
});

test('the trade jobs start on demand: the finder under the week key, a trade under its own key, and a bad trade is a 400', async () => {
  const calls: Array<{ url: string; body: { input: string } }> = [];
  const fake = (async (u: string, init: { body: string }) => (calls.push({ url: u, body: JSON.parse(init.body) }), new Response('{}', { status: 202 }))) as unknown as typeof fetch;
  const service = createSleeperService({ serviceToken: 't', dbPath: ':memory:', client: clientOf(), distributorUrl: 'http://d', owner: 'mallard', fetch: fake, now: () => new Date(AT) });
  await new Promise<void>((r) => service.listen(0, r));
  const post = (path: string, method = 'POST') => fetch(`http://127.0.0.1:${(service.address() as AddressInfo).port}${path}`, { method, headers: { authorization: 'Bearer t' } });
  try {
    const finder = await post('/leagues/L1/jobs/trade-finder');
    assert.deepEqual([finder.status, await finder.json()], [202, { sessionKey: 'sleeper:L1:9:trade-finder', status: 202 }]);
    assert.equal(calls[0].url, 'http://d/sessions/sleeper%3AL1%3A9%3Atrade-finder/invocations?agent=sleeper-trade-finder');
    assert.deepEqual(JSON.parse(calls[0].body.input), FINDER);
    const trade = await post('/leagues/L1/jobs/trade?partner=3&give=110&get=411,410');
    assert.deepEqual([trade.status, (await trade.json()).sessionKey], [202, 'sleeper:L1:9:trade:3:110:410+411']);
    assert.equal(calls[1].url, 'http://d/sessions/sleeper%3AL1%3A9%3Atrade%3A3%3A110%3A410%2B411/invocations?agent=sleeper-trade');
    assert.deepEqual(JSON.parse(calls[1].body.input), { ...FINDER, partner: '3', give: ['110'], get: ['411', '410'] });
    assert.equal((await post('/leagues/L1/jobs/trade?partner=3&give=110')).status, 400);
    assert.equal((await post('/leagues/L1/jobs/trade-finder?week=x')).status, 400);
    assert.equal((await post('/leagues/L1/jobs/trade', 'GET')).status, 405);
    assert.equal(calls.length, 2);
  } finally {
    service.stop();
  }
});
