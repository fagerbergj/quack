import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, type FauxResponseStep } from '@earendil-works/pi-ai';
import { createSchedulesService } from '@quack/schedules';
import { Artifacts } from '@quack/sdk';
import { openSession } from '@quack/session';
import { runWorkflow, workflowExtension, type Workflow } from '@quack/workflows';
import {
  buildDigestCard, buildDigestFacts, buildRetroCard, buildRetroFacts, buildSnapshot, createSleeperClient, createSleeperService, digestEvidence, digestKey, digestRubric, digestWorkflow, registerReportSchedules,
  reportRuns, retroEvidence, retroKey, retroRubric, retroWorkflow, type DigestCard, type DigestFacts, type RetroCard, type RetroFacts,
} from '../src/index.ts';
import type { DigestDecision } from '../src/report/digest.ts';
import type { RetroDecision } from '../src/report/retro.ts';

const fixtures = JSON.parse(readFileSync(new URL('./fixtures/report.json', import.meta.url), 'utf8')) as Record<string, any>;
const AT = Date.parse('2026-11-10T09:00:00Z');
const RECAP = { league: 'L1', week: 9, owner: 'mallard' };
const PREVIEW = { league: 'L1', week: 10, owner: 'mallard' };

const clientOf = (fx: Record<string, any>, seen: string[] = []) => createSleeperClient({
  fetch: (async (input: string) => {
    const path = new URL(input).pathname;
    seen.push(path);
    return path in fx ? new Response(JSON.stringify(fx[path])) : new Response('null');
  }) as unknown as typeof fetch,
  now: () => AT,
});
const snapshotOf = (fx: Record<string, any>) => async (league: string) => ({ data: await buildSnapshot(clientOf(fx), league) });
type News = NonNullable<Parameters<typeof buildDigestFacts>[0]['news']>;
// Dated six hours before the cutoff asked for, so it always precedes the row it is attached to.
const newsStub: News = async (p, at) => [{ title: `${p.name} report`, url: `https://www.nfl.com/${p.name.replace(' ', '-')}`, date: new Date(at - 6 * 3_600_000).toISOString() }];

const digestFacts = async (input = RECAP, fx = fixtures, news?: News) => buildDigestFacts({ client: clientOf(fx), news }, await snapshotOf(fx)('L1'), input, AT);
const retroFacts = async (fx = fixtures, news?: News) => buildRetroFacts({ client: clientOf(fx), news }, await snapshotOf(fx)('L1'), RECAP);

const digestDecision: DigestDecision = {
  summary: 'Mallard Mayhem edged teal by 3 behind Quinn Quill\'s 20, while Pintail Pride made the week\'s biggest gap.',
  notes: [{ matchup: 1, note: 'Quill and Wade Wide outscored Tara Teal\'s lone 40 by a field goal.' }, { matchup: 2, note: 'Penny Pin\'s 60 buried Wiggy Wig.' }],
};
const previewDecision: DigestDecision = {
  summary: 'Early look: Pintail Pride is the form team; the first game turns on Wade Wide\'s Questionable tag.',
  notes: [{ matchup: 1, note: 'Wade Wide is Questionable, per [report](https://www.nfl.com/Wade-Wide).' }, { matchup: 2, note: 'Penny Pin averages 50 against a 30 side.' }],
};

const gate = async <F, C>(rubric: typeof digestRubric, evidence: (f: F) => (a: null, s: { content: string }) => Promise<any>, id: string, f: F, card: C) => {
  const check = rubric.criteria.flatMap((c) => c.checks).find((c) => c.id === id)!;
  const content = JSON.stringify(card);
  return check.code!(content, await evidence(f)(null, { content }));
};
const digestGate = (id: string, f: DigestFacts, card: unknown) => gate(digestRubric, digestEvidence, id, f, card);
const retroGate = (id: string, f: RetroFacts, card: unknown) => gate(retroRubric, retroEvidence, id, f, card);
const DIGEST_GATES = ['every_game_covered', 'numbers_match_tools', 'no_projection_source'];
const RETRO_GATES = ['tool_figures_copied', 'knowable_dated_before_kickoff'];

