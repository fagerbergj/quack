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
  buildSnapshot, buildWaiverFacts, buildWaiversCard, createSleeperClient, createSleeperService, nextProcessing, registerWaiversSchedules, waiversEvidence, waiversKey, waiversRubric, waiversRun, waiversWorkflow,
  type WaiverFacts, type WaiversCard,
} from '../src/index.ts';
import { closeCalls } from '../src/waivers/facts.ts';
import type { WaiverDecision } from '../src/waivers/card.ts';

type Fixtures = Record<string, any>;
const base = JSON.parse(readFileSync(new URL('./fixtures/waivers.json', import.meta.url), 'utf8')) as Fixtures;
const AT = Date.parse('2026-11-06T15:00:00Z');
const INPUT = { league: 'L1', week: 9, owner: 'mallard' };

const faab: Fixtures = { ...base, '/v1/league/L1': { ...base['/v1/league/L1'], settings: { ...base['/v1/league/L1'].settings, waiver_type: 2 } } };

const clientOf = (fixtures: Fixtures) => createSleeperClient({
  fetch: (async (input: string) => {
    const path = new URL(input).pathname;
    return path in fixtures ? new Response(JSON.stringify(fixtures[path])) : new Response('null');
  }) as unknown as typeof fetch,
  now: () => AT,
});
const snapshotOf = (fixtures: Fixtures) => async (league: string) => ({ data: await buildSnapshot(clientOf(fixtures), league) });
const factsOf = async (fixtures: Fixtures = base, news?: Parameters<typeof buildWaiverFacts>[0]['news']) => buildWaiverFacts({ client: clientOf(fixtures), news }, await snapshotOf(fixtures)('L1'), INPUT, AT);

// What a good model returns: two claims against bench drops, the injured widely-owned receiver passed over.
const decision: WaiverDecision = {
  claims: [
    { add: '300', drop: '103', bid: 18, why: 'Felix Free has the lead-back role in Kansas City; Dan Dash (4.0) is a bench dead spot.' },
    { add: '302', drop: '110', bid: 5, why: 'Wally Wire covers the WR bye in week 10; Bo Bench is bench depth.' },
  ],
  passed: [{ playerId: '303', why: 'Ike Hurt is Out.' }],
  summary: 'Claim Felix Free first, then Wally Wire.',
};
const rollingDecision: WaiverDecision = { ...decision, claims: decision.claims.map(({ bid: _bid, ...c }) => c) };

const card = async (fixtures: Fixtures = base, d: WaiverDecision = fixtures === base ? rollingDecision : decision) => {
  const f = await factsOf(fixtures);
  return { f, card: buildWaiversCard(f, d) };
};
const gate = async (id: string, f: WaiverFacts, c: unknown) => {
  const check = waiversRubric.criteria.flatMap((k) => k.checks).find((k) => k.id === id)!;
  const content = JSON.stringify(c);
  return check.code!(content, await waiversEvidence(f)(null, { content }));
};
const GATES = ['context_fields_match', 'drop_role_matches_roster', 'widely_owned_accounted', 'faab_language_on_rolling'];

test('facts hold the waiver context, the competition table, bucketed free agents and the roster roles from the recorded week', async () => {
  const f = await factsOf();
  assert.deepEqual([f.waiverType, f.myPriority, f.faabLeft, f.teams], ['rolling', 2, null, 3]);
  assert.deepEqual(f.table.map((t) => [t.team, t.priority, t.faabLeft]), [['Mallard Mayhem', 2, null], ['teal', 1, null], ['Pintail Pride', 3, null]]);
  assert.deepEqual(f.table[0].needs, { TE: 1 });
  assert.deepEqual(f.table[1].needs, { RB: 2, WR: 2, TE: 2, K: 1, DEF: 1 });
  assert.deepEqual(f.table[0].byes, [{ week: 10, pos: 'WR', out: 3 }]);
  assert.deepEqual(f.buckets.RB, ['300', '301']);
  assert.deepEqual(f.buckets.WR, ['302', '303']);
  assert.deepEqual(f.widely, ['300', '302', '303']);
  assert.ok(!('310' in f.free) && !('311' in f.free), 'a retired player and a lineman are not free agents');
  assert.deepEqual(f.free['300'], { id: '300', name: 'Felix Free', pos: 'RB', team: 'KC', inj: null, proj: 12.4, bye: null, owned: 61.2, adds: 4200 });
  assert.equal(f.free['303'].inj, 'Out');
  assert.deepEqual([f.roster['101'].role, f.roster['103'].role, f.roster['106'].role], ['starter', 'bench', 'starter']);
  assert.equal(f.roster['106'].bye, 10);
});

