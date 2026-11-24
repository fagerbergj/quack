import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { buildFacts, buildTradeFacts, buildWaiverFacts, createSleeperClient, liveLeague } from '../src/index.ts';
import { contextLines, dvpRanks, faDepth, fetchRecent, type Recent } from '../src/context.ts';
import { replacementWorth } from '../src/waivers/facts.ts';
import type { TradeFacts } from '../src/trades/facts.ts';
import { factsPrompt as lineupPrompt } from '../src/lineup/workflow.ts';
import { factsPrompt as waiversPrompt } from '../src/waivers/workflow.ts';
import { finderPrompt } from '../src/trades/workflow.ts';

// Week 12 of a real 10-team PPR league, trimmed to the rostered players, the top free agents and their teams' depth.
const week12 = JSON.parse(readFileSync(new URL('./fixtures/week12.json', import.meta.url), 'utf8')) as Record<string, unknown>;
const AT = Date.parse('2026-11-27T15:00:00Z');
const INPUT = { league: 'L1', week: 12, owner: 'mallard' };
const MAYER = '9482';
const HENRY = '3214';

const fetchOf = (fx: Record<string, unknown>, fail = new Set<string>()) => (async (input: string) => {
  const path = new URL(input).pathname;
  if (fail.has(path)) return new Response('boom', { status: 500 });
  return path in fx ? new Response(JSON.stringify(fx[path])) : new Response('null');
}) as unknown as typeof fetch;
const client = (fx = week12, fail?: Set<string>) => createSleeperClient({ fetch: fetchOf(fx, fail), now: () => AT, sleep: async () => {} });

async function jobFacts(fx = week12, fail?: Set<string>) {
  const c = client(fx, fail);
  const snap = await liveLeague(c, 'L1');
  return { lineup: await buildFacts({ client: c }, snap, INPUT, AT), waivers: await buildWaiverFacts({ client: c }, snap, INPUT, AT), trades: await buildTradeFacts({ client: c }, snap, INPUT) };
}

// About 4 characters a token; digit-heavy lines run nearer 3, so the real cost is up to a third higher.
const tokens = (s: string) => Math.ceil(s.length / 4);

const row = (o: Record<string, unknown>) => ({ gp: 1, ...o });
// Two weeks, three defenses: BBB allows the most to TEs, AAA the fewest; CCC was on bye in week 2.
const small: Recent = {
  weeks: [
    { week: 2, stats: { AAA: row({ fan_pts_allow_te: 4, pts_allow: 10 }), BBB: row({ fan_pts_allow_te: 20, pts_allow: 30 }), t1: row({ pts_ppr: 12.34, off_snp: 40, tm_off_snp: 50, rec_tgt: 7 }) } },
    { week: 1, stats: { AAA: row({ fan_pts_allow_te: 6, pts_allow: 14 }), BBB: row({ fan_pts_allow_te: 16, pts_allow: 20 }), CCC: row({ fan_pts_allow_te: 9, pts_allow: 17 }), q1: row({ pts_ppr: 20 }) } },
  ],
  scores: [{ metadata: { home_team: 'AAA', away_team: 'BBB', home_record: '5-5-0', away_record: '6-4-1', spread: { AAA: -3.5, BBB: 3.5 } } }],
};
const dump = {
  t1: { first_name: 'Tee', last_name: 'One', team: 'BBB', position: 'TE', depth_chart_order: 2 },
  t2: { first_name: 'Tee', last_name: 'Two', team: 'BBB', position: 'TE', depth_chart_order: 1, injury_status: 'Out' },
  q1: { first_name: 'Cue', last_name: 'Starter', team: 'BBB', position: 'QB', depth_chart_order: 2, injury_status: 'Out' },
  q2: { first_name: 'Cue', last_name: 'Backup', team: 'BBB', position: 'QB', depth_chart_order: 1 },
};
const schedule = [{ week: 1, home: 'AAA', away: 'BBB' }, { week: 2, home: 'BBB', away: 'AAA' }, { week: 3, home: 'AAA', away: 'BBB' }];