test('a recap holds every game with final points, margin, top scorers, the owner\'s game and the playoff line from the standings', async () => {
  const seen: string[] = [];
  const f = await buildDigestFacts({ client: clientOf(fixtures, seen) }, await snapshotOf(fixtures)('L1'), RECAP, AT);
  assert.equal(f.mode, 'recap');
  assert.deepEqual(f.games.map((g) => [g.id, g.sides.map((s) => [s.team, s.pts]), g.margin, g.mine]), [[1, [['Mallard Mayhem', 43], ['teal', 40]], 3, true], [2, [['Pintail Pride', 60], ['wigeon', 30]], 30, false]]);
  assert.deepEqual(f.games[0].sides[0].top, [{ id: 'p1', name: 'Quinn Quill', pts: 20 }, { id: 'p3', name: 'Wade Wide', pts: 12 }]);
  assert.deepEqual(f.standings.map((s) => [s.rank, s.team, s.in_playoff_line]), [[1, 'Mallard Mayhem', true], [2, 'teal', true], [3, 'Pintail Pride', false], [4, 'wigeon', false]]);
  assert.deepEqual([f.playoffTeams, f.remainingWeeks, f.sourceNote], [2, 5, 'Final scores from Sleeper, week 9.']);
  assert.ok(!seen.some((p) => p.includes('projections')), 'a digest never asks Sleeper for projections');
});

test('a preview sums each starter\'s recent average in code, counting a bye or a player ruled out as 0', async () => {
  const f = await digestFacts(PREVIEW);
  assert.equal(f.mode, 'preview');
  const [mallard, teal] = f.games[0].sides;
  // Quill averages 18 and Wide 12; Rowe is Out and Dash is on his bye.
  assert.deepEqual([mallard.pts, mallard.out, teal.pts, f.games[0].margin], [30, ['Rob Rowe (Out)', 'Dan Dash (bye)'], 26.67, 3.33]);
  assert.deepEqual(mallard.top.map((t) => [t.name, t.pts]), [['Quinn Quill', 18], ['Wade Wide', 12]]);
  assert.deepEqual(mallard.designated, [{ id: 'p3', name: 'Wade Wide', inj: 'Questionable' }]);
  assert.match(f.sourceNote, /average over the last 3 finished weeks; counted 0: Rob Rowe \(Out\), Dan Dash \(bye\)/);
});

test('a preview with no finished week shows 0 and says so, and a week in progress is refused', async () => {
  const early = { ...fixtures, '/v1/state/nfl': { ...fixtures['/v1/state/nfl'], week: 1 }, '/v1/league/L1/matchups/1': fixtures['/v1/league/L1/matchups/10'], '/schedule/nfl/regular/2026': fixtures['/schedule/nfl/regular/2026'].map((g: any) => ({ ...g, week: g.week === 10 ? 1 : g.week })) };
  const f = await digestFacts({ ...PREVIEW, week: 1 }, early);
  assert.deepEqual([f.games[0].sides.map((s) => s.pts), f.sourceNote], [[0, 0], 'No finished weeks yet, so every side shows 0; the news carries the preview.']);
  const live = { ...fixtures, '/schedule/nfl/regular/2026': fixtures['/schedule/nfl/regular/2026'].map((g: any) => (g.game_id === 'g100' ? { ...g, status: 'in_game' } : g)) };
  await assert.rejects(digestFacts(PREVIEW, live), /week 10 is in progress/);
});

