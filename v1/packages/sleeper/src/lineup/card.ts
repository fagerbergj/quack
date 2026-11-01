import type { ArtifactKind } from '@quack/sdk';
import type { Facts, Game, NewsItem, PlayerFact } from './facts.ts';
import { fits } from './slots.ts';

/** The page reads every Sleeper job's result from this kind; `kind` inside the file says which card it is. */
export const cardsKind: ArtifactKind = {
  kind: 'sleeper.cards',
  path: 'cards.json',
  validate(text) {
    try {
      const c = JSON.parse(text);
<<<<<<< HEAD
      const rows = c?.kind === 'lineup' ? c.starters : c?.kind === 'waivers' ? c.candidates : c?.kind === 'history' ? c.seasons : c?.kind === 'trade' ? c.offers : c?.kind === 'trade-finder' ? c.suggestions : c?.kind === 'trends' ? c.items : undefined;
      return Array.isArray(rows) && typeof c.summary === 'string' ? [] : ['a card needs a kind (lineup, waivers, trade, trade-finder, trends, history) with its rows, and a summary'];
=======
      const rows = c?.kind === 'lineup' ? c.starters : c?.kind === 'waivers' ? c.candidates : c?.kind === 'digest' ? c.games : c?.kind === 'retro' ? c.misses : c?.kind === 'trade' ? c.offers : c?.kind === 'trade-finder' ? c.suggestions : undefined;
      return Array.isArray(rows) && typeof c.summary === 'string' ? [] : ['a card needs a kind (lineup, waivers, digest, retro, trade, trade-finder) with its rows, and a summary'];
>>>>>>> 2943101b (feat(v1): Sleeper league report jobs, digest and retro)
    } catch {
      return ['cards.json is not JSON'];
    }
  },
};

export type PlayerRef = { id: string; name: string; pos: string; team: string | null; inj: string | null };
export type Row = { slot: string; player: PlayerRef; proj: number; action: 'start' | 'keep'; locked: boolean; replaces: PlayerRef | null; game: Game | null; why: string;
  /** Attached by code from the facts, for the starter and the player replaced, so a link is never the model's to copy. */
  news: Array<NewsItem & { player: string }>;
};
export type LineupCard = {
  kind: 'lineup';
  league: string;
  week: number;
  team: string;
  opponent: string | null;
  my_proj: number;
  opp_proj: number | null;
  summary: string;
  starters: Row[];
  bench: Array<{ player: PlayerRef; proj: number; game: Game | null }>;
};

/** What the model decides for the open slots (SL-6: no predictions in it). */
export type Decision = { slots: Array<{ slot: string; playerId: string; why: string }>; summary: string };

export const ref = (p: PlayerFact): PlayerRef => ({ id: p.id, name: p.name, pos: p.pos, team: p.team, inj: p.inj });
const sum = (rows: Array<{ proj: number }>) => Math.round(rows.reduce((n, r) => n + r.proj, 0) * 100) / 100;

type SlotPick = { playerId: string; why: string };

/** A change is a set difference, so a starter the model moved to another open slot goes back to his current slot when the player there fits his. */
function settle(f: Facts, picks: Record<string, SlotPick>): void {
  for (const slot of f.open) {
    const cur = f.current[slot];
    const at = f.open.find((o) => o !== slot && cur && picks[o]?.playerId === cur);
    const mine = picks[slot];
    if (!at || !mine || !f.players[mine.playerId] || !fits(at, f.players[mine.playerId].pos)) continue;
    [picks[slot], picks[at]] = [picks[at], mine];
  }
}

/** Locked slots keep their player by construction; open slots take the model's pick, whose figures come from the facts, never the model. */
export function buildCard(f: Facts, d: Decision): LineupCard {
  const row = (slot: string, p: PlayerFact, why: string): Row => {
    const inc = f.current[slot] ? f.players[f.current[slot]!] : null;
    const change = inc?.id !== p.id;
    const news = [p, ...(change && inc ? [inc] : [])].flatMap((x) => (f.news[x.id] ?? []).map((n) => ({ ...n, player: x.name })));
    return { slot, player: ref(p), proj: p.proj, action: change ? 'start' : 'keep', locked: !f.open.includes(slot), replaces: change && inc ? ref(inc) : null, game: p.game, why, news };
  };
  const picks: Record<string, SlotPick> = Object.fromEntries(d.slots.map((s) => [s.slot, s]));
  settle(f, picks);
  const starters = f.slots.flatMap((slot) => {
    if (!f.open.includes(slot)) return [row(slot, f.players[f.current[slot]!], 'Locked: the game has started.')];
    const pick = picks[slot];
    if (!pick) return [];
    if (!f.players[pick.playerId]) throw new Error(`the model picked ${pick.playerId} for ${slot}, who is not on the roster`);
    return [row(slot, f.players[pick.playerId], pick.why)];
  });
  const started = new Set(starters.map((r) => r.player.id));
  if (started.size < starters.length) throw new Error(`the model started one player in two slots: ${starters.map((r) => `${r.slot}=${r.player.name}`).join(', ')}`);
  const bench = Object.values(f.players).filter((p) => !started.has(p.id)).sort((a, b) => b.proj - a.proj).map((p) => ({ player: ref(p), proj: p.proj, game: p.game }));
  return { kind: 'lineup', league: f.league, week: f.week, team: f.team, opponent: f.opponent, my_proj: sum(starters), opp_proj: f.oppProj, summary: starters.some((r) => r.action === 'start') || /no change/i.test(d.summary) ? d.summary : 'No changes: the lineup stands.', starters, bench };
}
