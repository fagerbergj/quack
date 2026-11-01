import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import { closeCalls, type Facts } from './facts.ts';
import type { LineupCard, Row } from './card.ts';

const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });
const parse = (artifact: string) => JSON.parse(artifact) as LineupCard;
const factsOf = (ev: Evidence) => ev.facts[0].state as unknown as Facts;
const changed = (r: Row, f: Facts) => r.player.id !== f.current[r.slot];

function everySlotDecided(artifact: string, ev: Evidence): CodeResult {
  const { starters } = parse(artifact);
  const missing = factsOf(ev).slots.filter((s) => !starters.some((r) => r.slot === s && r.player?.id));
  return share(factsOf(ev).slots.length - missing.length, factsOf(ev).slots.length, `no verdict for ${missing.join(', ')}`);
}

// Change is judged against Sleeper's own lineup, not the row's `action`: the card's bookkeeping proves nothing.
function onePlayerPerSlot(artifact: string): CodeResult {
  const ids = parse(artifact).starters.map((r) => r.player.id);
  const dup = [...new Set(ids.filter((id, i) => ids.indexOf(id) !== i))];
  return share(ids.length - dup.length, ids.length, `started in more than one slot: ${dup.join(', ')}`);
}

function noLockedSwap(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const rows = parse(artifact).starters.filter((r) => changed(r, f));
  const bad = rows.filter((r) => f.players[r.player.id]?.game?.locked || (f.current[r.slot] && f.players[f.current[r.slot]!]?.game?.locked));
  return share(rows.length - bad.length, rows.length, `${bad.map((r) => r.slot).join(', ')} swap a player whose game has locked`);
}

function figuresMatchTools(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const card = parse(artifact);
  const rows = [...card.starters.map((r) => ({ at: r.slot, id: r.player.id, proj: r.proj, game: r.game })), ...card.bench.map((b) => ({ at: `bench ${b.player.name}`, id: b.player.id, proj: b.proj, game: b.game }))];
  const wrong = rows.filter((r) => !f.players[r.id] || f.players[r.id].proj !== r.proj || JSON.stringify(f.players[r.id].game) !== JSON.stringify(r.game));
  return share(rows.length - wrong.length, rows.length, `proj, opponent or kickoff differ from the tools for ${wrong.map((r) => r.at).join(', ')}`);
}

function summaryNamesChanges(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const card = parse(artifact);
  const rows = card.starters.filter((r) => changed(r, f));
  const text = card.summary.toLowerCase();
  if (!rows.length) return { met: /no change/.test(text) ? 1 : 0, detail: 'there are no changes and the summary does not say so' };
  const missed = rows.filter((r) => ![r.player.name, f.current[r.slot] ? f.players[f.current[r.slot]!].name : ''].every((n) => text.includes(n.toLowerCase())));
  return share(rows.length - missed.length, rows.length, `the summary does not name both players of the ${missed.map((r) => r.slot).join(', ')} change`);
}

/** Row units carry the incumbent and the baseline pick so a check sees only what it needs. */
export function lineupEvidence(f: Facts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse(sealed.content);
    const unit = (r: Row) => ({
      id: r.slot,
      state: { slot: r.slot, week: f.week, row: r, incumbent: f.current[r.slot] ? f.players[f.current[r.slot]!] : null, baseline: f.baseline.find((b) => b.slot === r.slot) ?? null, news: f.news[r.player.id] ?? [] } as never,
    });
    // Locked rows keep their player by construction; no judgment is asked of them beyond a plain action.
    const where = (keep: (r: Row) => boolean) => card.starters.filter(keep).map(unit);
    const live = (keep: (r: Row) => boolean) => where((r) => !r.locked && keep(r));
    const close = new Set(closeCalls(f).map((p) => p.id));
    return {
      facts: [{ id: 'facts', state: f as never }],
      row: where(() => true),
      differs: live((r) => f.baseline.find((b) => b.slot === r.slot)?.id !== r.player.id),
      changed: live((r) => changed(r, f)),
      close: live((r) => close.has(r.player.id) || (!!f.current[r.slot] && close.has(f.current[r.slot]!))),
      designated: live((r) => !!r.player.inj || !!f.players[f.current[r.slot] ?? '']?.inj),
      card: [{ id: 'card', state: { summary: card.summary, rows: card.starters.map((r) => ({ slot: r.slot, player: r.player.name, why: r.why })) } as never }],
    };
  };
}

// The audit's lineup table (docs/v1/audits/sleeper.md), unreplayed: D45 asks for replay before any weight gates delivery.
export const lineupRubric: Rubric = {
  name: 'sleeper.lineup',
  version: 1,
  threshold: 0.8,
  criteria: [
    { id: 'legal_and_complete', checks: [
      { id: 'every_slot_decided', weight: 'gate', code: everySlotDecided },
      { id: 'one_player_per_slot', weight: 'gate', code: onePlayerPerSlot },
      { id: 'no_locked_swap', weight: 'gate', code: noLockedSwap },
      { id: 'figures_match_tools', weight: 'gate', code: figuresMatchTools },
      { id: 'summary_names_changes', weight: 'gate', code: summaryNamesChanges },
    ] },
    { id: 'calls_are_reasoned', checks: [
      { id: 'names_baseline_it_beats', weight: 3, scope: 'differs', question: 'This starter differs from the best-by-projection baseline for the slot. Does `why` name the baseline player and what changed the call?' },
      { id: 'close_call_news', weight: 3, scope: 'close', question: 'This is a Questionable or limited-practice starter, or a bench player within the FLEX tie-break gap. Does `why` cite news that bears on the call?' },
      { id: 'designation_weighted', weight: 3, scope: 'designated', question: 'Does the call treat an injury designation as a play rate, not as in or out?' },
      { id: 'picks_an_action', weight: 2, scope: 'row', question: 'Does the row start, sit or keep a player, rather than leave the slot open?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'matchup_overrides_talent', weight: -4, scope: 'changed', question: 'Does this change bench a clearly better healthy player for matchup alone?' },
      { id: 'weather_mentioned_unmoved', weight: -2, scope: 'card', question: 'Does the card mention weather for a call that weather did not move?' },
    ] },
  ],
};
