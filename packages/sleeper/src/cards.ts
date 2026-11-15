/** The seal-time shape of a sleeper.cards card: contracts:5f1e60fc-d8f3-463e-9460-4cc02763a40c v5. Objects stay open; `'key?'` marks an optional field. */
type Prim = 'string' | 'number' | 'integer' | 'boolean';
type Spec =
  | { t: 'prim'; of: Prim }
  | { t: 'enum'; of: readonly string[] }
  | { t: 'null'; of: Spec }
  | { t: 'arr'; of: Spec; max?: number }
  | { t: 'rec'; of: Spec }
  | { t: 'obj'; of: Record<string, Spec> }
  | { t: 'by'; of: Record<string, Spec> };

const P = (of: Prim): Spec => ({ t: 'prim', of });
const S = P('string'), N = P('number'), I = P('integer'), B = P('boolean');
const E = (of: readonly string[]): Spec => ({ t: 'enum', of });
const nul = (of: Spec): Spec => ({ t: 'null', of });
const A = (of: Spec, max?: number): Spec => ({ t: 'arr', of, max });
const O = (of: Record<string, Spec>): Spec => ({ t: 'obj', of });

const playerRef = O({ id: S, name: S, pos: S, team: nul(S), inj: nul(S) });
const item = { title: S, url: S, date: S, 'excerpt?': S, player: S };
const news = A(O(item));
const signal = nul(O(item));
const game = nul(O({ opponent: S, home: B, kickoff: nul(S), locked: B, status: nul(S) }));
const namePts = O({ name: S, pts: N });
const tRef = O({ id: S, name: S, pos: S, team: nul(S), inj: nul(S), proj: N, bye: nul(I) });
const figure = O({ round: I, pick: I, player: O({ id: S, name: S, pos: S }), drafted_as: I, finished: I, pts: N, verdict: E(['reach', 'steal', 'as_expected', 'unknown']) });
const side = O({ team: S, pts: N, top_scorers: A(namePts) });
const standing = O({ rank: I, rosterId: I, team: S, wins: I, losses: I, ties: I, fpts: N, fpts_against: N, in_playoff_line: B });
const season = O({
  season: S, league_id: S, in_progress: B, wins: I, losses: I, ties: I, pf: N, pa: N, pf_rank: I, pa_rank: I, eff: nul(N), left_total: N, close_losses: I, champion: nul(S),
  weeks: A(O({ week: I, result: nul(E(['W', 'L', 'T'])), started: N, best: N, left: N, opp: nul(N), close_loss: B })), draft: nul(A(figure)), note: nul(S), cited: A(figure),
});
const missed = { knowable: B, signal, before: S, why: S };

const KINDS: Record<string, Spec> = {
  lineup: O({
    league: S, week: I, team: S, opponent: nul(S), my_proj: N, opp_proj: nul(N), summary: S,
    starters: A(O({ slot: S, player: playerRef, proj: N, action: E(['start', 'keep']), locked: B, replaces: nul(playerRef), game, why: S, news })),
    bench: A(O({ player: playerRef, proj: N, game })),
  }),
  waivers: O({
    league: S, week: I, team: S, waiver_type: E(['rolling', 'reverse_standings', 'faab']), my_priority: nul(I), faab_left: nul(N), teams: I, summary: S,
    candidates: A(O({
      rank: I, player: playerRef, proj: N, owned_pct: nul(N), 'adds_24h?': N, bye: nul(I), drop: O({ player: playerRef, role: E(['starter', 'bench', 'ir']) }),
      size: { t: 'by', of: { faab: O({ bid: I, left: N, pct: I }), priority: O({ position: nul(I), of: I }) } }, why: S, news,
    })),
    also_checked: A(O({ player: playerRef, why: S })),
  }),
  digest: O({
    league: S, week: I, past: B, summary: S, source_note: S,
    games: A(O({ id: I, home: side, away: side, margin: N, mine: B, note: S, news })),
    playoff_line: O({ teams: I, remaining_weeks: I, standings: A(standing) }),
  }),
  retro: O({
    league: S, week: I, team: S, opponent: nul(S), bye: B, started: N, opp: nul(N), won: B, best: N, left: N, summary: S, lessons: A(S, 3),
    misses: A(O({ slot: S, started: namePts, better: namePts, swing: N, ...missed })),
    waiver_misses: A(O({ name: S, pos: S, pts: N, over_name: S, over_pts: N, ...missed })),
  }),
  'trade-finder': O({
    league: S, week: I, team: S, summary: S,
    suggestions: A(O({
      rank: I, partner: O({ rosterId: I, team: S, record: S, stance: E(['contender', 'rebuilder']), needs: { t: 'rec', of: N } }),
      partner_need: S, give: tRef, get: tRef, delta: O({ you: N, partner: N }), note: S, news,
    }), 3),
  }),
  trends: O({
    league: S, week: I, date: S, since: nul(S), summary: S, source_note: S,
    items: A(O({
      time: S, source: S, kind: E(['injury', 'waiver', 'news', 'note']), text: S, 'link?': S,
      'player?': O({ id: S, name: S, pos: nul(S), team: nul(S), inj: nul(S) }),
    })),
  }),
  history: O({
    league: S, week: I, team: S, summary: S, seasons: A(season),
    definitions: O({ reach_steal: S, efficiency: S, close_loss: S, left: S }),
  }),
};

