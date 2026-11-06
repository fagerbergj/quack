import type { Json } from './ports.ts';

// The scores feed was seen spelling it 'cancelled'; the schedule feed has been documented as 'canceled'. Either voids the game.
const VOID = new Set(['cancelled', 'canceled', 'postponed']);

/** A game that will not be played at its listed time, by its schedule `status` or its score `metadata.status`. */
export const voidGame = (status: unknown) => VOID.has(String(status));

/** Kickoff instant (ms) by game id from a week's scores; a flex-scheduled game's start_time is a placeholder, so it has none. */
export function kickoffsByGame(scores: Json[]): Map<string, number> {
  const real = scores.filter((s) => s.start_time > 0 && !voidGame(s.metadata?.status) && s.metadata?.status !== 'flex-schedule');
  return new Map(real.map((s) => [s.game_id, s.start_time as number]));
}

/** The week's known kickoff instants. */
export const kickoffTimes = (scores: Json[]) => [...kickoffsByGame(scores).values()];
