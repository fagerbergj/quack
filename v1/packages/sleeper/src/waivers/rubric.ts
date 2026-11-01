import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import type { Candidate, WaiversCard } from './card.ts';
import type { WaiverFacts } from './facts.ts';

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
      facts: [{ id: 'facts', state: f as never }],
      row: rows,
      close: card.candidates.filter((r) => (r.news?.length ?? 0) > 0).map(unit),
      card: [{ id: 'card', state: { summary: card.summary, waiver_type: f.waiverType, rows: card.candidates.map((r) => ({ rank: r.rank, add: r.player.name, drop: r.drop.player.name, why: r.why })), table: f.table } as never }],
    };
  };
}

// The audit's waivers table (docs/v1/audits/sleeper.md), unreplayed: D45 asks for replay before any weight gates delivery.
export const waiversRubric: Rubric = {
  name: 'sleeper.waivers',
  version: 1,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [
      { id: 'context_fields_match', weight: 'gate', code: contextFieldsMatch },
      { id: 'drop_role_matches_roster', weight: 'gate', code: dropRoleMatchesRoster },
      { id: 'widely_owned_accounted', weight: 'gate', code: widelyOwnedAccounted },
    ] },
    { id: 'claims_are_reasoned', checks: [
      { id: 'sized_to_waiver_type', weight: 4, scope: 'row', question: 'Is the claim sized in this league\'s own currency: a share of remaining FAAB in a FAAB league, or the queue position spent in a rolling or reverse-standings one?' },
      { id: 'ranked_on_usage', weight: 3, scope: 'row', question: 'Does the rank rest on role and usage, with trending and ownership counts framed as scarcity?' },
      { id: 'competition_read', weight: 3, scope: 'card', question: 'Does the urgency or size of a claim name a competing team\'s need, priority or FAAB left from the table?' },
      { id: 'close_call_news', weight: 3, scope: 'close', question: 'Does this close top claim or drop reflect the attached `news`, naming the right player for each report and the Sleeper `designations`?' },
      { id: 'bye_coverage', weight: 2, scope: 'row', question: 'Does it flag a bye inside the lead time, and a shared bye as two slots?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'crowd_metric_as_value', weight: -3, scope: 'row', question: 'Is a trending or ownership number given as the reason the player is good?' },
      { id: 'ordered_fallback_flagged', weight: -4, scope: 'card', question: 'Does it describe claims that share a drop as a mistake or a conflict?' },
      { id: 'faab_language_on_rolling', weight: -4, code: faabLanguageOnRolling },
    ] },
  ],
};
