import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import type { Candidate, WaiversCard } from './card.ts';
import { worth, type WaiverFacts } from './facts.ts';

const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });
const parse = (artifact: string) => JSON.parse(artifact) as WaiversCard;
const factsOf = (ev: Evidence) => ev.facts[0].state as unknown as WaiverFacts;

function contextFieldsMatch(artifact: string, ev: Evidence): CodeResult {
  const c = parse(artifact);
  const f = factsOf(ev);
  const wrong = (['waiver_type', 'my_priority', 'faab_left', 'teams'] as const).filter((k) => c[k] !== { waiver_type: f.waiverType, my_priority: f.myPriority, faab_left: f.faabLeft, teams: f.teams }[k]);
  return share(4 - wrong.length, 4, `${wrong.join(', ')} differ from the tools`);
}

// The label is checked against Sleeper's own starters, not the card's bookkeeping; a starter drop must also be said aloud.
function dropRoleMatchesRoster(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const rows = parse(artifact).candidates;
  const bad = rows.filter((r) => f.roster[r.drop.player.id]?.role !== r.drop.role || (r.drop.role === 'starter' && !/starter|starting/i.test(r.why)));
  return share(rows.length - bad.length, rows.length, `the drop of ${bad.map((r) => r.drop.player.name).join(', ')} is mislabeled or a starter not stated as one`);
}

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

const FAAB_WORDS = /\bfaab\b|\bbids?\b|\bbidding\b|\$\s?\d|\bbudget\b/i;

// A pitfall: met is 1 when the card talks FAAB in a league that has none.
function faabLanguageOnRolling(artifact: string): CodeResult {
  const c = parse(artifact);
  if (c.waiver_type === 'faab') return { met: 0 };
  const hits = [c.summary, ...c.candidates.map((r) => r.why)].filter((t) => FAAB_WORDS.test(t));
  return { met: hits.length ? 1 : 0, detail: hits.length ? `FAAB language in a ${c.waiver_type} league: ${hits[0]}` : undefined };
}

/** Sleeper's designation by player name, so a check never pins one player's injury on another. */
const designations = (r: Candidate) => Object.fromEntries([r.player, r.drop.player].map((p) => [p.name, p.inj ?? 'none']));

/** Row units carry the claim, its news and designations, and the league's currency so a check sees only what it needs. */
export function waiversEvidence(f: WaiverFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse(sealed.content);
    const unit = (r: Candidate) => ({
      id: `claim${r.rank}`,
      state: { row: r, week: f.week, waiver_type: f.waiverType, my_priority: f.myPriority, faab_left: f.faabLeft, teams: f.teams, news: r.news ?? [], designations: designations(r), bye_exposure: f.table.find((t) => t.mine)?.byes ?? [] } as never,
    });
    const rows = card.candidates.map(unit);
    return {
      facts: [{ id: 'facts', whole: true as const, state: f as never }],
      row: rows,
      close: card.candidates.filter((r) => (r.news?.length ?? 0) > 0).map(unit),
      card: [{ id: 'card', whole: true as const, state: { summary: card.summary, waiver_type: f.waiverType, rows: card.candidates.map((r) => ({ rank: r.rank, add: r.player.name, drop: r.drop.player.name, why: r.why })), table: f.table } as never }],
    };
  };
}

// The audit's waivers table (docs/v1/audits/sleeper.md), unreplayed: D45 asks for replay before any weight gates delivery.
export const waiversRubric: Rubric = {
  name: 'sleeper.waivers',
  version: 4,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [
      { id: 'context_fields_match', label: "Card facts differ from the league data", weight: 'gate', code: contextFieldsMatch },
      { id: 'drop_role_matches_roster', label: "Drops a player in the wrong roster role", weight: 'gate', code: dropRoleMatchesRoster },
      { id: 'drop_not_worth_more', label: "Drops a player worth more than the add", weight: 'gate', code: dropNotWorthMore },
      { id: 'widely_owned_accounted', label: "Ignores that the add is widely owned", weight: 'gate', code: widelyOwnedAccounted },
    ] },
    { id: 'claims_are_reasoned', checks: [
      { id: 'sized_to_waiver_type', label: "Bid not sized to this league's waivers", weight: 4, scope: 'row', question: 'Is the claim sized in this league\'s own currency: a share of remaining FAAB in a FAAB league, or the queue position spent in a rolling or reverse-standings one?' },
      { id: 'ranked_on_usage', label: "Ranks a claim without role or usage", weight: 3, scope: 'row', question: 'Does the rank rest on role and usage, with trending and ownership counts framed as scarcity?' },
      { id: 'competition_read', label: "Urgency ignores rival teams' needs", weight: 3, scope: 'card', question: 'Does the urgency or size of a claim name a competing team\'s need, priority or FAAB left from the table?' },
      { id: 'close_call_news', label: "A close call doesn't cite the news", weight: 3, scope: 'close', question: 'Does this close top claim or drop reflect the attached `news`, naming the right player for each report and the Sleeper `designations`?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'bye_coverage', label: "Misses a bye week in the plan", weight: -2, scope: 'row', question: 'Does it miss a bye inside the lead time, or count a shared bye as one slot?' },
      { id: 'crowd_metric_as_value', label: "Says a player is good because he trends", weight: -3, scope: 'row', question: 'Is a trending or ownership number given as the reason the player is good?' },
      { id: 'ordered_fallback_flagged', label: "Calls backup claims a conflict", weight: -4, scope: 'card', question: 'Does it describe claims that share a drop as a mistake or a conflict?' },
      { id: 'faab_language_on_rolling', label: "Talks about FAAB in a non-FAAB league", weight: -4, code: faabLanguageOnRolling },
    ] },
  ],
};