test('news is searched only for a preview\'s top scorers and designated starters, and attached to their game', async () => {
  const searched: string[] = [];
  const f = await digestFacts(PREVIEW, fixtures, async (p, at) => (searched.push(p.name), newsStub(p, at)));
  assert.deepEqual([...new Set(searched)].sort(), ['Penny Pin', 'Quinn Quill', 'Tara Teal', 'Wade Wide', 'Wiggy Wig']);
  const card = buildDigestCard(f, previewDecision);
  assert.deepEqual(card.games[0].news.map((n) => n.player), ['Quinn Quill', 'Wade Wide', 'Wade Wide', 'Tara Teal']);
  const recap = await digestFacts(RECAP, fixtures, newsStub);
  assert.deepEqual(recap.news, {}, 'a recap needs no pre-game news');
});

test('the digest card carries the facts\' figures and each code gate passes it, in a recap and a preview', async () => {
  for (const [input, decision] of [[RECAP, digestDecision], [PREVIEW, previewDecision]] as const) {
    const f = await digestFacts(input, fixtures, newsStub);
    const card = buildDigestCard(f, decision);
    assert.equal(card.past, f.mode === 'recap');
    assert.deepEqual(card.games.map((g) => [g.home.team, g.home.pts, g.away.pts]), f.games.map((g) => [g.sides[0].team, g.sides[0].pts, g.sides[1].pts]));
    for (const id of DIGEST_GATES) assert.equal((await digestGate(id, f, card)).met, 1, id);
  }
});

test('a digest note for a game that is not in the week fails the card step', async () => {
  assert.throws(() => buildDigestCard({ ...({} as DigestFacts), games: [], week: 9 }, { summary: 'x', notes: [{ matchup: 7, note: 'x' }] }), /matchups that are not in week 9: 7/);
});

test('every_game_covered, numbers_match_tools and no_projection_source each fail a card that breaks them', async () => {
  const f = await digestFacts();
  const card = buildDigestCard(f, digestDecision);
  assert.match((await digestGate('every_game_covered', f, { ...card, games: card.games.slice(0, 1) })).detail!, /Pintail Pride v wigeon/);
  const wrongScore = { ...card, games: card.games.map((g, i) => (i ? g : { ...g, home: { ...g.home, pts: 44 } })) };
  assert.match((await digestGate('numbers_match_tools', f, wrongScore)).detail!, /Mallard Mayhem v teal/);
  const wrongTop = { ...card, games: card.games.map((g, i) => (i ? g : { ...g, away: { ...g.away, top_scorers: [{ name: 'Tara Teal', pts: 39 }] } })) };
  assert.ok((await digestGate('numbers_match_tools', f, wrongTop)).met < 1);
  for (const text of ['Mallard is projected to win.', 'Teal has a 70% chance to win.', 'Their win probability is high.', 'The odds favor Pintail.']) {
    const r = await digestGate('no_projection_source', f, { ...card, summary: text });
    assert.equal(r.met, 0, text);
  }
  assert.equal((await digestGate('no_projection_source', f, { ...card, games: card.games.map((g) => ({ ...g, note: 'Projected 130 points.' })) })).met, 0);
});

test('the retro holds the best lineup, the legal swap, the free-agent hit and the cutoffs, all from the final week', async () => {
  const f = await retroFacts();
  assert.deepEqual([f.team, f.opponent, f.started, f.opp, f.won, f.best, f.left], ['Mallard Mayhem', 'teal', 43, 40, true, 55, 12]);
  assert.deepEqual(f.misses.map((m) => [m.slot, m.started.name, m.better.name, m.better.pts, m.swing, m.before]), [['FLEX', 'Ned Nope', 'Dan Dash', 15, 12, '2026-11-08T18:00:00.000Z']]);
  // The Wednesday 00:00 Pacific waiver run before the Thursday game.
  assert.deepEqual(f.waiverMisses.map((w) => [w.player.name, w.player.pts, w.over.slot, w.over.name, w.before]), [['Felix Free', 16, 'FLEX', 'Ned Nope', '2026-11-04T08:00:00.000Z']]);
});

test('a retro for a week that is not final is refused', async () => {
  await assert.rejects(buildRetroFacts({ client: clientOf(fixtures) }, await snapshotOf(fixtures)('L1'), PREVIEW), /week 10 is not final/);
});

