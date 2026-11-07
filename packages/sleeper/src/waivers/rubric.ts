import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import { factsOf as factsIn, parseCard, share, type NewsGate } from '../rubric-kit.ts';
import type { Candidate, WaiversCard } from './card.ts';
import { worth, type WaiverFacts } from './facts.ts';

const parse = (artifact: string) => parseCard<WaiversCard>(artifact);
const factsOf = (ev: Evidence) => factsIn<WaiverFacts>(ev);

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

/** Row units carry the claim, its news and designations, and the league's currency so a check sees only what it needs. */
export function waiversEvidence(f: WaiverFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse(sealed.content);
    const unit = (r: Candidate) => ({
      id: `claim${r.rank}`,
      state: { row: r, week: f.week, waiver_type: f.waiverType, my_priority: f.myPriority, faab_left: f.faabLeft, teams: f.teams, news: r.news ?? [], designations: designations(r) } as never,
    });
    const rows = card.candidates.map(unit);
    const cardUnit = { id: 'card', whole: true as const, state: { summary: card.summary, waiver_type: f.waiverType, rows: card.candidates.map((r) => ({ rank: r.rank, add: r.player.name, drop: r.drop.player.name, why: r.why })), table: f.table } as never };
    return {
      facts: [{ id: 'facts', whole: true as const, state: f as never }],
      row: rows,
      close: card.candidates.filter((r) => (r.news?.length ?? 0) > 0).map(unit),
      card: [cardUnit],
    };
  };
}

// The card builder copies the league context, each drop's roster role, each add's bye and the claim size from the facts, and refuses a bid where the league has none, so none is checked here.
// Waivers weights are unreplayed: replay them before any weight gates delivery.
export const waiversRubric: Rubric = {
  name: 'sleeper.waivers',
  version: 7,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [
      { id: 'drop_not_worth_more', label: "Drops a player worth more than the add", weight: 'gate', code: dropNotWorthMore },
      { id: 'widely_owned_accounted', label: "Ignores that the add is widely owned", weight: 'gate', code: widelyOwnedAccounted },
    ] },
    { id: 'claims_are_reasoned', checks: [
      { id: 'sized_to_waiver_type', label: "Bid not sized to this league's waivers", weight: 4, scope: 'row', question: 'Is the claim sized in this league\'s own currency: a share of remaining FAAB in a FAAB league, or the queue position spent in a rolling or reverse-standings one?' },
      { id: 'ranked_on_usage', label: "Ranks a claim without role, usage or matchup", weight: 3, scope: 'row', question: 'Does the rank rest on the player\'s role and usage, or for a team defense or kicker, its matchup?' },
      { id: 'competition_read', label: "Urgency ignores rival teams' needs", weight: 3, scope: 'card', question: 'Does the urgency or size of a claim name a competing team\'s need, priority or FAAB left from the table?' },
      { id: 'close_call_news', label: "A close call doesn't cite the news", weight: 3, scope: 'close', question: 'Does this close top claim or drop reflect the attached `news`, naming the right player for each report and the Sleeper `designations`?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'bye_coverage', label: "Misses a bye week in the plan", weight: -2, code: byeCoverage },
      { id: 'crowd_metric_as_value', label: "Treats a crowd number as scoring evidence", weight: -3, scope: 'row', question: 'Is a trending or ownership number given as evidence the player will score, rather than as how hard he is to land?' },
      { id: 'ordered_fallback_flagged', label: "Calls backup claims a conflict", weight: -4, scope: 'card', question: 'Does it describe claims that share a drop as a mistake or a conflict?' },
    ] },
  ],
};

/** The waivers job's news checks, gated by the live judge and its replay alike. */
export const WAIVERS_NEWS: NewsGate = { checks: ['close_call_news'], what: 'close-call news' };
