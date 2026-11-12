import { registerSplitter, type CodeResult, type Declaration, type Evidence, type Inputs, type Rubric } from '@quack/judge';
import { defect, metaFields, parseCard, share, SHARE, yes } from '../rubric-kit.ts';
import type { Candidate, WaiversCard } from './card.ts';
import { worth, type WaiverFacts } from './facts.ts';

const parse = (artifact: string) => parseCard<WaiversCard>(artifact);
const factsOf = (ev: Evidence) => ev.facts[0].state.facts as unknown as WaiverFacts;

// A dearer drop than the add is fine when no bench player is cheaper (an empty slot scores zero); a player with no figure is not compared.
function dropNotWorthMore(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const rows = parse(artifact).candidates;
  const bench = Object.values(f.roster).filter((p) => p.role === 'bench').flatMap((p) => { const w = worth(p); return w === null ? [] : [{ p, w }]; });
  const flaws = rows.flatMap((r) => {
    const d = worth(f.roster[r.drop.player.id]);
    const a = worth(f.free[r.player.id]);
    if (d === null || a === null || d <= a) return [];
    const cheap = bench.filter((b) => b.p.id !== r.drop.player.id && b.w < d).sort((x, y) => x.w - y.w)[0];
    return cheap ? [`${r.drop.player.name} (${d}) is worth more than ${r.player.name} (${a}); drop ${cheap.p.name} (${cheap.w}) first`] : [];
  });
  return share(rows.length - flaws.length, rows.length, `the drop costs more than the add: ${flaws.join('; ')}`);
}

function widelyOwnedAccounted(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const c = parse(artifact);
  const seen = new Set([...c.candidates.map((r) => r.player.id), ...c.also_checked.map((r) => r.player.id)]);
  const missing = f.widely.filter((id) => !seen.has(id));
  return share(f.widely.length - missing.length, f.widely.length, `no claim or reason for ${missing.map((id) => f.free[id].name).join(', ')}`);
}

// A pitfall over the bye holes: a hole needs as many claims at its position, on another bye, as it is short. Each add's own bye is on the card.
function byeCoverage(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const rows = parse(artifact).candidates;
  const holes = f.table.find((t) => t.mine)?.byes ?? [];
  const open = holes.flatMap((h) => {
    // Facts recorded before rubric 6 carry no `short`; a hole is at least one empty slot.
    const short = h.short ?? 1;
    const covers = rows.filter((r) => r.player.pos === h.pos && r.bye !== h.week).length;
    return covers < short ? [`week ${h.week} leaves ${short} ${h.pos} slot${short > 1 ? 's' : ''} empty and the claims cover ${covers}`] : [];
  });
  return { met: holes.length ? open.length / holes.length : 0, detail: open.length ? open.join('; ') : undefined };
}

/** Sleeper's designation by player name, so a check never pins one player's injury on another. */
const designations = (r: Candidate) => Object.fromEntries([r.player, r.drop.player].map((p) => [p.name, p.inj ?? 'none']));

/** Each claim as one record: what the decision-model checks read, and the legacy unit close_call_news keeps. */
function claims(f: WaiverFacts, card: WaiversCard) {
  return card.candidates.map((r) => ({
    id: `claim${r.rank}`, rank: r.rank, why: r.why,
    add: { name: r.player.name, pos: r.player.pos, team: r.player.team, inj: r.player.inj, proj: r.proj, owned_pct: r.owned_pct, adds_24h: r.adds_24h, bye: r.bye },
    close: { row: r, week: f.week, waiver_type: f.waiverType, my_priority: f.myPriority, faab_left: f.faabLeft, teams: f.teams, news: r.news ?? [], designations: designations(r) },
  }));
}