test('a row is knowable only when the model cites a link the code attached that precedes the cutoff', async () => {
  const f = await retroFacts(fixtures, newsStub);
  const dash = f.misses[0].news.find((n) => n.player === 'Dan Dash')!;
  const free = f.waiverMisses[0].news[0];
  const decision: RetroDecision = {
    misses: [{ index: 0, why: 'Dan Dash had the role per the report.', knowable: true, signalUrl: dash.url }],
    waiverMisses: [{ index: 0, why: 'A hot add.', knowable: true, signalUrl: 'https://www.nfl.com/invented' }],
    lessons: ['Check the Saturday report.', 'b', 'c', 'dropped'], summary: 'Dan Dash over Ned Nope was the week\'s swing.',
  };
  const card = buildRetroCard(f, decision);
  assert.deepEqual([card.misses[0].knowable, card.misses[0].signal?.url, card.waiver_misses[0].knowable, card.waiver_misses[0].signal], [true, dash.url, false, null]);
  assert.equal(card.lessons.length, 3);
  assert.equal(buildRetroCard(f, { ...decision, waiverMisses: [{ index: 0, why: 'x', knowable: true, signalUrl: free.url }] }).waiver_misses[0].knowable, true);
  assert.equal(buildRetroCard(f, { ...decision, misses: [], waiverMisses: [] }).misses[0].why, 'No pre-kickoff signal was found.');
  assert.throws(() => buildRetroCard(f, { ...decision, misses: [{ index: 4, why: 'x', knowable: false }] }), /miss 4/);
  for (const id of RETRO_GATES) assert.equal((await retroGate(id, f, card)).met, 1, id);
});

test('tool_figures_copied and knowable_dated_before_kickoff each fail a card that breaks them', async () => {
  const f = await retroFacts(fixtures, newsStub);
  const url = f.misses[0].news[0].url;
  const card = buildRetroCard(f, { misses: [{ index: 0, why: 'x', knowable: true, signalUrl: url }], waiverMisses: [], lessons: [], summary: 's' });
  for (const [field, wrong] of [['left', 13], ['best', 54], ['won', false], ['opp', 41]] as const) assert.match((await retroGate('tool_figures_copied', f, { ...card, [field]: wrong })).detail!, new RegExp(field === 'won' ? 'won' : field));
  const swung: RetroCard = { ...card, misses: [{ ...card.misses[0], swing: 99 }] };
  assert.match((await retroGate('tool_figures_copied', f, swung)).detail!, /misses/);
  assert.match((await retroGate('tool_figures_copied', f, { ...card, waiver_misses: [] })).detail!, /waiver_misses/);
  const late = { ...card, misses: [{ ...card.misses[0], signal: { ...card.misses[0].signal!, date: '2026-11-08T19:00:00.000Z' } }] };
  assert.match((await retroGate('knowable_dated_before_kickoff', f, late)).detail!, /1 knowable row/);
  assert.match((await retroGate('knowable_dated_before_kickoff', f, { ...card, misses: [{ ...card.misses[0], signal: null }] })).detail!, /1 knowable row/);
});

test('a row\'s judge evidence carries its news, and only knowable rows are asked for a signal', async () => {
  const f = await retroFacts(fixtures, newsStub);
  const card = buildRetroCard(f, { misses: [{ index: 0, why: 'x', knowable: true, signalUrl: f.misses[0].news[0].url }], waiverMisses: [{ index: 0, why: 'y', knowable: false }], lessons: [], summary: 's' });
  const ev = await retroEvidence(f)(null, { content: JSON.stringify(card) });
  assert.deepEqual([ev.row.length, ev.known.map((u) => u.id)], [2, ['row1']]);
  assert.equal((ev.row[1].state as unknown as { news: unknown[] }).news.length, 1);
});

