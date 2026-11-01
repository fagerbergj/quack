import type { ArtifactKind } from '@quack/sdk';
import type { Facts, Game, PlayerFact } from './facts.ts';

/** The page reads every Sleeper job's result from this kind; `kind` inside the file says which card it is. */
export const cardsKind: ArtifactKind = {
  kind: 'sleeper.cards',
  path: 'cards.json',
  validate(text) {
    try {
      const c = JSON.parse(text);
      return c?.kind === 'lineup' && Array.isArray(c.starters) && typeof c.summary === 'string' ? [] : ['a lineup card needs kind "lineup", starters and a summary'];
    } catch {
      return ['cards.json is not JSON'];
    }
  },
};

export type PlayerRef = { id: string; name: string; pos: string; team: string | null; inj: string | null };
export type Row = { slot: string; player: PlayerRef; proj: number; action: 'start' | 'keep'; locked: boolean; replaces: PlayerRef | null; game: Game | null; why: string };
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

/** Locked slots keep their player by construction; open slots take the model's pick, whose figures come from the facts, never the model. */
export function buildCard(f: Facts, d: Decision): LineupCard {
  const row = (slot: string, p: PlayerFact, why: string): Row => {
    const inc = f.current[slot] ? f.players[f.current[slot]!] : null;
    const change = inc?.id !== p.id;
    return { slot, player: ref(p), proj: p.proj, action: change ? 'start' : 'keep', locked: !f.open.includes(slot), replaces: change && inc ? ref(inc) : null, game: p.game, why };
  };
  const starters = f.slots.flatMap((slot) => {
    if (!f.open.includes(slot)) return [row(slot, f.players[f.current[slot]!], 'Locked: the game has started.')];
    const pick = d.slots.find((s) => s.slot === slot);
    if (!pick) return [];
    if (!f.players[pick.playerId]) throw new Error(`the model picked ${pick.playerId} for ${slot}, who is not on the roster`);
    return [row(slot, f.players[pick.playerId], pick.why)];
  });
  const started = new Set(starters.map((r) => r.player.id));
  const bench = Object.values(f.players).filter((p) => !started.has(p.id)).sort((a, b) => b.proj - a.proj).map((p) => ({ player: ref(p), proj: p.proj, game: p.game }));
  return { kind: 'lineup', league: f.league, week: f.week, team: f.team, opponent: f.opponent, my_proj: sum(starters), opp_proj: f.oppProj, summary: d.summary, starters, bench };
}