test('a defense ranks 1 for the fewest points allowed per game to the position, over the weeks it played', () => {
  assert.deepEqual([...dvpRanks(small.weeks, 'TE')], [['AAA', 1], ['CCC', 2], ['BBB', 3]]);
  assert.deepEqual([...dvpRanks(small.weeks, 'RB')], [], 'no defense row carries RB allowed, so none ranks');
});

test('recent form reads his team\'s games newest first, with usage, team scoring, record, spread and who is out', () => {
  const c = contextLines(['t1', 'q2'], { dump, schedule, key: 'pts_ppr', week: 3, recent: small });
  assert.equal(c.t1, 'L2 12/-, snap 80%, 7 tgt/g, vs AAA #1, 6-4-1 12ppg +3.5, QB Backup (Starter Out), Two Out');
  assert.equal(c.q2, 'no recent data, vs AAA, 6-4-1 12ppg +3.5, Starter Out', 'a QB names the QB who is out, not a QB line');
  const brief = contextLines(['t1'], { dump, schedule, key: 'pts_ppr', week: 3, recent: small, brief: true });
  assert.equal(brief.t1, 'L2 12/-, snap 80%, 7 tgt/g, QB Backup (Starter Out), Two Out', 'a trade row keeps form, usage and who is out');
});

test('an RB\'s catches count as touches in a game with no carries', () => {
  // Week 7 of Emmett Johnson (13337): Sleeper omits rush_att when he had no carries.
  const carryless = (week12['/v1/stats/nfl/regular/2026/7'] as Record<string, Record<string, number>>)['13337'];
  assert.equal(carryless.rush_att, undefined);
  const recent: Recent = { weeks: [{ week: 2, stats: { r1: carryless } }, { week: 1, stats: { r1: row({ rush_att: 10, rec: 1, off_snp: 30, tm_off_snp: 60 }) } }], scores: [] };
  const c = contextLines(['r1'], { dump: { r1: { last_name: 'Back', team: 'BBB', position: 'RB' } }, schedule, key: 'pts_ppr', week: 3, recent, brief: true });
  assert.equal(c.r1, 'L2 4/0, snap 39%, 6.5 tch/g', '(2 + 11) touches over 2 games');
});

test('a missing week, a failed endpoint or no recent stats degrades to "no recent data", never a failed job', async () => {
  const fail = new Set(['/v1/stats/nfl/regular/2026/11', '/scores/nfl/regular/2026/12']);
  const recent = await fetchRecent(client(week12, fail), '2026', 12);
  assert.deepEqual(recent.weeks.map((w) => w.week), [10, 9, 8, 7, 6]);
  assert.deepEqual(recent.scores, []);
  assert.deepEqual((await fetchRecent(client({}), '2026', 1)).weeks, [], 'week 1 has no prior weeks');
  const bare = Object.fromEntries(Object.entries(week12).filter(([k]) => !/^\/v1\/stats\/nfl\/regular\/2026\/\d+$/.test(k)));
  const { lineup, waivers, trades } = await jobFacts(bare);
  assert.equal(lineup.context[MAYER], 'no recent data, vs CLE, 4-7 +4.5, Bowers Out');
  assert.ok(Object.keys(waivers.context).length && Object.keys(trades.context).length);
});

test('free agents per position are counted against the bar, and waivers sets it at replacement worth', async () => {
  const free = [{ pos: 'K', v: 8 }, { pos: 'K', v: 7 }, { pos: 'K', v: 6 }, { pos: 'TE', v: null }];
  assert.deepEqual(faDepth(free, { K: 7, TE: 5 }, (p) => p.v), ['K: 2 FA ≥ 7', 'TE: 0 FA ≥ 5']);
  const { waivers } = await jobFacts();
  const k = waivers.depth.find((d) => d.startsWith('K: '));
  assert.match(k!, new RegExp(`≥ ${Math.round(replacementWorth(waivers, 'K')! * 10) / 10}$`));
});