test('FAAB shows only in a FAAB league, as what remains of the budget', async () => {
  const f = await factsOf(faab);
  assert.deepEqual([f.waiverType, f.faabLeft], ['faab', 70]);
  assert.deepEqual(f.table.map((t) => t.faabLeft), [70, 20, 100]);
  assert.equal((await factsOf()).table.every((t) => t.faabLeft === null), true);
});

test('an unknown waiver type fails the facts rather than guessing a currency', async () => {
  const odd = { ...base, '/v1/league/L1': { ...base['/v1/league/L1'], settings: { waiver_type: 7 } } };
  await assert.rejects(factsOf(odd), /unknown waiver_type 7/);
});

test('the card carries the tools figures, the league currency and the claim order', async () => {
  const rolling = (await card()).card;
  assert.deepEqual(rolling.candidates.map((c) => [c.rank, c.player.id, c.proj, c.owned_pct, c.adds_24h, c.drop.player.id, c.drop.role]), [[1, '300', 12.4, 61.2, 4200, '103', 'bench'], [2, '302', 11.1, 12, 150, '110', 'bench']]);
  assert.deepEqual(rolling.candidates[0].size, { kind: 'priority', position: 2, of: 3 });
  assert.deepEqual(rolling.also_checked.map((a) => a.player.id), ['303']);
  const bid = (await card(faab)).card;
  assert.deepEqual(bid.candidates.map((c) => c.size), [{ kind: 'faab', bid: 18, left: 70, pct: 26 }, { kind: 'faab', bid: 5, left: 70, pct: 7 }]);
});

test('a bad decision fails the card step instead of reaching the judge', async () => {
  const f = await factsOf(faab);
  const claim = decision.claims[0];
  assert.throws(() => buildWaiversCard(f, { ...decision, claims: [{ ...claim, bid: 71 }] }), /bid 71.*70 left/);
  assert.throws(() => buildWaiversCard(f, { ...decision, claims: [{ ...claim, bid: undefined }] }), /bid undefined/);
  assert.throws(() => buildWaiversCard(f, { ...decision, claims: [{ ...claim, add: '999' }] }), /not a free agent/);
  assert.throws(() => buildWaiversCard(f, { ...decision, claims: [{ ...claim, drop: '300' }] }), /not on the roster/);
  assert.throws(() => buildWaiversCard(f, { ...decision, claims: [claim, claim] }), /more than once/);
});

test('a card with no claims says so, and a pass for a player already claimed is not repeated', async () => {
  const f = await factsOf();
  assert.equal(buildWaiversCard(f, { claims: [], passed: [], summary: 'All set.' }).summary, 'No claims: nothing on the wire beats the roster.');
  const c = buildWaiversCard(f, { ...rollingDecision, passed: [{ playerId: '300', why: 'x' }, { playerId: '303', why: 'Out.' }, { playerId: 'nobody', why: 'x' }] });
  assert.deepEqual(c.also_checked.map((a) => a.player.id), ['303']);
});

test('each code gate passes the good card in either league', async () => {
  for (const fixtures of [base, faab]) {
    const { f, card: c } = await card(fixtures);
    for (const id of GATES) assert.equal((await gate(id, f, c)).met, id === 'faab_language_on_rolling' ? 0 : 1, id);
  }
});

