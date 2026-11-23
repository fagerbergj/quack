import type { ComponentType, ReactNode } from 'react';
import { sentence } from '../../extension.ts';
import { Blurb, Markdown } from '../../Markdown.tsx';
import { NewsList } from './parts.tsx';
import { KINDS, SHAPES, metas, pts, record, unid, type Shape } from './shapes.tsx';
import type { Result } from './week.ts';

type P = { name: string; pos: string; team: string | null; inj?: string | null };
type Card = Record<string, any>;

const who = (p: P) => `${p.name} · ${p.pos}${p.team ? ` · ${p.team}` : ''}`;

const chip = 'rounded-md bg-soft px-1.5 py-0.5 text-[11px] font-medium text-ink-2';
const Chip = ({ children, tone = chip }: { children: ReactNode; tone?: string }) => <span className={tone}>{children}</span>;
const H = ({ children }: { children: ReactNode }) => <h3 className="m-0 mt-4 mb-1 text-[12px] font-semibold uppercase tracking-[0.06em] text-muted">{children}</h3>;
const Row = ({ children }: { children: ReactNode }) => <li className="flex min-w-0 flex-col gap-1 border-t border-line py-3 first:border-t-0">{children}</li>;
const List = ({ children }: { children: ReactNode }) => <ul className="m-0 flex list-none flex-col p-0">{children}</ul>;
const Figure = ({ n, label }: { n: string; label: string }) => <div className="min-w-0"><p className="m-0 text-[22px] font-semibold tabular-nums tracking-[-0.02em]">{n}</p><p className="m-0 text-[12px] text-muted">{label}</p></div>;
const Sum = ({ text }: { text: string }) => <Blurb text={text} whole />;

function Waivers({ c }: { c: Card }) {
  const also: any[] = c.also_checked ?? [];
  return (
    <>
      <Sum text={c.summary} />
      <p className="m-0 mt-2 text-[12px] text-muted">{`${sentence(c.waiver_type)} · priority ${c.my_priority} of ${c.teams}`}</p>
      <List>
        {c.candidates.map((x: any) => (
          <Row key={x.rank}>
            <div className="flex min-w-0 items-baseline gap-2">
              <span className="w-5 flex-none text-[12px] text-muted">{x.rank}</span>
              <span className="min-w-0 flex-1 break-words text-[14px] font-medium">{`+ ${x.player.name}`}</span>
              <span className="flex-none text-[14px] font-semibold tabular-nums">{x.proj.toFixed(1)}</span>
            </div>
            <p className="m-0 pl-7 text-[12px] text-muted">{`${x.player.pos} · ${x.player.team} · ${x.owned_pct}% owned · bye ${x.bye}`}</p>
            {x.drop && <p className="m-0 pl-7 text-[13px] text-ink-2">{`− ${x.drop.player.name}`}{x.drop.role === 'starter' && <> <Chip>Starter</Chip></>}{x.drop.player.inj && <> <Chip>{x.drop.player.inj}</Chip></>}</p>}
            <div className="pl-7"><Blurb text={x.why} /><NewsList items={x.news ?? []} /></div>
          </Row>
        ))}
      </List>
      {also.length > 0 && (
        <details className="text-[13px]">
          <summary className="flex min-h-11 cursor-pointer items-center text-ink-2">{`${also.length} more players checked`}</summary>
          <List>{also.map((a) => <Row key={a.player.id}><span className="text-[13px] font-medium">{who(a.player)}</span><Blurb text={a.why} /></Row>)}</List>
        </details>
      )}
    </>
  );
}

function Trades({ c }: { c: Card }) {
  const deals: any[] = c.suggestions ?? [];
  return (
    <>
      <Sum text={c.summary} />
      <List>
        {deals.map((d, i) => (
          <Row key={i}>
            <span className="break-words text-[14px] font-medium">{d.partner.team}</span>
            <p className="m-0 text-[13px] text-ink-2">{`Give ${d.give.name}, get ${d.get.name}`}</p>
            <Blurb text={d.note} />
            <NewsList items={d.news ?? []} />
          </Row>
        ))}
      </List>
    </>
  );
}

