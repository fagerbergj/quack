import type { ReactNode } from 'react';
import { Blurb } from '../../Markdown.tsx';
import { weekday } from './parts.tsx';

type Card = Record<string, any>;
/** A job's closed card: its own shape, drawn from the card fields alone. `team` is the user's team, when the page knows it. */
export type Shape = { c: Card; team?: string };

export const pts = (n: number) => String(Math.round(n * 100) / 100);
export const record = (s: Card) => `${s.wins}-${s.losses}${s.ties ? `-${s.ties}` : ''}`;
// Trend text carries Sleeper player ids ("4881 Lamar Jackson: ..."), which mean nothing to a reader.
export const unid = (t: string) => t.replace(/\b(?!(?:19|20)\d{2}(?!\d))\d{4,5}(?:\/\d{4,5})*:?\s+(?=[A-Z])/g, '').replace(/\b\d{4,5}(?:\/\d{4,5})+:?\s*/g, '');
export const KINDS: Record<string, string> = { injury: 'Injuries', waiver: 'Rising on waivers', note: 'Notes' };
const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? '' : 's'}`;
const same = (a: unknown, b: unknown) => typeof a === 'string' && typeof b === 'string' && a.trim() === b.trim();

const Label = ({ children }: { children: ReactNode }) => <p className="m-0 text-[11.5px] font-semibold uppercase tracking-[0.06em] text-muted">{children}</p>;
const Pos = ({ children }: { children: ReactNode }) => <span className="ml-1.5 rounded-[5px] bg-surface px-1 py-px align-[1px] font-mono text-[10.5px] font-semibold text-muted">{children}</span>;
const Bar = ({ pct, tone }: { pct: number; tone: string }) => (
  <span aria-hidden="true" className="block h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-line"><span className={`block h-full rounded-full ${tone}`} style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} /></span>
);
const Swatch = ({ tone }: { tone: string }) => <span aria-hidden="true" className={`mr-1.5 inline-block h-2.5 w-2.5 rounded-[3px] ${tone}`} />;
const big = 'font-display font-semibold tabular-nums tracking-[-0.02em]';

/** The header line beside a closed card's title. */
export const metas: Record<string, (c: Card) => string | undefined> = {
  waivers: (c) => (c.my_priority == null ? undefined : `Priority ${c.my_priority} of ${c.teams}`),
  // A bare date is midday UTC so it names the same day in the league zone.
  trends: (c) => (c.since ? `Since ${weekday(Date.parse(`${c.since}T12:00:00Z`))}` : undefined),
  digest: (c) => (c.playoff_line?.remaining_weeks == null ? undefined : `${plural(c.playoff_line.remaining_weeks, 'week')} to the playoffs`),
  history: (c) => (c.seasons ? plural(c.seasons.length, 'season') : undefined),
};

function Claim({ x }: { x: Card }) {
  const drop = x.drop;
  return (
    <div className="grid grid-cols-[1.75rem_minmax(0,1fr)_auto] items-center gap-x-2.5 gap-y-1 rounded-2xl bg-soft p-3">
      <span className="row-span-2 grid h-7 w-7 place-items-center rounded-full bg-warning-bg font-display font-bold text-warning"><span className="sr-only">Rank </span>{x.rank}</span>
      <span className="min-w-0 break-words text-[15px] font-semibold">{`+ ${x.player.name}`}<Pos>{x.player.pos}</Pos></span>
      <span className={`row-span-2 text-right text-[18px] ${big}`}>{Number(x.proj).toFixed(1)}<small className="block text-[10.5px] font-medium text-muted">proj</small></span>
      <span className="min-w-0 break-words text-[13px] text-danger">{drop && <>{`− ${drop.player.name} `}<span className="text-muted">{[drop.player.pos, drop.role === 'ir' ? 'IR' : drop.role].filter(Boolean).join(' · ')}</span></>}</span>
      {x.owned_pct != null && <span className="col-start-2 col-end-4 flex items-center gap-2 text-[11.5px] text-muted">{`Rostered in ${Math.round(x.owned_pct)}% of leagues`}<Bar pct={x.owned_pct} tone="bg-muted" /></span>}
    </div>
  );
}

function Waivers({ c }: Shape) {
  const top = c.candidates?.[0];
  const also = c.also_checked?.length ?? 0;
  return (
    <>
      {top && <Claim x={top} />}
      {c.summary && <Blurb text={c.summary} />}
      {also > 0 && <p className="m-0 text-[12.5px] text-muted">{`${also} more players checked`}</p>}
    </>
  );
}

const DOWN = /^(out|ir|doubtful|pup|sus)/i;
const dirs = { down: ['↓', 'bg-danger-bg text-danger'], up: ['↑', 'bg-success-bg text-success'], flat: ['?', 'bg-soft text-muted'] } as const;
const dirOf = (i: Card) => (i.kind === 'waiver' ? dirs.up : DOWN.test(i.player?.inj ?? '') ? dirs.down : dirs.flat);

function Chips({ items }: { items: Card[] }) {
  return (
    <ul className="m-0 flex list-none flex-wrap gap-1.5 p-0">
      {items.map((i, n) => {
        const [glyph, tone] = dirOf(i);
        return (
          <li key={n} className="inline-flex min-w-0 items-center gap-1.5 rounded-full border border-line bg-surface py-1 pr-2.5 pl-1.5 text-[13px]">
            <span aria-hidden="true" className={`grid h-[18px] w-[18px] flex-none place-items-center rounded-full text-[11px] font-bold ${tone}`}>{glyph}</span>
            <span className="min-w-0 break-words">{i.player.name}<Pos>{i.player.pos}</Pos></span>
            {i.player.inj && <span className="text-[12px] text-muted">{i.player.inj}</span>}
          </li>
        );
      })}
    </ul>
  );
}

// "563K in 24h", "434K adds", "134,316 adds"; a waiver item without one leaves its group as chips, never a guessed bar.
const ADDS = /(\d[\d,]*(?:\.\d+)?)\s*([km])?\s+(?:adds\b|in 24 ?h\b)/i;
const SCALE: Record<string, number> = { k: 1e3, m: 1e6 };
export function addsOf(text: string): number | undefined {
  const m = ADDS.exec(unid(text));
  return m ? Number(m[1]!.replace(/,/g, '')) * (SCALE[m[2]?.toLowerCase() ?? ''] ?? 1) : undefined;
}
const short = (n: number) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${Math.round(n / 1e3)}k` : String(n));

