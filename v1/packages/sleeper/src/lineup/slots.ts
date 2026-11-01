const FLEX: Record<string, string[]> = { FLEX: ['RB', 'WR', 'TE'], WRRB_FLEX: ['RB', 'WR'], REC_FLEX: ['WR', 'TE'], SUPER_FLEX: ['QB', 'RB', 'WR', 'TE'] };
const RESERVE = new Set(['BN', 'IR', 'TAXI']);

/** Sleeper's `roster_positions` without bench, IR and taxi: the slots a lineup fills, in starters order. */
export const lineupSlots = (positions: string[]) => positions.filter((p) => !RESERVE.has(p));

/** RB, RB becomes RB1, RB2; a lone slot keeps its name. */
export function numberSlots(slots: string[]): string[] {
  const seen: Record<string, number> = {};
  return slots.map((s) => (slots.filter((x) => x === s).length === 1 ? s : `${s}${(seen[s] = (seen[s] ?? 0) + 1)}`));
}

const base = (slot: string) => slot.replace(/\d+$/, '');
export const fits = (slot: string, pos: string) => (FLEX[base(slot)] ?? [base(slot)]).includes(pos);
const isFlex = (slot: string) => base(slot) in FLEX;

type Pick = { id: string; pos: string; pts: number };

/** Best legal lineup by points: dedicated slots take their top scorer, then the flex slots are solved together (exhaustive; a roster is small). */
export function bestLineup(slots: string[], players: Pick[]): Array<string | null> {
  const pool = [...players].sort((a, b) => b.pts - a.pts);
  const out: Array<string | null> = slots.map(() => null);
  const taken = new Set<string>();
  slots.forEach((s, i) => {
    const p = isFlex(s) ? undefined : pool.find((q) => fits(s, q.pos) && !taken.has(q.id));
    if (p) {
      out[i] = p.id;
      taken.add(p.id);
    }
  });
  const flexIdx = slots.flatMap((s, i) => (isFlex(s) ? [i] : []));
  const rest = pool.filter((p) => !taken.has(p.id));
  const solve = (k: number, used: Set<string>): { total: number; picks: Array<string | null> } => {
    if (k === flexIdx.length) return { total: 0, picks: [] };
    const skip = solve(k + 1, used);
    let top = { total: skip.total, picks: [null, ...skip.picks] };
    for (const p of rest) {
      if (used.has(p.id) || !fits(slots[flexIdx[k]], p.pos)) continue;
      const r = solve(k + 1, new Set(used).add(p.id));
      if (p.pts + r.total > top.total) top = { total: p.pts + r.total, picks: [p.id, ...r.picks] };
    }
    return top;
  };
  solve(0, new Set()).picks.forEach((id, j) => (out[flexIdx[j]] = id));
  return out;
}