async function run(make: (deps: Parameters<typeof digestWorkflow>[0]) => Workflow, input: object, decision: object, news?: News) {
  const judged: string[] = [];
  const responses: FauxResponseStep[] = [
    () => fauxAssistantMessage(JSON.stringify(decision)),
    ...Array.from({ length: 40 }, () => (c: { messages: Array<{ content: unknown }> }) => {
      const asked = String(c.messages.at(-1)!.content);
      judged.push(asked);
      return fauxAssistantMessage(JSON.stringify({ choice: /lucky without the point margin|comeback, collapse|blamed on the manager/.test(asked) ? 'no' : 'yes', confidence: 1, reason: 'ok' }));
    }),
  ];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-report-'));
  const workflow = make({ client: clientOf(fixtures), snapshot: snapshotOf(fixtures), now: () => AT, news });
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'You are the league reporter.', extensions: [workflowExtension([workflow])] } });
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

test('the digest workflow seals a recap and a preview card and the judge passes them, saying what was not checked', async () => {
  const recap = await run(digestWorkflow, RECAP, digestDecision);
  assert.deepEqual([(JSON.parse(recap.sealed.content) as DigestCard).kind, recap.verdict.rubric, recap.verdict.gatesFailed, recap.verdict.passed], ['digest', 'sleeper.digest@1', [], true]);
  assert.ok(recap.judged.some((q) => /real story/.test(q)));
  assert.ok(!recap.verdict.checks.some((k) => k.id === 'preview_news_decides'));
  assert.deepEqual(recap.verdict.notes, ['preview_news_decides not checked: no news source']);
  const preview = await run(digestWorkflow, PREVIEW, previewDecision, newsStub);
  assert.deepEqual([(JSON.parse(preview.sealed.content) as DigestCard).past, preview.verdict.gatesFailed, preview.verdict.passed, preview.verdict.notes], [false, [], true, []]);
  assert.ok(preview.judged.some((q) => /swings it/.test(q)));
});

test('a digest that states a projection fails its gate in the workflow', async () => {
  const { verdict } = await run(digestWorkflow, RECAP, { ...digestDecision, summary: 'Teal had a 70% chance to win.' });
  assert.deepEqual([verdict.passed, verdict.gatesFailed], [false, ['faithful_to_tools.no_projection_source']]);
});

test('the retro workflow seals a card whose figures are the facts\' and the judge passes it', async () => {
  const f = await retroFacts(fixtures, newsStub);
  const decision: RetroDecision = { misses: [{ index: 0, why: 'Dash had the flex role.', knowable: true, signalUrl: f.misses[0].news[0].url }], waiverMisses: [{ index: 0, why: 'No signal before the run.', knowable: false }], lessons: ['Check Saturday\'s report.'], summary: 'Dan Dash over Ned Nope cost 12.' };
  const { sealed, verdict, judged } = await run(retroWorkflow, RECAP, decision, newsStub);
  const card = JSON.parse(sealed.content) as RetroCard;
  assert.deepEqual([card.kind, card.left, card.misses[0].knowable, verdict.rubric, verdict.gatesFailed, verdict.passed, verdict.notes], ['retro', 12, true, 'sleeper.retro@1', [], true, []]);
  assert.ok(judged.some((q) => /not the same injury designation restated/.test(q)));
  const bare = await run(retroWorkflow, RECAP, { ...decision, misses: [{ index: 0, why: 'x', knowable: false }] });
  assert.deepEqual(bare.verdict.notes, ['signal_is_evidence not checked: no news source']);
});