function Adds({ rows }: { rows: Array<{ i: Card; n: number }> }) {
  const top = Math.max(...rows.map((r) => r.n));
  return (
    <ul className="m-0 flex list-none flex-col gap-1.5 p-0 text-[13px]">
      {rows.map(({ i, n }) => (
        <li key={i.player.id ?? i.player.name} className="grid grid-cols-[minmax(0,9.5em)_minmax(0,1fr)_3.2em] items-center gap-2.5">
          <span className="truncate">{i.player.name} <span className="text-muted">{i.player.pos}</span></span>
          <Bar pct={(n / top) * 100} tone="bg-success" />
          <span className="text-right text-[12px] tabular-nums text-muted">{short(n)}<span className="sr-only"> adds</span></span>
        </li>
      ))}
    </ul>
  );
}

function TrendGroup({ kind, items }: { kind: string; items: Card[] }) {
  const rows = items.map((i) => ({ i, n: addsOf(i.text ?? '') }));
  const chart = kind === 'waiver' && rows.every((r) => r.n !== undefined);
  return (
    <section className="flex flex-col gap-1.5">
      <Label>{chart ? `${KINDS.waiver} · adds` : (KINDS[kind] ?? kind)}</Label>
      {chart ? <Adds rows={rows as Array<{ i: Card; n: number }>} /> : <Chips items={items} />}
    </section>
  );
}

function Trends({ c }: Shape) {
  const items: Card[] = (c.items ?? []).filter((i: Card) => i.player?.name);
  const kinds = [...new Set(items.map((i) => String(i.kind)))];
  if (!kinds.length) return c.summary ? <Blurb text={c.summary} /> : null;
  return <>{kinds.map((k) => <TrendGroup key={k} kind={k} items={items.filter((i) => i.kind === k)} />)}</>;
}

type Side = { team: string; pts: number };
const fixed = (n: number) => Number(n).toFixed(2);

function Team({ s, lead, proj, right }: { s: Side; lead: boolean; proj: boolean; right?: boolean }) {
  return (
    <div className={`min-w-0 ${right ? 'text-right' : ''}`}>
      <p className="m-0 break-words text-[13px] text-ink-2">{s.team.trim()}</p>
      <p className={`m-0 text-[26px] @3xl:text-[30px] ${big} ${lead ? 'text-accent-ink' : ''}`}>{fixed(s.pts)}</p>
      {proj && <p className="m-0 text-[11px] text-muted">projected</p>}
    </div>
  );
}