function Trends({ c }: { c: Card }) {
  const items: any[] = c.items;
  const kinds = [...new Set(items.map((i) => i.kind as string))];
  return (
    <>
      <Sum text={c.summary} />
      {kinds.map((k) => (
        <section key={k}>
          <H>{KINDS[k] ?? sentence(k)}</H>
          <List>
            {items.filter((i) => i.kind === k).map((i, n) => (
              <Row key={n}>
                {i.player && <span className="text-[14px] font-medium">{who(i.player)}</span>}
                <Blurb text={unid(i.text).replace(`${i.player?.name}: `, '')} />
                {i.link ? <a href={i.link} target="_blank" rel="noreferrer" className="text-[12px] text-link">{i.source}</a> : <span className="text-[12px] text-muted">{i.source}</span>}
              </Row>
            ))}
          </List>
        </section>
      ))}
      {c.source_note && <details className="text-[13px]"><summary className="flex min-h-11 cursor-pointer items-center text-ink-2">Source notes</summary><Blurb text={unid(c.source_note)} whole /></details>}
    </>
  );
}

function Score({ team, pts: n }: { team: string; pts: number }) {
  return <div className="flex min-w-0 items-baseline justify-between gap-2"><span className="min-w-0 break-words text-[13px]">{team.trim()}</span><span className="text-[14px] font-semibold tabular-nums">{n.toFixed(2)}</span></div>;
}

function Recap({ c }: { c: Card }) {
  const games: any[] = [...c.games].sort((a, b) => Number(b.mine) - Number(a.mine));
  return (
    <>
      <Sum text={c.summary} />
      <List>
        {games.map((g) => (
          <Row key={g.id}>
            <Score team={g.home.team} pts={g.home.pts} />
            <Score team={g.away.team} pts={g.away.pts} />
            <Blurb text={g.note} />
          </Row>
        ))}
      </List>
      <H>{`Standings · top ${c.playoff_line.standings.filter((s: any) => s.in_playoff_line).length} make it`}</H>
      <List>{c.playoff_line.standings.map((s: any) => <li key={s.rosterId} className="flex gap-2 py-1.5 text-[13px]"><span className="w-5 text-muted">{s.rank}</span><span className="min-w-0 flex-1 break-words">{s.team.trim()}</span><span className="tabular-nums text-ink-2">{record(s)}</span></li>)}</List>
      {c.source_note && <p className="m-0 mt-2 text-[12px] text-muted">{c.source_note}</p>}
    </>
  );
}

function Misses({ title, rows }: { title: string; rows: any[] }) {
  if (!rows.length) return null;
  return (
    <>
      <H>{title}</H>
      <List>
        {rows.map((m, i) => (
          <Row key={i}>
            <div className="flex min-w-0 items-baseline gap-2"><span className="flex-none font-semibold tabular-nums text-warning">{`+${pts(m.swing ?? m.pts - m.over_pts)}`}</span><span className="min-w-0 break-words text-[14px] font-medium">{`${(m.better ?? m).name} over ${(m.started ?? { name: m.over_name }).name}`}</span></div>
            <div><Chip>{m.knowable ? 'Knowable' : 'Not knowable'}</Chip></div>
            <Blurb text={m.why} />
          </Row>
        ))}
      </List>
    </>
  );
}

function Retro({ c }: { c: Card }) {
  if (c.bye) return <Sum text={c.summary ?? 'Bye week.'} />;
  return (
    <>
      <div className="grid grid-cols-3 gap-3"><Figure n={pts(c.started)} label={c.won ? 'scored · won' : 'scored · lost'} /><Figure n={pts(c.best)} label="best possible" /><Figure n={pts(c.left)} label="left on bench" /></div>
      {c.summary && <div className="mt-3"><Sum text={c.summary} /></div>}
      <Misses title="Lineup misses" rows={c.misses ?? []} />
      <Misses title="Waiver misses" rows={c.waiver_misses ?? []} />
      {c.lessons?.length > 0 && <><H>Lessons</H><List>{c.lessons.map((l: string) => <Row key={l}><Blurb text={l} whole /></Row>)}</List></>}
    </>
  );
}