test('the report runs are a preview an hour before the first kickoff and a recap and retro once the last game is final', () => {
  assert.deepEqual(reportRuns([], 'L1', 9), []);
  const first = Date.parse('2026-11-06T01:15:00Z');
  const last = Date.parse('2026-11-10T01:15:00Z');
  const runs = reportRuns([last, first, Date.parse('2026-11-08T18:00:00Z')], 'L1', 9);
  assert.deepEqual(runs.map((r) => [r.id, r.agent, r.sessionKey, new Date(r.at).toISOString(), new Date(r.deadline).toISOString()]), [
    ['sleeper/digest/L1/9/preview', 'sleeper-digest', 'sleeper:L1:9:digest', '2026-11-05T19:15:00.000Z', '2026-11-06T00:15:00.000Z'],
    ['sleeper/digest/L1/9/recap', 'sleeper-digest', 'sleeper:L1:9:digest', '2026-11-10T06:15:00.000Z', '2026-11-13T06:15:00.000Z'],
    ['sleeper/retro/L1/9', 'sleeper-retro', 'sleeper:L1:9:retro', '2026-11-10T06:15:00.000Z', '2026-11-13T06:15:00.000Z'],
  ]);
  assert.deepEqual([digestKey('L1', 9), retroKey('L1', 9)], ['sleeper:L1:9:digest', 'sleeper:L1:9:retro']);
});

test('the schedules service lists the three runs with their session keys and deadlines, once however often they register', async () => {
  const schedules = createSchedulesService({ serviceToken: 's', distributorUrl: 'http://unused', dbPath: ':memory:' });
  await new Promise<void>((r) => schedules.listen(0, r));
  const url = `http://127.0.0.1:${(schedules.address() as AddressInfo).port}`;
  const week9 = { ...fixtures, '/v1/state/nfl': { ...fixtures['/v1/state/nfl'], week: 9 } };
  try {
    await registerReportSchedules({ url, token: 's' }, clientOf(week9), 'L1', 'mallard');
    const ids = await registerReportSchedules({ url, token: 's' }, clientOf(week9), 'L1', 'mallard');
    assert.deepEqual(ids, ['sleeper/digest/L1/9/preview', 'sleeper/digest/L1/9/recap', 'sleeper/retro/L1/9']);
    const { schedules: listed } = await (await fetch(`${url}/schedules`, { headers: { authorization: 'Bearer s' } })).json();
    assert.equal(listed.length, 3);
    assert.partialDeepStrictEqual(listed.find((s: { id: string }) => s.id === 'sleeper/retro/L1/9'), { agent: 'sleeper-retro', times: ['2026-11-10T06:15:00.000Z'], deadline: '2026-11-13T06:15:00.000Z', sessionKey: 'sleeper:L1:9:retro', input: JSON.stringify(RECAP) });
    const off = { ...week9, '/v1/state/nfl': { ...week9['/v1/state/nfl'], season_type: 'post' } };
    assert.deepEqual(await registerReportSchedules({ url, token: 's' }, clientOf(off), 'L1', 'mallard'), []);
  } finally {
    schedules.stop();
  }
});

test('the digest and retro start on demand under their own week session keys', async () => {
  const calls: Array<{ url: string; body: { input: string } }> = [];
  const fake = (async (u: string, init: { body: string }) => (calls.push({ url: u, body: JSON.parse(init.body) }), new Response('{}', { status: 202 }))) as unknown as typeof fetch;
  const service = createSleeperService({ serviceToken: 't', dbPath: ':memory:', client: clientOf(fixtures), distributorUrl: 'http://d', owner: 'mallard', fetch: fake, now: () => new Date(AT) });
  await new Promise<void>((r) => service.listen(0, r));
  const post = (path: string) => fetch(`http://127.0.0.1:${(service.address() as AddressInfo).port}${path}`, { method: 'POST', headers: { authorization: 'Bearer t' } });
  try {
    assert.deepEqual(await (await post('/leagues/L1/jobs/digest?week=9')).json(), { sessionKey: 'sleeper:L1:9:digest', status: 202 });
    assert.deepEqual(await (await post('/leagues/L1/jobs/retro')).json(), { sessionKey: 'sleeper:L1:10:retro', status: 202 });
    assert.deepEqual(calls.map((c) => c.url), ['http://d/sessions/sleeper%3AL1%3A9%3Adigest/invocations?agent=sleeper-digest', 'http://d/sessions/sleeper%3AL1%3A10%3Aretro/invocations?agent=sleeper-retro']);
    assert.deepEqual(JSON.parse(calls[0].body.input), RECAP);
  } finally {
    service.stop();
  }
});
