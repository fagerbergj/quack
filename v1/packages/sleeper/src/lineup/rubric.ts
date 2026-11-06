import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import { factsOf as factsIn, parseCard, share } from '../rubric-kit.ts';
import { closeCalls, type Facts } from './facts.ts';
import type { LineupCard, Row } from './card.ts';

const parse = (artifact: string) => parseCard<LineupCard>(artifact);
const factsOf = (ev: Evidence) => factsIn<Facts>(ev);
const changed = (r: Row, f: Facts) => r.player.id !== f.current[r.slot];

function everySlotDecided(artifact: string, ev: Evidence): CodeResult {
  const { starters } = parse(artifact);
  const missing = factsOf(ev).slots.filter((s) => !starters.some((r) => r.slot === s && r.player?.id));
  return share(factsOf(ev).slots.length - missing.length, factsOf(ev).slots.length, `no verdict for ${missing.join(', ')}`);
}

function noLockedSwap(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const rows = parse(artifact).starters.filter((r) => changed(r, f));
  const bad = rows.filter((r) => f.players[r.player.id]?.game?.locked || (f.current[r.slot] && f.players[f.current[r.slot]!]?.game?.locked));
  return share(rows.length - bad.length, rows.length, `${bad.map((r) => r.slot).join(', ')} swap a player whose game has locked`);
}

/** Sleeper's designation by player name, so a check never pins one player's injury on another. */
const designations = (r: Row, f: Facts) => Object.fromEntries([r.player, ...(f.current[r.slot] ? [f.players[f.current[r.slot]!]] : [])].map((p) => [p.name, p.inj ?? 'none']));

/** Row units carry the incumbent and the baseline pick so a check sees only what it needs. */
export function lineupEvidence(f: Facts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse(sealed.content);
    const unit = (r: Row) => ({
      id: r.slot,
      state: { slot: r.slot, week: f.week, row: r, incumbent: f.current[r.slot] ? f.players[f.current[r.slot]!] : null, baseline: f.baseline.find((b) => b.slot === r.slot) ?? null, news: r.news ?? [], designations: designations(r, f) } as never,
    });
    // Locked rows keep their player by construction; no judgment is asked of them beyond a plain action.
    const where = (keep: (r: Row) => boolean) => card.starters.filter(keep).map(unit);
    const live = (keep: (r: Row) => boolean) => where((r) => !r.locked && keep(r));
    const inBaseline = new Set(f.baseline.map((b) => b.id));
    const close = new Set(closeCalls(f).map((p) => p.id));
    return {
      facts: [{ id: 'facts', whole: true as const, state: f as never }],
      row: where(() => true),
      // By membership, not slot: the baseline may seat the same player in RB2 that the card keeps in RB1.
      differs: live((r) => changed(r, f) && !inBaseline.has(r.player.id)),
      changed: live((r) => changed(r, f)),
      close: live((r) => (r.news?.length ?? 0) > 0 && (close.has(r.player.id) || (!!f.current[r.slot] && close.has(f.current[r.slot]!)))),
      designated: live((r) => !!r.player.inj || !!f.players[f.current[r.slot] ?? '']?.inj),
      card: [{ id: 'card', whole: true as const, state: { summary: card.summary, rows: card.starters.map((r) => ({ slot: r.slot, player: r.player.name, why: r.why })) } as never }],
    };
  };
}

// The card builder seats one player per slot, copies every figure and writes the summary from the changes, so only what the model can get wrong is gated.
export const lineupRubric: Rubric = {
  name: 'sleeper.lineup',
  version: 5,
  threshold: 0.8,
  criteria: [
    { id: 'legal_and_complete', checks: [
      { id: 'every_slot_decided', label: "A lineup slot is left unset", weight: 'gate', code: everySlotDecided },
      { id: 'no_locked_swap', label: "Swaps a player who is already locked", weight: 'gate', code: noLockedSwap },
    ] },
    { id: 'calls_are_reasoned', checks: [
      { id: 'names_baseline_it_beats', label: "Doesn't say who the swap beats", weight: 3, scope: 'differs', question: 'This change starts a player the best-by-projection baseline does not. Does `why` name the baseline player and what changed the call?' },
      { id: 'close_call_news', label: "A close call doesn't cite the news", weight: 3, scope: 'close', question: 'This is a Questionable starter, or a bench player within the FLEX tie-break gap. Does `why` reflect the attached `news`, naming the right player for each report and the Sleeper `designations`?' },
      { id: 'designation_weighted', label: "Treats an injury tag as in or out", weight: 3, scope: 'designated', question: 'Does the call treat an injury designation as a play rate, not as in or out?' },
      { id: 'picks_an_action', label: "A slot has no start or sit call", weight: 2, scope: 'row', question: 'Does the row start, sit or keep a player, rather than leave the slot open?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'matchup_overrides_talent', label: "Benches a better player over matchup", weight: -4, scope: 'changed', question: 'Does this change bench a clearly better healthy player for matchup alone?' },
      { id: 'weather_mentioned_unmoved', label: "Blames weather for a call weather didn't change", weight: -2, scope: 'card', question: 'Does the card mention weather for a call that weather did not move?' },
    ] },
  ],
};
