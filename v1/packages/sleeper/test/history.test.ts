import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, type FauxResponseStep } from '@earendil-works/pi-ai';
import { Artifacts } from '@quack/sdk';
import { openSession } from '@quack/session';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { runWorkflow, workflowExtension } from '@quack/workflows';
import {
  buildHistoryCard, buildHistoryFacts, createSleeperClient, createSleeperService, DEFINITIONS, historyEvidence, historyKey, historyRubric, historyWorkflow, verdictOf,
  type HistoryCard, type HistoryFacts,
} from '../src/index.ts';
import type { HistoryDecision } from '../src/history/card.ts';

type Fixtures = Record<string, any>;
const fixtures = JSON.parse(readFileSync(new URL('./fixtures/history.json', import.meta.url), 'utf8')) as Fixtures;
const INPUT = { league: 'H2', week: 3, owner: 'mallard' };

const clientOf = (fx: Fixtures = fixtures) => createSleeperClient({
  fetch: (async (input: string) => {
    const path = new URL(input).pathname;
    return path in fx ? new Response(JSON.stringify(fx[path])) : new Response('null');
  }) as unknown as typeof fetch,
  now: () => 0,
});
const factsOf = (input: Parameters<typeof buildHistoryFacts>[1] = INPUT, fx: Fixtures = fixtures) => buildHistoryFacts(clientOf(fx), input);

const decision: HistoryDecision = {
  summary: 'Two seasons in: 2025 lost two close games while leaving 21 points on the bench; 2026 is two weeks old.',
  seasons: [{ season: '2025', text: 'Efficiency 89.4%; the week 3 lineup left 11 points, and week 1 was lost by 4 to the champion.' }],
};

const gate = async (f: HistoryFacts, c: unknown) => {
  const check = historyRubric.criteria.flatMap((k) => k.checks).find((k) => k.id === 'figures_from_tools')!;
  const content = JSON.stringify(c);
  return check.code!(content, await historyEvidence(f)(null, { content }));
};

test('the chain walks every season, newest first, and builds record, points, ranks, efficiency and the champion from the tools', async () => {
  const f = await factsOf();
  assert.deepEqual([f.team, f.seasons.map((s) => s.season)], ['Mallard Mayhem', ['2026', '2025']]);
  const { weeks: _w, draft: _d, ...y25 } = f.seasons[1];
  assert.deepEqual(y25, { season: '2025', league_id: 'H1', in_progress: false, wins: 2, losses: 1, ties: 0, pf: 177, pa: 158, pf_rank: 2, pa_rank: 2, eff: 89.4, left_total: 21, close_losses: 1, champion: 'teal' });
  assert.deepEqual([f.seasons[0].in_progress, f.seasons[0].champion, f.seasons[0].draft], [true, null, null]);
});

test('weekly hindsight solves the best legal lineup, counts regular-season weeks only, and flags a loss by under ten as close', async () => {
  const f = await factsOf();
  assert.deepEqual(f.seasons[1].weeks.map((w) => [w.week, w.result, w.started, w.best, w.left, w.opp, w.close_loss]), [[1, 'L', 64, 70, 6, 68, true], [2, 'W', 55, 59, 4, 50, false], [3, 'W', 58, 69, 11, 40, false]]);
  assert.equal(f.seasons[1].weeks.length, 3, 'the week 4 playoff matchup does not count');
  assert.deepEqual(f.seasons[0].weeks.map((w) => w.week), [1, 2], 'weeks Sleeper has no matchups for are skipped, not errors');
});

test('reach and steal use one rule: position-rank gap of five between drafted-as and finish', async () => {
  const f = await factsOf();
  assert.deepEqual(f.seasons[1].draft!.map((p) => [p.player.pos, p.drafted_as, p.finished, p.verdict]), [['RB', 1, 2, 'as_expected'], ['WR', 1, 7, 'reach'], ['WR', 7, 1, 'steal']]);
  assert.deepEqual([[1, 5], [1, 6], [6, 1], [7, 3], [0, 4], [3, 0]].map(([d, f]) => verdictOf(d, f)), ['as_expected', 'reach', 'steal', 'as_expected', 'unknown', 'unknown']);
  assert.match(f.definitions.reach_steal, /5 or more ranks below the draft rank is a reach/);
});

test('a season the owner did not play is skipped, a league with none fails, and the seasons cap limits the chain', async () => {
  const gone = { ...fixtures, '/v1/league/H1/rosters': fixtures['/v1/league/H1/rosters'].filter((r: any) => r.roster_id !== 1) };
  assert.deepEqual((await factsOf(INPUT, gone)).seasons.map((s) => s.season), ['2026']);
  await assert.rejects(factsOf({ ...INPUT, owner: 'nobody' }), /no team for nobody/);
  assert.deepEqual((await factsOf({ ...INPUT, seasons: 1 })).seasons.map((s) => s.season), ['2026']);
});

test('the card is the figures plus the model notes by season; a note for a season that was not reviewed fails the card step', async () => {
  const f = await factsOf();
  const c = buildHistoryCard(f, decision);
  assert.deepEqual([c.kind, c.seasons.map((s) => [s.season, s.note === null])], ['history', [['2026', true], ['2025', false]]]);
  assert.deepEqual(c.definitions, DEFINITIONS);
  assert.throws(() => buildHistoryCard(f, { ...decision, seasons: [{ season: '1999', text: 'x' }] }), /season 1999, which was not reviewed/);
  assert.throws(() => buildHistoryCard(f, { ...decision, seasons: [decision.seasons[0], decision.seasons[0]] }), /twice/);
});