test('context_fields_match fails a card whose priority, type, teams or balance differ from the tools', async () => {
  const { f, card: c } = await card(faab);
  for (const [field, wrong] of [['my_priority', 1], ['waiver_type', 'rolling'], ['teams', 12], ['faab_left', 100]] as const) {
    const r = await gate('context_fields_match', f, { ...c, [field]: wrong });
    assert.ok(r.met < 1, field);
    assert.match(r.detail!, new RegExp(field));
  }
});

test('drop_role_matches_roster fails a mislabeled role and a starter drop not stated as one', async () => {
  const { f, card: c } = await card();
  const relabel = { ...c, candidates: c.candidates.map((r, i) => (i ? r : { ...r, drop: { ...r.drop, role: 'ir' as const } })) };
  assert.match((await gate('drop_role_matches_roster', f, relabel)).detail!, /Dan Dash/);
  const starter = buildWaiversCard(f, { ...rollingDecision, claims: [{ add: '300', drop: '102', why: 'Felix Free has the lead-back role.' }] });
  assert.equal(starter.candidates[0].drop.role, 'starter');
  assert.match((await gate('drop_role_matches_roster', f, starter)).detail!, /Rob Rowe/);
  const said = buildWaiversCard(f, { ...rollingDecision, claims: [{ add: '300', drop: '102', why: 'Drops your starting RB2, Questionable.' }] });
  assert.equal((await gate('drop_role_matches_roster', f, said)).met, 1);
});

test('widely_owned_accounted fails a card that neither claims nor passes over a player above the cut', async () => {
  const { f } = await card();
  const forgot = buildWaiversCard(f, { ...rollingDecision, passed: [] });
  const r = await gate('widely_owned_accounted', f, forgot);
  assert.ok(r.met < 1);
  assert.match(r.detail!, /Ike Hurt/);
  assert.equal((await gate('widely_owned_accounted', f, buildWaiversCard(f, { ...rollingDecision, passed: [{ playerId: '303', why: 'Out.' }] }))).met, 1);
});

test('faab_language_on_rolling penalizes FAAB figures in a rolling league and not in a FAAB one', async () => {
  const { f, card: c } = await card();
  const talks = { ...c, candidates: c.candidates.map((r, i) => (i ? r : { ...r, why: 'Felix Free is worth a $20 bid.' })) };
  const r = await gate('faab_language_on_rolling', f, talks);
  assert.equal(r.met, 1);
  assert.match(r.detail!, /rolling league/);
  assert.equal((await gate('faab_language_on_rolling', f, { ...c, summary: 'Bid 30% of remaining FAAB.' })).met, 1);
  const { f: ff, card: fc } = await card(faab);
  assert.equal((await gate('faab_language_on_rolling', ff, { ...fc, summary: 'Bid 30% of remaining FAAB.' })).met, 0);
});

const item = { title: 'Backfield report', url: 'https://www.nfl.com/n', date: new Date(AT - 5 * 3_600_000).toISOString() };

test('news is searched for close calls only and each row carries the links of its add and its drop, attached by code', async () => {
  const searched: string[] = [];
  const f = await factsOf(base, async (p) => (searched.push(p.name), [item]));
  const close = closeCalls(f).map((p) => p.id).sort();
  assert.deepEqual(Object.keys(f.news).sort(), close);
  assert.ok(searched.includes('Felix Free') && searched.includes('Dan Dash'));
  assert.ok(!searched.includes('Quinn Quill'), 'a starter nobody is dropping is not searched');
  const c = buildWaiversCard(f, rollingDecision);
  assert.deepEqual(c.candidates[0].news.map((n) => [n.player, n.url]), [['Felix Free', item.url], ['Dan Dash', item.url]]);
});