function Scoreline({ g, team, proj }: { g: Card; team?: string; proj: boolean }) {
  const [me, them] = (same(g.away.team, team) ? [g.away, g.home] : [g.home, g.away]) as [Side, Side];
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] items-end gap-2.5 rounded-2xl bg-soft p-3.5">
      <Team s={me} lead={me.pts >= them.pts} proj={proj} />
      <span className="pb-2 text-[12px] text-muted">vs</span>
      <Team s={them} lead={them.pts > me.pts} proj={proj} right />
    </div>
  );
}

function Games({ games, proj }: { games: Card[]; proj: boolean }) {
  return (
    <section className="flex flex-col gap-1.5">
      <Label>{proj ? 'Other games · projected' : 'Other games'}</Label>
      <ul className="m-0 flex list-none flex-col gap-1.5 p-0 text-[12.5px]">
        {games.map((g) => {
          const [a, b] = (g.home.pts >= g.away.pts ? [g.home, g.away] : [g.away, g.home]) as [Side, Side];
          return (
            <li key={g.id} className="grid grid-cols-[minmax(0,1fr)_auto_auto_minmax(0,1fr)] items-center gap-2 whitespace-nowrap">
              <span className="truncate text-right font-semibold">{a.team.trim()}</span>
              <span className="text-center font-semibold tabular-nums">{fixed(a.pts)}</span>
              <span className="text-center tabular-nums">{fixed(b.pts)}</span>
              <span className="truncate text-muted">{b.team.trim()}</span>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

const cut = "flex items-center gap-2 py-0.5 text-[10.5px] font-semibold uppercase tracking-[0.06em] text-warning before:flex-1 before:border-t-[1.5px] before:border-dashed before:border-warning before:content-[''] after:flex-1 after:border-t-[1.5px] after:border-dashed after:border-warning after:content-['']";

// The line falls after the last team inside it, and only when someone sits below.
function Ladder({ rows, team }: { rows: Card[]; team?: string }) {
  const last = rows.findLastIndex((s) => s.in_playoff_line);
  return (
    <ol aria-label="Standings" className="m-0 flex list-none flex-col p-0 text-[12.5px]">
      {rows.flatMap((s, i) => [
        <li key={s.rosterId} aria-current={same(s.team, team) || undefined} className="grid grid-cols-[1.4em_minmax(0,1fr)_auto] gap-2 rounded-[7px] px-2 py-[3px] aria-[current=true]:bg-link-bg aria-[current=true]:font-semibold aria-[current=true]:text-link">
          <span className="tabular-nums text-muted">{s.rank}</span><span className="min-w-0 break-words">{s.team.trim()}</span><span className="tabular-nums text-ink-2">{record(s)}</span>
        </li>,
        ...(i === last && i < rows.length - 1 ? [<li key="cut" role="separator" aria-label="Playoff line" className={cut}>Playoff line</li>] : []),
      ])}
    </ol>
  );
}

// A preview's numbers are projections and say so; a recap (past) is the same shape with final scores.
function Digest({ c, team }: Shape) {
  const games: Card[] = c.games ?? [];
  const mine = games.find((g) => g.mine);
  const proj = c.past === false;
  const others = games.filter((g) => g !== mine);
  return (
    <div className="grid gap-4 @3xl:grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)]">
      <div className="flex min-w-0 flex-col gap-3">
        {mine && <Scoreline g={mine} team={team} proj={proj} />}
        {others.length > 0 && <Games games={others} proj={proj} />}
      </div>
      <Ladder rows={c.playoff_line?.standings ?? []} team={team} />
    </div>
  );
}

type Swing = { d: number; text: string; why: string };
const known = (m: Card) => (m.knowable ? 'knowable' : 'not knowable');
const swings = (c: Card): Swing[] => [
  ...(c.misses ?? []).map((m: Card) => ({ d: m.swing, text: `${m.better?.name} over ${m.started?.name}`, why: `lineup · ${known(m)}` })),
  ...(c.waiver_misses ?? []).map((m: Card) => ({ d: m.pts - m.over_pts, text: `${m.name} over ${m.over_name}`, why: `waiver · ${known(m)}` })),
].sort((a, b) => b.d - a.d).slice(0, 3);

function Swings({ c }: Shape) {
  const top = swings(c);
  if (!top.length) return null;
  return (
    <section className="flex flex-col gap-1">
      <Label>Biggest swings</Label>
      <ul className="m-0 flex list-none flex-col gap-1 p-0 text-[13px]">
        {top.map((s) => (
          <li key={s.text} className="flex items-baseline gap-2">
            <span className="w-[3.4em] flex-none font-semibold tabular-nums text-warning">{`+${pts(s.d)}`}</span>
            <span className="min-w-0 break-words">{s.text}</span>
            <span className="ml-auto hidden flex-none text-[12px] text-muted @3xl:inline">{s.why}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}

const bench = 'bg-[repeating-linear-gradient(135deg,var(--color-mark)_0_4px,transparent_4px_8px)]';

function Retro({ c }: Shape) {
  if (c.bye) return <Blurb text={c.summary ?? 'Bye week.'} />;
  const pct = (n: number) => `${c.best > 0 ? (n / c.best) * 100 : 0}%`;
  return (
    <>
      <p className="m-0 flex flex-wrap items-baseline gap-x-2.5">
        <span className={`rounded-md px-1.5 py-0.5 text-[11px] font-bold tracking-[0.08em] ${c.won ? 'bg-success-bg text-success' : 'bg-danger-bg text-danger'}`}>{c.won ? 'WON' : 'LOST'}</span>
        <span className={`text-[26px] ${big}`}>{pts(c.started)}</span>
        <span className="text-[13px] text-muted">{`to ${pts(c.opp)}${c.opponent ? ` vs ${c.opponent}` : ''}`}</span>
      </p>
      <div role="img" aria-label={`Scored ${pts(c.started)} of a best possible ${pts(c.best)}; ${pts(c.left)} left on the bench`} className="flex h-3.5 overflow-hidden rounded-full bg-line">
        <span className="bg-accent" style={{ width: pct(c.started) }} /><span className={`bg-mark/40 ${bench}`} style={{ width: pct(c.left) }} />
      </div>
      <p aria-hidden="true" className="m-0 flex flex-wrap justify-between gap-x-3 gap-y-1 text-[12px] tabular-nums text-muted">
        <span><Swatch tone="bg-accent" />Scored <b className="font-semibold text-ink">{pts(c.started)}</b></span>
        <span><Swatch tone="bg-mark" />Bench <b className="font-semibold text-ink">{pts(c.left)}</b></span>
        <span>Best <b className="font-semibold text-ink">{pts(c.best)}</b></span>
      </p>
      <Swings c={c} />
    </>
  );
}

const RESULT: Record<string, string> = { W: 'bg-success', L: 'bg-danger/50', T: 'bg-muted' };

function Season({ s }: { s: Card }) {
  const weeks: Card[] = s.weeks ?? [];
  const cells = Array.from({ length: Math.max(14, weeks.length) }, (_, i) => weeks[i]?.result as string | undefined);
  return (
    <li className="grid grid-cols-[auto_3.2em_minmax(0,1fr)_3.4em] items-center gap-2.5 whitespace-nowrap text-[12.5px]">
      <span className={`font-semibold ${s.in_progress ? 'text-accent-ink' : ''}`}>{s.season}</span>
      <span className="tabular-nums text-ink-2">{record(s)}</span>
      <span role="img" aria-label={`${s.season} week by week: ${weeks.map((w) => w.result).join(' ') || 'no games'}`} className="grid gap-[3px]" style={{ gridTemplateColumns: `repeat(${cells.length}, minmax(0, 1fr))` }}>
        {cells.map((r, i) => <span key={i} className={`h-3.5 rounded-[3px] ${RESULT[r ?? ''] ?? 'bg-line'}`} />)}
      </span>
      <span className="text-right tabular-nums text-muted">{s.eff == null ? '' : `${s.eff}%`}</span>
    </li>
  );
}

function History({ c }: Shape) {
  return (
    <>
      <ul className="m-0 flex list-none flex-col gap-1.5 p-0">{(c.seasons ?? []).map((s: Card) => <Season key={s.season} s={s} />)}</ul>
      <p aria-hidden="true" className="m-0 flex flex-wrap justify-between gap-x-3 text-[12px] text-muted">
        <span><Swatch tone="bg-success" />Win <Swatch tone="ml-2 bg-danger/50" />Loss</span>
        <span>Lineup efficiency</span>
      </p>
    </>
  );
}

function Trades({ c }: Shape) {
  return (
    <ul className="m-0 flex list-none flex-col gap-1.5 p-0 text-[13px]">
      {(c.suggestions ?? []).map((d: Card, i: number) => (
        <li key={i} className="flex min-w-0 flex-wrap items-baseline gap-x-2">
          <span className="font-medium">{d.partner?.team}</span>
          <span className="text-ink-2"><span className="sr-only">Give </span>{d.give?.name} <span aria-hidden="true">⇄</span><span className="sr-only">, get</span> {d.get?.name}</span>
        </li>
      ))}
    </ul>
  );
}

export const SHAPES = { waivers: Waivers, trends: Trends, digest: Digest, retro: Retro, history: History, trades: Trades };