const typeName = (v: unknown) => (v === undefined ? 'nothing' : v === null ? 'null' : Array.isArray(v) ? 'an array' : typeof v === 'object' ? 'an object' : `${typeof v} ${JSON.stringify(v)}`);
const isObject = (v: unknown): v is Record<string, unknown> => !!v && typeof v === 'object' && !Array.isArray(v);

function expectedOf(s: Spec): string {
  if (s.t === 'prim') return s.of === 'integer' ? 'an integer' : `a ${s.of}`;
  if (s.t === 'enum') return `one of ${s.of.join(', ')}`;
  return s.t === 'null' ? `${expectedOf(s.of)} or null` : s.t === 'arr' ? 'an array' : 'an object';
}

function primOk(of: Prim, v: unknown): boolean {
  return of === 'integer' ? Number.isInteger(v) : of === 'number' ? typeof v === 'number' && Number.isFinite(v) : typeof v === of;
}

function fields(of: Record<string, Spec>, v: Record<string, unknown>, at: string, out: string[]): void {
  for (const [key, sub] of Object.entries(of)) {
    const name = key.replace(/\?$/, '');
    if (v[name] !== undefined) walk(sub, v[name], `${at}.${name}`, out);
    else if (!key.endsWith('?')) out.push(`${at}.${name} is required (expected ${expectedOf(sub)})`);
  }
}

function items(s: Extract<Spec, { t: 'arr' }>, v: unknown, at: string, out: string[]): void {
  if (!Array.isArray(v)) return void out.push(`${at}: expected an array, got ${typeName(v)}`);
  if (s.max !== undefined && v.length > s.max) out.push(`${at}: expected at most ${s.max} items, got ${v.length}`);
  v.forEach((x, i) => walk(s.of, x, `${at}[${i}]`, out));
}

function walk(s: Spec, v: unknown, at: string, out: string[]): void {
  if (s.t === 'null') return v === null ? undefined : walk(s.of, v, at, out);
  if (s.t === 'prim' || s.t === 'enum') {
    if (!(s.t === 'prim' ? primOk(s.of, v) : s.of.includes(v as string))) out.push(`${at}: expected ${expectedOf(s)}, got ${typeName(v)}`);
  } else if (s.t === 'arr') items(s, v, at, out);
  else if (!isObject(v)) out.push(`${at}: expected an object, got ${typeName(v)}`);
  else if (s.t === 'obj') fields(s.of, v, at, out);
  else if (s.t === 'rec') for (const [k, x] of Object.entries(v)) walk(s.of, x, `${at}.${k}`, out);
  else if (typeof v.kind === 'string' && s.of[v.kind]) walk(s.of[v.kind], v, at, out);
  else out.push(`${at}.kind: expected one of ${Object.keys(s.of).join(', ')}, got ${typeName(v.kind)}`);
}

/** What is wrong with a card, each as "path: what was expected"; empty when it matches the contract. */
export function cardProblems(card: unknown): string[] {
  if (!isObject(card)) return [`card: expected an object, got ${typeName(card)}`];
  const kind = card.kind;
  const spec = typeof kind === 'string' ? KINDS[kind] : undefined;
  if (!spec) return [`card.kind: expected one of ${Object.keys(KINDS).join(', ')}, got ${typeName(kind)}`];
  const out: string[] = [];
  walk(spec, card, 'card', out);
  return out;
}
