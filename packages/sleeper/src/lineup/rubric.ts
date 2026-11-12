import type { CodeResult, Declaration, Evidence, Inputs, Question, Rubric, Scope } from '@quack/judge';
import { parseCard, share } from '../rubric-kit.ts';
import type { Facts, PlayerFact } from './facts.ts';
import type { LineupCard, Row } from './card.ts';

const parse = (artifact: string) => parseCard<LineupCard>(artifact);
const factsOf = (ev: Evidence) => ev.facts[0].state.facts as unknown as Facts;
const changed = (r: Row, f: Facts) => r.player.id !== f.current[r.slot];

function everySlotDecided(artifact: string, ev: Evidence): CodeResult {
  const { starters } = parse(artifact);
  const missing = factsOf(ev).slots.filter((s) => !starters.some((r) => r.slot === s && r.player?.id));
  return share(factsOf(ev).slots.length - missing.length, factsOf(ev).slots.length, `no verdict for ${missing.join(', ')}`);
}

// An open slot's incumbent has not locked and a locked slot keeps its player by construction, so only the player brought in can be locked.
function noLockedSwap(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const rows = parse(artifact).starters.filter((r) => changed(r, f));
  const bad = rows.filter((r) => f.players[r.player.id]?.game?.locked);
  return share(rows.length - bad.length, rows.length, `${bad.map((r) => r.slot).join(', ')} swap a player whose game has locked`);
}

/** Sleeper's designation by player name, so a check never pins one player's injury on another. */
const designations = (r: Row, f: Facts) => Object.fromEntries([r.player, ...(f.current[r.slot] ? [f.players[f.current[r.slot]!]] : [])].map((p) => [p.name, p.inj ?? 'none']));
const player = (p: PlayerFact | undefined) => p && { name: p.name, pos: p.pos, proj: p.proj, inj: p.inj, game: p.game };

// A why names a player in full or by surname ("Hubbard"); a generational suffix is not a surname.
const surname = (name: string) => name.replace(/\s+(Jr\.?|Sr\.?|II|III|IV|V)$/, '').split(' ').at(-1)!;
const names = (text: string, name: string) => [name, surname(name)].some((n) => new RegExp(`\\b${n.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\b`).test(text));

/** The row's own reports and those of every rostered player its why names, each once: a why may cite another player's report from the card. */
function newsFor(r: Row, f: Facts) {
  const cited = Object.values(f.players).filter((p) => names(r.why, p.name)).flatMap((p) => (f.news[p.id] ?? []).map((n) => ({ ...n, player: p.name })));
  return [...new Map([...(r.news ?? []), ...cited].map((n) => [`${n.player}\n${n.url}`, n])).values()];
}

/** Each unlocked row as one record: what a check reads, and the flags its scope keeps it by. Locked rows keep their player and are asked nothing. */
export function lineupRows(f: Facts, card: LineupCard) {
  const inBaseline = new Set(f.baseline.map((b) => b.id));
  return card.starters.filter((r) => !r.locked).map((r) => {
    const incumbent = f.current[r.slot] ? f.players[f.current[r.slot]!] : undefined;
    const b = f.baseline.find((x) => x.slot === r.slot);
    return {
      id: r.slot, slot: r.slot, start: player(f.players[r.player.id]), action: r.action, why: r.why,
      incumbent: player(incumbent) ?? null, baseline: b ? { name: b.name, proj: b.proj } : null, news: newsFor(r, f), designations: designations(r, f),
      // By membership, not slot: the baseline may seat the same player in RB2 that the card keeps in RB1.
      differs: changed(r, f) && !inBaseline.has(r.player.id),
      // News is fetched only for close calls, so a row with news is a close call.
      hasNews: (r.news?.length ?? 0) > 0,
      designated: !!r.player.inj || !!incumbent?.inj,
    };
  });
}

/** The judge's named texts: the facts the card was built on, and its unlocked rows. */
export const lineupInputsOf = (f: Facts, content: string): Inputs => ({ facts: JSON.stringify(f), rows: JSON.stringify(lineupRows(f, parse(content))) });
export const lineupInputs = (f: Facts) => async (_api: unknown, sealed: { content: string }): Promise<Inputs> => lineupInputsOf(f, sealed.content);

// Every row is its own request with nothing shared: batched, the 27B passed more.
const rows = (flag: string, pick: string[]): Scope => ({ split: { name: 'jsonItems', from: 'rows', pick, where: [{ path: flag, op: 'truthy' }] }, piece: 'row' });
const yes = (instructions: string): Question => ({ type: 'bool', instructions });
// A pitfall is asked as the defect, so "yes" flags it and the item holds on "no".
const defect = (instructions: string): Question => ({ type: 'bool', instructions, pass: ['no'] });
const SHARE = { name: 'share' } as const;

/** How each model check of lineupRubric is asked: per unlocked row its scope keeps, alone. */
export const lineupDeclaration: Declaration = {
  scopes: {
    facts: { split: { name: 'whole', from: 'facts' }, piece: 'facts' },
    differs: rows('differs', ['slot', 'start', 'action', 'why', 'incumbent', 'baseline']),
    news: rows('hasNews', ['slot', 'start', 'action', 'why', 'incumbent', 'news', 'designations']),
    designated: rows('designated', ['slot', 'start', 'action', 'why', 'incumbent', 'designations']),
  },
  checks: {
    names_baseline_it_beats: { scope: 'differs', aggregate: SHARE, question: yes('This change starts a player the best-by-projection baseline does not. Does `why` name the baseline player and what changed the call?') },
    // Weighing `why` against the reports is a claim against evidence, which the decision model passes unread, so the 27B decides it.
    close_call_news: { scope: 'news', aggregate: SHARE, decisionModel: false, question: yes('This is a Questionable starter, or a bench player within the FLEX tie-break gap. Does `why` reflect the attached `news`, naming the right player for each report and the Sleeper `designations`?') },
    designation_weighted: { scope: 'designated', aggregate: SHARE, question: yes('Does the call treat an injury designation as a play rate, not as in or out?') },
    // A change the baseline also makes follows the projection, which already prices the matchup, so only one against it can bench on matchup alone.
    matchup_overrides_talent: { scope: 'differs', aggregate: SHARE, question: defect('Does this change bench a clearly better healthy player for matchup alone?') },
  },
};

// The card builder seats one player per slot with an action, copies every figure and writes the summary from the changes, so only what the model can get wrong is checked.
// Lineup weights are unreplayed: replay them before any weight gates delivery.
export const lineupRubric: Rubric = {
  name: 'sleeper.lineup',
  version: 6,
  threshold: 0.8,
  criteria: [
    { id: 'legal_and_complete', checks: [
      { id: 'every_slot_decided', label: "A lineup slot is left unset", weight: 'gate', code: everySlotDecided },
      { id: 'no_locked_swap', label: "Swaps a player who is already locked", weight: 'gate', code: noLockedSwap },
    ] },
    { id: 'calls_are_reasoned', checks: [
      { id: 'names_baseline_it_beats', label: "Doesn't say who the swap beats", weight: 3 },
      { id: 'close_call_news', label: "A close call doesn't cite the news", weight: 3 },
      { id: 'designation_weighted', label: "Treats an injury tag as in or out", weight: 3 },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'matchup_overrides_talent', label: "Benches a better player over matchup", weight: -4 },
    ] },
  ],
};