test('figures_from_tools passes the card and fails a changed figure, a verdict off the one rule, or a changed definition', async () => {
  const f = await factsOf();
  const c = buildHistoryCard(f, decision);
  assert.equal((await gate(f, c)).met, 1);
  const edit = (change: (s: HistoryCard['seasons'][number]) => object) => ({ ...c, seasons: c.seasons.map((s) => (s.season === '2025' ? { ...s, ...change(s) } : s)) });
  const wins = await gate(f, edit(() => ({ wins: 3 })));
  assert.equal(wins.met, 0.5);
  assert.match(wins.detail!, /2025/);
  assert.ok((await gate(f, edit((s) => ({ eff: 95.0, left_total: s.left_total }))))!.met < 1);
  const verdict = await gate(f, edit((s) => ({ draft: s.draft!.map((p) => ({ ...p, verdict: 'steal' })) })));
  assert.ok(verdict.met < 1);
  assert.equal((await gate(f, { ...c, definitions: { ...c.definitions, reach_steal: 'PFF 20+ spots' } })).met, 0);
  assert.equal((await gate(f, { ...c, seasons: c.seasons.slice(0, 1) })).met, 0, 'a dropped season');
});

test('a row\'s judge evidence carries that season\'s figures; a season with no note has no row', async () => {
  const f = await factsOf();
  const ev = await historyEvidence(f)(null, { content: JSON.stringify(buildHistoryCard(f, decision)) });
  assert.deepEqual(ev.row.map((u) => u.id), ['season2025']);
  const row = ev.row[0].state as unknown as { season: { record: string; weeks: unknown[]; draft: Array<{ verdict: string }> } };
  assert.deepEqual([row.season.record, row.season.weeks.length, row.season.draft.map((p) => p.verdict)], ['2-1', 3, ['as_expected', 'reach', 'steal']]);
});

async function run(d: HistoryDecision) {
  const judged: string[] = [];
  const responses: FauxResponseStep[] = [
    () => fauxAssistantMessage(JSON.stringify(d)),
    ...Array.from({ length: 40 }, () => (c: { messages: Array<{ content: unknown }> }) => {
      const asked = String(c.messages.at(-1)!.content);
      judged.push(asked);
      return fauxAssistantMessage(JSON.stringify({ choice: /one week, one bust|equate lineup efficiency/.test(asked) ? 'no' : 'yes', confidence: 1, reason: 'ok' }));
    }),
  ];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-history-'));
  const workflow = historyWorkflow({ client: clientOf(), snapshot: async () => ({}), now: () => 0 });
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'You are the history analyst.', extensions: [workflowExtension([workflow])] } });
  try {
    const outcome = await runWorkflow(s.harness, s.root, workflow, JSON.stringify(INPUT));
    assert.equal(outcome.status, 'completed', JSON.stringify(outcome));
    const sealed = (await s.harness.snapshot(Artifacts, s.root.id, ctx))!.kinds['sleeper.cards'].versions[0];
    return { sealed, judged, verdict: sealed.verdict as { passed: boolean; rubric: string; gatesFailed: string[] } };
  } finally {
    await s.close();
    await rm(dir, { recursive: true, force: true });
  }
}

test('the workflow seals a history card whose figures are the tools and the judge passes it', async () => {
  const { sealed, verdict, judged } = await run(decision);
  const c = JSON.parse(sealed.content) as HistoryCard;
  assert.deepEqual([c.kind, c.seasons.map((s) => s.season), c.seasons[1].draft!.length], ['history', ['2026', '2025'], 3]);
  assert.deepEqual([verdict.rubric, verdict.gatesFailed, verdict.passed], ['sleeper.history@1', [], true]);
  assert.ok(judged.some((q) => /schedule luck/.test(q)));
});

test('the history job starts on demand under the week key with an optional seasons cap, and is never scheduled', async () => {
  const calls: Array<{ url: string; body: { input: string } }> = [];
  const fake = (async (u: string, init: { body: string }) => (calls.push({ url: u, body: JSON.parse(init.body) }), new Response('{}', { status: 202 }))) as unknown as typeof fetch;
  const service = createSleeperService({ serviceToken: 't', dbPath: ':memory:', client: clientOf(), distributorUrl: 'http://d', owner: 'mallard', fetch: fake, now: () => new Date(0) });
  await new Promise<void>((r) => service.listen(0, r));
  const post = (path: string, method = 'POST') => fetch(`http://127.0.0.1:${(service.address() as AddressInfo).port}${path}`, { method, headers: { authorization: 'Bearer t' } });
  try {
    const first = await post('/leagues/H2/jobs/history');
    assert.deepEqual([first.status, await first.json()], [202, { sessionKey: 'sleeper:H2:3:history', status: 202 }]);
    assert.equal(calls[0].url, 'http://d/sessions/sleeper%3AH2%3A3%3Ahistory/invocations?agent=sleeper-history');
    assert.deepEqual(JSON.parse(calls[0].body.input), INPUT);
    await post('/leagues/H2/jobs/history?seasons=2');
    assert.deepEqual(JSON.parse(calls[1].body.input), { ...INPUT, seasons: 2 });
    assert.equal((await post('/leagues/H2/jobs/history?seasons=0')).status, 400);
    assert.equal((await post('/leagues/H2/jobs/history', 'GET')).status, 405);
    assert.equal(calls.length, 2);
    assert.equal(historyKey('H2', 3), 'sleeper:H2:3:history');
  } finally {
    service.stop();
  }
});