test('a row\'s judge evidence carries its news, the designations of both players and the league currency', async () => {
  const f = { ...(await factsOf()), news: { '300': [item] } };
  const c = buildWaiversCard(f, rollingDecision);
  const ev = await waiversEvidence(f)(null, { content: JSON.stringify(c) });
  const row = ev.row[0].state as unknown as { news: Array<{ url: string }>; designations: Record<string, string>; waiver_type: string; bye_exposure: unknown[] };
  assert.deepEqual([row.news.map((n) => n.url), row.designations, row.waiver_type], [[item.url], { 'Felix Free': 'none', 'Dan Dash': 'none' }, 'rolling']);
  assert.equal(row.bye_exposure.length, 1);
  assert.deepEqual(ev.close.map((u) => u.id), ['claim1']);
  assert.equal(ev.row.length, 2);
});

async function runWaivers(fixtures: Fixtures, d: WaiverDecision, news?: Parameters<typeof buildWaiverFacts>[0]['news']) {
  const judged: string[] = [];
  const responses: FauxResponseStep[] = [
    () => fauxAssistantMessage(JSON.stringify(d)),
    ...Array.from({ length: 40 }, () => (c: { messages: Array<{ content: unknown }> }) => {
      const asked = String(c.messages.at(-1)!.content);
      judged.push(asked);
      return fauxAssistantMessage(JSON.stringify({ choice: /trending or ownership number given|share a drop as a mistake/.test(asked) ? 'no' : 'yes', confidence: 1, reason: 'ok' }));
    }),
  ];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-waivers-'));
  const workflow = waiversWorkflow({ client: clientOf(fixtures), snapshot: snapshotOf(fixtures), now: () => AT, news });
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'You are the waiver scout.', extensions: [workflowExtension([workflow])] } });
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

test('the workflow seals a rolling-league card and the judge passes it, sized in queue position and saying news was not checked', async () => {
  const { sealed, judged, verdict } = await runWaivers(base, rollingDecision);
  const c = JSON.parse(sealed.content) as WaiversCard;
  assert.deepEqual([c.kind, c.waiver_type, c.candidates.map((r) => r.player.name)], ['waivers', 'rolling', ['Felix Free', 'Wally Wire']]);
  assert.deepEqual([verdict.rubric, verdict.gatesFailed, verdict.passed, verdict.notes], ['sleeper.waivers@1', [], true, ['close-call news not checked: no news source']]);
  assert.ok(!verdict.checks.some((k) => k.id === 'close_call_news'));
  assert.ok(verdict.checks.some((k) => k.id === 'faab_language_on_rolling'));
  assert.ok(judged.some((q) => /queue position spent/.test(q)));
});

test('the workflow seals a FAAB card with its bids, and with a news source the close calls are scored', async () => {
  const { sealed, verdict, judged } = await runWaivers(faab, decision, async () => [item]);
  assert.deepEqual((JSON.parse(sealed.content) as WaiversCard).candidates.map((r) => r.size.kind), ['faab', 'faab']);
  assert.deepEqual([verdict.rubric, verdict.gatesFailed, verdict.passed, verdict.notes], ['sleeper.waivers@1', [], true, []]);
  assert.ok(verdict.checks.some((k) => k.id === 'close_call_news'));
  assert.ok(judged.some((q) => /close top claim or drop/.test(q)));
});

test('a rolling-league card that talks FAAB gets the pitfall applied by the judge', async () => {
  const talks = { ...rollingDecision, summary: 'Bid 30% of remaining FAAB on Felix Free.' };
  const { verdict } = await runWaivers(base, talks);
  const hit = (verdict as unknown as { checks: Array<{ id: string; met: number }> }).checks.find((k) => k.id === 'faab_language_on_rolling')!;
  assert.equal(hit.met, 1);
});

test('the next processing time follows the league weekday and hour in New York time, across DST', () => {
  // Wednesday 3am EST, after the November change.
  assert.equal(new Date(nextProcessing(AT, 3, 3)).toISOString(), '2026-11-11T08:00:00.000Z');
  // The same wall time on a Wednesday still in EDT.
  assert.equal(new Date(nextProcessing(Date.parse('2026-10-27T12:00:00Z'), 3, 3)).toISOString(), '2026-10-28T07:00:00.000Z');
  // At the processing instant itself the next one is a week out.
  assert.equal(new Date(nextProcessing(Date.parse('2026-11-11T08:00:00Z'), 3, 3)).toISOString(), '2026-11-18T08:00:00.000Z');
});

