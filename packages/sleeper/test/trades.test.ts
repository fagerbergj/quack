import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, type FauxResponseStep } from '@earendil-works/pi-ai';
import { createSchedulesService } from '@quack/schedules';
import { Artifacts, serveApp } from '@quack/sdk';
import { readSession } from '@quack/eval';
import { buildEvidence } from '@quack/judge';
import { replayed } from './replayed.ts';
import { openSession } from '@quack/session';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { runWorkflow, workflowExtension } from '@quack/workflows';
import {
  buildFinderCard, liveLeague, buildTradeFacts, createSleeperClient, createSleeperService, finderKey, finderRun, finderWorkflow, registerFinderSchedules, tradeDeclaration, tradeInputsOf, tradeRubric,
  type FinderCard,
} from '../src/index.ts';
import type { FinderDecision } from '../src/trades/card.ts';
import { dealable } from '../src/trades/facts.ts';
import { newsFor } from '../src/lineup/news.ts';
import { refusedRun } from './refused.ts';

type Fixtures = Record<string, any>;
const fixtures = JSON.parse(readFileSync(new URL('./fixtures/trades.json', import.meta.url), 'utf8')) as Fixtures;
const AT = Date.parse('2026-11-06T15:00:00Z');
const FINDER = { league: 'L1', week: 9, owner: 'mallard' };

const clientOf = (fx: Fixtures = fixtures) => createSleeperClient({
  fetch: (async (input: string) => {
    const path = new URL(input).pathname;
    return path in fx ? new Response(JSON.stringify(fx[path])) : new Response('null');
  }) as unknown as typeof fetch,
  now: () => AT,
});
const snapshotOf = (fx: Fixtures = fixtures) => (league: string) => liveLeague(clientOf(fx), league);
const factsOf = async (input: Parameters<typeof buildTradeFacts>[2] = FINDER) => buildTradeFacts({ client: clientOf() }, await snapshotOf()('L1'), input);

const item = { title: 'Depth chart report', url: 'https://www.nfl.com/n', date: new Date(AT - 5 * 3_600_000).toISOString() };
const finderDecision: FinderDecision = {
  suggestions: [{ partner: 3, give: '110', get: '411', partner_need: 'needs WR', note: 'Pintail Pride is 2-6 and short at WR; Tom Tail is their third tight end.' }],
  summary: 'Offer Bo Bench to Pintail Pride for Tom Tail.',
};

test('no trade check is a code gate: the card builder already fixes the sides and figures', () => {
  assert.deepEqual(tradeRubric.criteria.flatMap((k) => k.checks).filter((k) => k.weight === 'gate' || k.code).map((k) => k.id), []);
});