function Season({ s }: { s: any }) {
  const draft: any[] = s.draft ?? [];
  const count = (v: string) => draft.filter((d) => d.verdict === v).length;
  const bits = [`${count('steal')} steals`, `${count('reach')} reaches`].filter((b) => !b.startsWith('0 '));
  const facts = [`PF rank ${s.pf_rank}`, `PA rank ${s.pa_rank}`, `Efficiency ${s.eff}%`, `Bench ${pts(s.left_total)}`, ...(bits.length ? [`Draft ${bits.join(' · ')}`] : [])];
  return (
    <Row>
      <div className="flex min-w-0 items-baseline gap-2"><span className="text-[15px] font-semibold">{s.season}</span><span className="tabular-nums text-ink-2">{record(s)}</span>{s.in_progress ? <Chip>In progress</Chip> : s.champion && <span className="min-w-0 break-words text-[12px] text-muted">{`Champion ${s.champion}`}</span>}</div>
      <p className="m-0 text-[12px] text-muted">{facts.join(' · ')}</p>
      {s.note && <Blurb text={s.note} />}
      {(s.cited ?? []).map((p: any) => <p key={p.pick} className="m-0 text-[12px] text-muted">{`Pick ${p.pick} ${p.player.name}: ${p.player.pos}${p.drafted_as} drafted, ${p.player.pos}${p.finished} finish, ${sentence(String(p.verdict).replace(/_/g, ' '))}`}</p>)}
    </Row>
  );
}

function History({ c }: { c: Card }) {
  return <><Sum text={c.summary} /><List>{c.seasons.map((s: any) => <Season key={s.season} s={s} />)}</List></>;
}

/** One Sleeper job: its `label` wherever the job itself is named, the `title` of a week's result (Week 9 recap), the line under that title in
 * a past week, its running line, its colour on the week strip, its closed card (Shape, meta beside the title, width) and its opened View. */
type Job = {
  label: string; title?: (week: number) => string; what: string; run: string; tone: string; View?: ComponentType<{ c: Card }>;
  Shape?: ComponentType<Shape>; meta?: (c: Card) => string | undefined; width?: (c: Card) => 'wide' | 'quiet' | undefined;
};

export const JOBS: Record<string, Job> = {
  lineup: { label: 'Lineup', what: 'Your starters and bench', run: 'Checking your lineup', tone: 'bg-link' },
  waivers: { label: 'Waivers', what: 'Claims for the week', run: 'Looking at waivers', tone: 'bg-warning', View: Waivers, Shape: SHAPES.waivers, meta: metas.waivers },
  'trade-finder': { label: 'Trade finder', what: 'Deals worth asking about', run: 'Looking for trades', tone: 'bg-success', View: Trades, Shape: SHAPES.trades, width: (c) => (c.suggestions?.length ? undefined : 'quiet') },
  trends: { label: 'Trends', what: 'What is moving in the league', run: 'Reading trends', tone: 'bg-muted', View: Trends, Shape: SHAPES.trends, meta: metas.trends },
  digest: { label: 'Recap', what: 'League recap', run: 'Writing the recap', tone: 'bg-muted', View: Recap, Shape: SHAPES.digest, meta: metas.digest, width: () => 'wide' },
  retro: { label: 'Retro', what: 'Your lineup calls, looked back on', run: 'Writing the retro', tone: 'bg-muted', View: Retro, Shape: SHAPES.retro },
  history: { label: 'League history', title: () => 'League history', what: 'Your seasons so far', run: 'Reading league history', tone: 'bg-info', View: History, Shape: SHAPES.history, meta: metas.history },
};
export const ORDER = Object.keys(JOBS);
// The lineup heads the current week on its own.
export const CURRENT = ORDER.filter((j) => j !== 'lineup');

const KEY = /^sleeper:([^:]+):(\d+):([a-z-]+)(?::|$)/;
/** A Sleeper session key, sleeper:<league>:<week>:<job>[:...]; undefined for any other key or job. */
export function parseKey(key: string): { league: string; week: number; job: string } | undefined {
  const m = KEY.exec(key);
  return m && Object.hasOwn(JOBS, m[3]!) ? { league: m[1]!, week: Number(m[2]), job: m[3]! } : undefined;
}

// A recap sealed before its week is played is a preview.
const preview = (job: string, card?: Card) => job === 'digest' && card?.past === false;

/** The name a job's result goes by everywhere: its session, its sealed card and the page. */
export function jobTitle(job: string, week: number, card?: Card): string {
  const j = JOBS[job];
  if (!j) return job;
  return j.title?.(week) ?? `Week ${week} ${(preview(job, card) ? 'Preview' : j.label).toLowerCase()}`;
}
export const titleOf = (r: Result) => jobTitle(r.job, r.week, r.card);

/** What a result is, for the line under its title in a past week. */
export const whatIs = (r: Result) => (preview(r.job, r.card) ? 'Preview of the week' : JOBS[r.job]?.what ?? '');

/** The opened view of a result; the lineup has its own. */
export function Full({ r }: { r: Result }) {
  const Body = JOBS[r.job]?.View;
  return Body ? <Body c={r.card} /> : <Markdown text={r.card.summary ?? ''} />;
}