test('the run starts 12 hours before processing and is due an hour ahead of it; a league without a processing day has none', () => {
  const run = waiversRun(AT, { waiver_day_of_week: 3, daily_waivers_hour: 3 })!;
  assert.deepEqual([new Date(run.at).toISOString(), new Date(run.deadline).toISOString()], ['2026-11-10T20:00:00.000Z', '2026-11-11T07:00:00.000Z']);
  assert.equal(waiversRun(AT, {}), null);
  assert.equal(waiversRun(AT, undefined), null);
  assert.equal(waiversKey('L1', 9), 'sleeper:L1:9:waivers');
});

test('the schedules service lists the waivers run with its session key and deadline, once however often it registers', async () => {
  const schedules = createSchedulesService({ serviceToken: 's', distributorUrl: 'http://unused', dbPath: ':memory:' });
  await new Promise<void>((r) => schedules.listen(0, r));
  const url = `http://127.0.0.1:${(schedules.address() as AddressInfo).port}`;
  try {
    const now = () => AT;
    await registerWaiversSchedules({ url, token: 's' }, clientOf(base), 'L1', 'mallard', now);
    const ids = await registerWaiversSchedules({ url, token: 's' }, clientOf(base), 'L1', 'mallard', now);
    assert.deepEqual(ids, ['sleeper/waivers/L1/9']);
    const { schedules: listed } = await (await fetch(`${url}/schedules`, { headers: { authorization: 'Bearer s' } })).json();
    assert.equal(listed.length, 1);
    assert.partialDeepStrictEqual(listed[0], { id: 'sleeper/waivers/L1/9', agent: 'sleeper-waivers', times: ['2026-11-10T20:00:00.000Z'], deadline: '2026-11-11T07:00:00.000Z', sessionKey: 'sleeper:L1:9:waivers', input: JSON.stringify(INPUT) });
    const noDay = { ...base, '/v1/league/L1': { ...base['/v1/league/L1'], settings: { waiver_type: 0 } } };
    assert.deepEqual(await registerWaiversSchedules({ url, token: 's' }, clientOf(noDay), 'L1', 'mallard', now), []);
  } finally {
    schedules.stop();
  }
});

test('the waivers job starts on demand under the week session key, beside the lineup route', async () => {
  const calls: Array<{ url: string; body: { input: string; requestId: string } }> = [];
  const fake = (async (u: string, init: { body: string }) => (calls.push({ url: u, body: JSON.parse(init.body) }), new Response('{}', { status: 202 }))) as unknown as typeof fetch;
  let tick = 0;
  const service = createSleeperService({ serviceToken: 't', dbPath: ':memory:', client: clientOf(base), distributorUrl: 'http://d', owner: 'mallard', fetch: fake, now: () => new Date(AT + tick++) });
  await new Promise<void>((r) => service.listen(0, r));
  const post = (path: string, method = 'POST') => fetch(`http://127.0.0.1:${(service.address() as AddressInfo).port}${path}`, { method, headers: { authorization: 'Bearer t' } });
  try {
    const first = await post('/leagues/L1/jobs/waivers');
    assert.equal(first.status, 202);
    assert.deepEqual(await first.json(), { sessionKey: 'sleeper:L1:9:waivers', status: 202 });
    await post('/leagues/L1/jobs/waivers?week=9');
    assert.equal(calls[0].url, 'http://d/sessions/sleeper%3AL1%3A9%3Awaivers/invocations?agent=sleeper-waivers');
    assert.deepEqual(JSON.parse(calls[0].body.input), INPUT);
    assert.notEqual(calls[0].body.requestId, calls[1].body.requestId);
    assert.equal((await post('/leagues/L1/jobs/waivers?week=x')).status, 400);
    assert.equal((await post('/leagues/L1/jobs/waivers', 'GET')).status, 405);
  } finally {
    service.stop();
  }
});