test('the partner table holds record, standing, stance, needs and surplus from the recorded week', async () => {
  const f = await factsOf();
  assert.deepEqual([f.teams, f.playoffTeams], [3, 2]);
  assert.deepEqual([f.mine.rank, f.mine.stance, f.mine.surplus, f.mine.needs], [2, 'contender', ['110'], { TE: 1 }]);
  assert.deepEqual(f.partners.map((p) => [p.rosterId, p.record, p.rank, p.stance, p.surplus]), [[2, '6-2', 1, 'contender', ['400']], [3, '2-6', 3, 'rebuilder', ['411']]]);
  assert.deepEqual(f.partners[1].needs, { RB: 3, WR: 2, K: 1, DEF: 1 });
  assert.equal(f.partners[0].players['400'].role, 'bench');
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

test('the news check keeps the legacy row unit; the partner check reads each suggestion with its partner row', async () => {
  const f = await factsOf();
  const c = buildFinderCard(f, finderDecision, { '110': [item] });
  const ev = (await buildEvidence(tradeDeclaration, tradeInputsOf(f, JSON.stringify(c)))).evidence;
  const row = ev.news[0].state as unknown as { news: unknown[]; designations: Record<string, string>; league_teams: string; partner: { stance: string; surplus: string[] } };
  assert.deepEqual(Object.keys(row), ['row', 'news', 'designations', 'league_teams', 'you', 'partner']);
  assert.deepEqual([row.news.length, Object.keys(row.designations), row.league_teams, row.partner.stance, row.partner.surplus], [1, ['Bo Bench', 'Tom Tail'], '3', 'rebuilder', ['411']]);
  const s = ev.suggestions[0].state.suggestion as { partner: { team: string; needs: object }; note: string };
  assert.deepEqual([s.partner.team, s.partner.needs, s.note], ['Pintail Pride', { RB: 3, WR: 2, K: 1, DEF: 1 }, finderDecision.suggestions[0].note]);
  // News is searched for every dealable player, so a row without any is a job with no source; it is not asked about news.
  assert.deepEqual((await buildEvidence(tradeDeclaration, tradeInputsOf(f, JSON.stringify(buildFinderCard(f, finderDecision, {}))))).evidence.news, []);
});

test('the finder judge asks the configured decision model', () => {
  const ref = { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 };
  const step = finderWorkflow({ client: clientOf(), decisionModel: ref }).steps.find((s) => s.name === 'card')!;
  assert.equal(step.kind, 'judged');
  const { options } = (step as Extract<typeof step, { kind: 'judged' }>).judge({ prior: { facts: {} } } as never);
  assert.deepEqual(options.decisionModel, ref);
});

test('news is searched once per player and skipped when there is no source', async () => {
  const f = await factsOf();
  const searched: string[] = [];
  const players = [f.mine.players['110'], f.partners[1].players['411'], f.mine.players['110']];
  assert.deepEqual(Object.keys(await newsFor(async (p) => (searched.push(p.name), [item]), players, AT)), ['110', '411']);
  assert.deepEqual(searched, ['Bo Bench', 'Tom Tail']);
  assert.deepEqual(await newsFor(undefined, players, AT), {});
});

async function run<D>(workflow: ReturnType<typeof finderWorkflow>, input: object, decision: D) {
  const judged: string[] = [];
  const asked: string[] = [];
  const responses: FauxResponseStep[] = [
    (c: { messages: Array<{ content: unknown }> }) => (asked.push(String(c.messages.at(-1)!.content)), fauxAssistantMessage(JSON.stringify(decision))),
    ...Array.from({ length: 40 }, () => (c: { messages: Array<{ content: unknown }> }) => {
      const asked = String(c.messages.at(-1)!.content);
      judged.push(asked);
      return fauxAssistantMessage(JSON.stringify({ choice: 'yes', confidence: 1, reason: 'ok' }));
    }),
  ];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-trades-'));
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'You are the trade finder.', extensions: [workflowExtension([workflow])] } });
  try {
    const outcome = await runWorkflow(s.harness, s.root, workflow, JSON.stringify(input));
    assert.equal(outcome.status, 'completed', JSON.stringify(outcome));
    const sealed = (await s.harness.snapshot(Artifacts, s.root.id, ctx))!.kinds['sleeper.cards'].versions[0];
    await s.close();
    return { record: await readSession(dir), sealed, judged, asked, verdict: sealed.verdict as { passed: boolean; rubric: string; gatesFailed: string[]; notes: string[]; checks: Array<{ id: string; units: number; met: unknown }> } };
  } finally {
    await s.close();
    await rm(dir, { recursive: true, force: true });
  }
}
const deps = (news?: (p: { name: string }) => Promise<unknown[]>) => ({ client: clientOf(), now: () => AT, news: news as never });

test('without a news source the finder verdict has no news to score', async () => {
  const { verdict, record, sealed } = await run(finderWorkflow(deps()), FINDER, finderDecision);
  assert.deepEqual(await replayed(record, sealed), tradeInputsOf(await factsOf(), sealed.content), 'the recording rebuilds the inputs the judge read');
  assert.deepEqual([verdict.passed, verdict.notes], [true, []]);
  assert.deepEqual(verdict.checks.filter((k) => k.id === 'news_per_traded_player').map((k) => [k.units, k.met]), [[0, null]]);
});