test('week 12: Mayer reads Bowers out, Henry reads QB DeVito with Maye out', async () => {
  const { lineup, waivers } = await jobFacts();
  // Mayer's last four: 1.4, 3.8, 4.7, 10.3 PPR; LV scored 24, 13, 17 and 38.
  assert.equal(lineup.context[MAYER], 'L4 1/4/5/10, snap 76%, 4 tgt/g, vs CLE #20, 4-7 23ppg +4.5, Bowers Out');
  // NE's week-11 bye leaves weeks 10 to 7; Henry is a free agent here.
  assert.equal(waivers.context[HENRY], 'L4 4/7/6/14, snap 48%, 4 tgt/g, vs LAC #16, 5-5 21ppg +1.5, QB DeVito (Maye Out)');
});

/** Week 12 with eight more free agents trending, so waivers lists the [MARKED] rows a live run adds. */
async function marked() {
  const { waivers } = await jobFacts();
  const listed = new Set(Object.values(waivers.buckets).flat());
  const extra = Object.values(waivers.free).filter((p) => !listed.has(p.id) && p.pos !== 'DEF').slice(0, 8).map((p) => ({ player_id: p.id, count: 100 }));
  assert.equal(extra.length, 8);
  return { ...week12, '/v1/players/nfl/trending/add': extra };
}

// The rows as they were before context: no context map, no depth line and, on trade rows, no season rate.
const bareTrades = (f: TradeFacts): TradeFacts => {
  const side = <S extends TradeFacts['mine']>(s: S): S => ({ ...s, players: Object.fromEntries(Object.entries(s.players).map(([id, { ppg: _, ...p }]) => [id, p])) });
  return { ...f, context: {}, mine: side(f.mine), partners: f.partners.map(side) };
};

test('each job\'s task text carries the context lines within its size budget', async () => {
  const { lineup, waivers, trades } = await jobFacts(await marked());
  assert.equal(waivers.widely.length, 8);
  const sizes = {
    lineup: tokens(lineupPrompt(lineup)) - tokens(lineupPrompt({ ...lineup, context: {} })),
    waivers: tokens(waiversPrompt(waivers)) - tokens(waiversPrompt({ ...waivers, context: {}, depth: [] })),
    trades: tokens(finderPrompt(trades, {})) - tokens(finderPrompt(bareTrades(trades), {})),
  };
  assert.ok(lineupPrompt(lineup).includes(`QB: Joe Burrow 24.46 | ${lineup.context['6770']}`), 'the baseline carries each starter\'s context');
  assert.ok(waiversPrompt(waivers).includes(waivers.context[MAYER]) && waiversPrompt(waivers).includes(waivers.depth[0]));
  for (const id of waivers.widely) assert.ok(waiversPrompt(waivers).includes(`[MARKED] | ${waivers.context[id]}`), 'a marked free agent carries its line');
  assert.ok(finderPrompt(trades, {}).includes(trades.context[MAYER]));
  // Measured 211, 907 and 603 on this week. Trades counts the new season column too, so its "about 600" gets a 25-token margin.
  assert.ok(sizes.lineup <= 250 && sizes.waivers <= 1000 && sizes.trades <= 625, JSON.stringify(sizes));
});

test('a close call the baseline leaves out still gets its context line', async () => {
  const { lineup } = await jobFacts();
  // Tyson (13281) 15 sits inside CLOSE_GAP of Nabers' 16.2, but the baseline starts Nabers.
  const f = { ...lineup, players: { ...lineup.players, 13281: { ...lineup.players['13281'], proj: 15 } } };
  assert.ok(!f.baseline.some((b) => b.id === '13281'));
  assert.ok(lineupPrompt(f).includes(`Context for the other close calls:\nJordyn Tyson 15 | ${f.context['13281']}`));
});