/** The judge's named texts: the facts the card was built on, its claims, and the card as the competition check reads it. */
export function waiversInputsOf(f: WaiverFacts, content: string): Inputs {
  const card = parse(content);
  const rows = card.candidates.map((r) => ({ rank: r.rank, add: r.player.name, drop: r.drop.player.name, why: r.why }));
  return { facts: JSON.stringify(f), claims: JSON.stringify(claims(f, card)), card: JSON.stringify({ summary: card.summary, waiver_type: f.waiverType, rows, table: f.table }) };
}
export const waiversInputs = (f: WaiverFacts) => async (_api: unknown, sealed: { content: string }): Promise<Inputs> => waiversInputsOf(f, sealed.content);

// Checks that stay on the 27B read the legacy unit: the card's row, then the other fields at the top. News is fetched only for close calls.
type Claim = ReturnType<typeof claims>[number];
registerSplitter('sleeper.waiverRows', (inputs, o) => (JSON.parse(inputs[String(o.from)]) as Claim[]).map((c) => ({ id: c.id, text: JSON.stringify(c.close.row), meta: c.close })));
const LEGACY_FIELDS = metaFields(['week', 'waiver_type', 'my_priority', 'faab_left', 'teams', 'news', 'designations']);

/** How each model check of waiversRubric is asked: per claim or of the card, each item alone with nothing shared. */
export const waiversDeclaration: Declaration = {
  scopes: {
    facts: { split: { name: 'whole', from: 'facts' }, piece: 'facts' },
    claims: { split: { name: 'jsonItems', from: 'claims', pick: ['rank', 'add', 'why'] }, piece: 'claim' },
    rows: { split: { name: 'sleeper.waiverRows', from: 'claims' }, piece: 'row', context: LEGACY_FIELDS },
    close: { split: { name: 'sleeper.waiverRows', from: 'claims', where: [{ path: 'news', op: 'truthy' }] }, piece: 'row', context: LEGACY_FIELDS },
    card: { split: { name: 'whole', from: 'card' }, piece: 'card' },
  },
  checks: {
    // Clef left every item of this unsure on replay, so the 27B decides it on the evidence it had before the port.
    ranked_on_usage: { scope: 'rows', aggregate: SHARE, decisionModel: false, question: yes('Does the rank rest on the player\'s role and usage, or for a team defense or kicker, its matchup?') },
    competition_read: { scope: 'card', aggregate: SHARE, question: yes('Does the urgency or size of a claim name a competing team\'s need, priority or FAAB left from the table?') },
    // Weighing `why` against the reports is a claim against evidence, which the decision model passes unread, so the 27B decides it.
    close_call_news: { scope: 'close', aggregate: SHARE, decisionModel: false, question: yes('Does this close top claim or drop reflect the attached `news`, naming the right player for each report and the Sleeper `designations`?') },
    crowd_metric_as_value: { scope: 'claims', aggregate: SHARE, question: defect('Is a trending or ownership number given as evidence the player will score, rather than as how hard he is to land?') },
  },
};

// The card builder copies the league context, each drop's roster role, each add's bye and the claim size from the facts, and refuses a bid where the league has none, so none is checked here.
// Waivers weights are unreplayed: replay them before any weight gates delivery.
export const waiversRubric: Rubric = {
  name: 'sleeper.waivers',
  version: 8,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [
      { id: 'drop_not_worth_more', label: "Drops a player worth more than the add", weight: 'gate', code: dropNotWorthMore },
      { id: 'widely_owned_accounted', label: "Ignores that the add is widely owned", weight: 'gate', code: widelyOwnedAccounted },
    ] },
    { id: 'claims_are_reasoned', checks: [
      { id: 'ranked_on_usage', label: "Ranks a claim without role, usage or matchup", weight: 3 },
      { id: 'competition_read', label: "Urgency ignores rival teams' needs", weight: 3 },
      { id: 'close_call_news', label: "A close call doesn't cite the news", weight: 3 },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'bye_coverage', label: "Misses a bye week in the plan", weight: -2, code: byeCoverage },
      { id: 'crowd_metric_as_value', label: "Treats a crowd number as scoring evidence", weight: -3 },
    ] },
  ],
};