test('the finder searches news once for every player a deal may name, gives it to the model before it decides, and seals a ranked card', async () => {
  const searched: string[] = [];
  const { sealed, verdict, asked, record } = await run(finderWorkflow(deps(async (p) => (searched.push(p.name), [{ ...item, title: `${p.name} report` }]))), FINDER, finderDecision);
  assert.deepEqual(await replayed(record, sealed), tradeInputsOf(await factsOf(), sealed.content), 'the recording rebuilds the inputs the judge read');
  const c = JSON.parse(sealed.content) as FinderCard;
  const dealt = dealable(await factsOf()).map((p) => p.name);
  assert.deepEqual([...searched].sort(), [...dealt].sort(), 'each dealable player once, before the decision');
  assert.ok(searched.includes('Bo Bench') && searched.includes('Tom Tail'));
  assert.match(asked[0], /\[Tom Tail report\]\(https:\/\/www\.nfl\.com/, 'the decision prompt carries the news');
  assert.deepEqual([c.kind, c.suggestions.map((s) => s.get.name), c.suggestions[0].news.map((n) => n.player)], ['trade-finder', ['Tom Tail'], ['Bo Bench', 'Tom Tail']]);
  assert.deepEqual([verdict.rubric, verdict.gatesFailed, verdict.passed, verdict.notes], ['sleeper.trade@6', [], true, []]);
  assert.ok(verdict.checks.some((k) => k.id === 'news_per_traded_player'));
});

test('the weekly finder run starts two hours after waivers process and stays useful a week; a league without a processing day has none', () => {
  const real = { waiver_day_of_week: 2, daily_waivers: 0 };
  const run = finderRun(AT, real)!;
  assert.deepEqual([new Date(run.at).toISOString(), new Date(run.deadline).toISOString()], ['2026-11-11T10:00:00.000Z', '2026-11-18T10:00:00.000Z']);
  assert.equal(finderRun(AT, {}), null);
  assert.equal(finderRun(AT, undefined), null);
  assert.equal(finderKey('L1', 9), 'sleeper:L1:9:trade-finder');
});

test('the schedules service lists the finder run once however often it registers, under the week session key', async () => {
  const schedules = createSchedulesService({ distributorUrl: 'http://unused', dbPath: ':memory:' });
  const schedulesServer = serveApp(schedules).listen(0);
  await new Promise((r) => schedulesServer.once('listening', r));
  const url = `http://127.0.0.1:${(schedulesServer.address() as AddressInfo).port}`;
  try {
    const now = () => AT;
    await registerFinderSchedules({ url }, clientOf(), 'L1', 'mallard', now);
    const ids = await registerFinderSchedules({ url }, clientOf(), 'L1', 'mallard', now);
    assert.deepEqual(ids, ['sleeper/trade-finder/L1/2026-11-11']);
    const { schedules: listed } = await (await fetch(url)).json();
    assert.equal(listed.length, 1);
    assert.partialDeepStrictEqual(listed[0], { id: ids[0], agent: 'sleeper-trade-finder', times: ['2026-11-11T10:00:00.000Z'], deadline: '2026-11-18T10:00:00.000Z', sessionKey: 'sleeper:L1:9:trade-finder', input: JSON.stringify(FINDER) });
    const noDay = { ...fixtures, '/v1/league/L1': { ...fixtures['/v1/league/L1'], settings: { waiver_type: 0 } } };
    assert.deepEqual(await registerFinderSchedules({ url }, clientOf(noDay), 'L1', 'mallard', now), []);
    const offseason = { ...fixtures, '/v1/state/nfl': { ...fixtures['/v1/state/nfl'], season_type: 'off' } };
    assert.deepEqual(await registerFinderSchedules({ url }, clientOf(offseason), 'L1', 'mallard', now), []);
  } finally {
    schedulesServer.close();
    schedules.stop();
  }
});

test('the finder starts on demand under the week key', async () => {
  const calls: Array<{ url: string; body: { input: string } }> = [];
  const fake = (async (u: string, init: { body: string }) => (calls.push({ url: u, body: JSON.parse(init.body) }), new Response('{}', { status: 202 }))) as unknown as typeof fetch;
  const service = serveApp(createSleeperService({ dbPath: ':memory:', client: clientOf(), distributorUrl: 'http://d', owner: 'mallard', league: 'L1', fetch: fake, now: () => new Date(AT) }));
  await new Promise<void>((r) => service.listen(0, r));
  const post = (path: string, method = 'POST') => fetch(`http://127.0.0.1:${(service.address() as AddressInfo).port}${path}`, { method, headers: { authorization: 'Bearer t' } });
  try {
    const finder = await post('/trade-finder');
    assert.deepEqual([finder.status, await finder.json()], [202, { sessionKey: 'sleeper:L1:9:trade-finder', status: 202 }]);
    assert.equal(calls[0].url, 'http://d/sessions/sleeper%3AL1%3A9%3Atrade-finder/invocations?agent=sleeper-trade-finder');
    assert.deepEqual(JSON.parse(calls[0].body.input), FINDER);
    assert.equal((await post('/trade-finder?week=x')).status, 400);
    assert.equal((await post('/trade-finder', 'GET')).status, 405);
    assert.equal(calls.length, 1);
  } finally {
    service.stop();
  }
});

test('a model host refusing the finder call fails the card step with the host\'s error', async () => {
  const { outcome, failed } = await refusedRun(finderWorkflow(deps()), FINDER);
  assert.equal(outcome.status, 'failed');
  assert.deepEqual([failed?.name, failed?.error], ['card', 'step card: model call failed: 503 no slot free']);
});
