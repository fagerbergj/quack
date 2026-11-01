import { useCallback, useEffect, useState } from 'react';
import { StatusIcon } from './Artifacts.tsx';
import { listArtifacts, runLineup, sleeperLeague, type SealedArtifact } from './api.ts';
import type { Row } from './sessions.ts';

// The slice of @quack/sleeper's lineup card the page reads; the card file is the contract.
type Player = { id: string; name: string; pos: string; team: string | null; inj: string | null };
type Game = { opponent: string; home: boolean; kickoff: string | null; locked: boolean; status: string | null };
type Starter = { slot: string; player: Player; proj: number; action: 'start' | 'keep'; locked: boolean; replaces: Player | null; game: Game | null; why: string; news?: Array<{ title: string; url: string; date: string; player: string }> };
export type Card = { kind: 'lineup'; league: string; week: number; team: string; opponent: string | null; my_proj: number; opp_proj: number | null; summary: string; starters: Starter[]; bench: Array<{ player: Player; proj: number }> };

const LINEUP = /^sleeper:([^:]+):(\d+):lineup$/;
const ET = 'America/New_York';
const when = (iso: string | number) => new Intl.DateTimeFormat('en-US', { timeZone: ET, weekday: 'short', hour: 'numeric', minute: '2-digit', timeZoneName: 'short' }).format(new Date(iso));

function parseCard(a: SealedArtifact | undefined): Card | undefined {
  try {
    const c = a && JSON.parse(a.content);
    return c?.kind === 'lineup' && Array.isArray(c.starters) ? c : undefined;
  } catch {
    return undefined;
  }
}

/** The newest week's lineup session for this league, and its latest sealed card. */
function useLineup(league: string | undefined, rows: Row[] | undefined, refresh: number) {
  const weekOf = (key: string) => Number(LINEUP.exec(key)?.[2] ?? -1);
  // No league yet means no lineup session: a non-matching key must not pass for league `undefined`.
  const session = league ? rows?.filter((r) => LINEUP.exec(r.key)?.[1] === league).sort((a, b) => weekOf(b.key) - weekOf(a.key))[0] : undefined;
  const [artifact, setArtifact] = useState<SealedArtifact>();
  const [error, setError] = useState('');
  const key = session?.key;
  useEffect(() => {
    if (!key) return;
    let live = true;
    listArtifacts(key).then((all) => live && (setArtifact(all.filter((a) => a.kind === 'sleeper.cards').sort((a, b) => b.version - a.version)[0]), setError('')), (e: Error) => live && setError(e.message));
    return () => void (live = false);
  }, [key, session?.running, refresh]);
  return { session, artifact, card: parseCard(artifact), error };
}

const verdictOf = (a: SealedArtifact) => (!a.verdict ? 'not checked' : a.verdict.passed ? 'checked' : 'checks failed');

const label = (id: string) => id.replace(/_/g, ' ').replace(/^./, (c) => c.toUpperCase());
const missed = (a: SealedArtifact) => (a.verdict?.checks ?? []).filter((c) => c.met !== null && c.met < 1);

// `brief` is the strip's one-word status; the card carries the full verdict.
export function Verdict({ artifact, brief }: { artifact: SealedArtifact; brief?: boolean }) {
  const v = artifact.verdict;
  const failed = brief ? [] : missed(artifact);
  return (
    <div className="flex min-w-0 flex-col gap-1.5 text-[12px]">
      <div className="flex items-center gap-2">
        <StatusIcon verdict={v} />
        <span className="font-medium text-ink-2">{brief ? verdictOf(artifact) : `v${artifact.version} · ${verdictOf(artifact)}`}</span>
      </div>
      {!brief && v && !v.passed && failed.length === 0 && <p className="m-0 font-mono text-ink-2">{`Judge: ${v.checks.filter((c) => (c.met ?? 0) >= 1).length} of ${v.checks.filter((c) => c.met !== null).length} checks passed`}</p>}
      {failed.map((c) => (
        <details key={`${c.criterion}.${c.id}`} className="rounded-lg border border-danger-line bg-danger-bg/60 px-3 py-1.5">
          <summary title={`${c.criterion}.${c.id}`} className="flex min-h-11 cursor-pointer items-center break-words font-medium text-danger lg:min-h-6">{label(c.id)}</summary>
          {c.notes.length ? c.notes.map((n, i) => <p key={i} className="m-0 mt-1 break-words text-ink-2">{n}</p>) : <p className="m-0 mt-1 text-muted">No note from the judge.</p>}
        </details>
      ))}
    </div>
  );
}

const NO_CHANGES = 'No changes: your lineup stands';
const unchanged = (card: Card) => card.starters.every((s) => s.action === 'keep');

// Locks at the earliest kickoff among slots still open; null when every game has started.
const lockTime = (card: Card | undefined) => {
  const kicks = (card?.starters ?? []).filter((s) => !s.locked && s.game?.kickoff).map((s) => Date.parse(s.game!.kickoff!));
  return kicks.length ? Math.min(...kicks) : undefined;
};

function ThisWeek({ card, artifact, running, onRun, status }: { card?: Card; artifact?: SealedArtifact; running: boolean; onRun: () => void; status: string }) {
  const lock = lockTime(card);
  return (
    <section aria-labelledby="todo" className="mt-5 rounded-[22px] border border-line bg-surface p-4 shadow-card">
      <h2 id="todo" className="m-0 mb-3 text-[16px]">This week</h2>
      <div className="flex flex-col gap-2 rounded-xl bg-warm/70 p-3.5">
        <p className="m-0 text-[11px] font-semibold uppercase tracking-[0.06em] text-warning">{`Lineup${card ? ` · week ${card.week}` : ''}${lock ? ` · locks ${when(lock)}` : ''}`}</p>
        <p className="m-0 text-[14px] font-medium text-ink">{running ? 'Running now' : card ? (unchanged(card) ? NO_CHANGES : card.summary) : 'No lineup yet this week'}</p>
        {artifact && <Verdict artifact={artifact} brief />}
        <div className="flex flex-wrap items-center gap-2">
          <button type="button" onClick={onRun} disabled={running} className="min-h-11 cursor-pointer rounded-lg bg-ink px-3 text-xs font-semibold text-on-ink disabled:cursor-default disabled:opacity-50 lg:min-h-0 lg:py-1">Run now</button>
          {status && <span role="status" className="text-[12px] text-ink-2">{status}</span>}
        </div>
      </div>
    </section>
  );
}

const host = (url: string) => new URL(url, 'http://x').hostname.replace(/^www\./, '');

// Only this row's players: the starter, and the one replaced under their own name.
function News({ s }: { s: Starter }) {
  const items = s.news ?? [];
  if (!items.length) return null;
  const names = [...new Set(items.map((n) => n.player))];
  return (
    <details className="rounded-lg border border-line bg-surface px-3 py-1.5 text-[12px]">
      <summary className="flex min-h-11 cursor-pointer list-none items-center font-medium text-ink-2 lg:min-h-6 [&::-webkit-details-marker]:hidden">{`News (${items.length})`}</summary>
      {names.map((name) => (
        <div key={name} className="mt-1 flex flex-col">
          {names.length > 1 && <p className="m-0 font-semibold text-ink-2">{name}</p>}
          {items.filter((n) => n.player === name).map((n) => (
            <a key={n.url} href={n.url} target="_blank" rel="noreferrer" className="flex min-h-11 min-w-0 flex-col justify-center break-words lg:min-h-0 lg:py-1">
              {n.title}
              <span className="text-muted">{`${host(n.url)} · ${n.date.slice(0, 10)}`}</span>
            </a>
          ))}
        </div>
      ))}
    </details>
  );
}

function StarterRow({ s }: { s: Starter }) {
  const change = s.action === 'start';
  const g = s.game;
  return (
    <li className={`flex flex-col gap-1 px-4 py-3 ${change ? 'bg-success-bg/60' : ''}`}>
      <div className="flex min-w-0 items-baseline gap-2">
        <span className={`w-10 flex-none font-mono text-[11.5px] ${change ? 'text-success' : 'text-muted'}`}>{s.slot}</span>
        <span className="min-w-0 flex-1 break-words text-[14px] font-medium">
          {s.player.name}
          {change && <span className="ml-1.5 rounded-md bg-success-bg px-1.5 py-0.5 text-[10px] font-bold text-success">START</span>}
          {s.player.inj && <span className="ml-1.5 rounded-md bg-warning-bg px-1.5 py-0.5 text-[10px] font-bold text-warning">{s.player.inj}</span>}
          {s.locked && <span className="ml-1.5 rounded-md bg-soft px-1.5 py-0.5 text-[10px] font-bold text-muted">LOCKED</span>}
        </span>
        <span className="flex-none text-[14px] font-semibold tabular-nums">{s.proj.toFixed(1)}</span>
      </div>
      <div className="flex flex-col gap-1 pl-12">
        <p className="m-0 text-[12px] text-muted">
          {`${s.player.pos} · ${s.player.team ?? 'FA'}`}
          {g && ` · ${g.home ? 'vs' : '@'} ${g.opponent}${g.kickoff ? ` · ${when(g.kickoff)}` : ''}`}
          {s.replaces && ` · replaces ${s.replaces.name}`}
        </p>
        <p className="m-0 break-words text-[13px] text-ink-2">{s.why}</p>
        <News s={s} />
      </div>
    </li>
  );
}

export function LineupCard({ card, artifact, sessionKey }: { card: Card; artifact: SealedArtifact; sessionKey: string }) {
  return (
    <article aria-label={`Week ${card.week} lineup`} className="mt-4 overflow-hidden rounded-[22px] border border-line bg-surface shadow-card">
      <div className="flex flex-col gap-2 border-b border-line px-4 py-3">
        <div className="flex items-center gap-2">
          <p className="m-0 text-sm font-semibold">Start / sit</p>
          <a href={`#/s/${encodeURIComponent(sessionKey)}`} className="ml-auto inline-flex min-h-11 items-center rounded-lg px-2 text-xs font-medium text-link no-underline hover:bg-soft lg:min-h-0 lg:py-1">Ask about this card</a>
        </div>
        <Verdict artifact={artifact} />
      </div>
      <div className="grid grid-cols-[1fr_auto_1fr] items-center gap-3 border-b border-line px-4 py-4">
        <div className="min-w-0"><p className="m-0 truncate text-[13px] text-muted">{card.team}</p><p className="m-0 text-[24px] font-semibold tabular-nums tracking-[-0.02em]">{card.my_proj.toFixed(1)}</p></div>
        <span className="text-[12px] text-muted">vs</span>
        <div className="min-w-0 text-right"><p className="m-0 truncate text-[13px] text-muted">{card.opponent ?? 'No opponent'}</p><p className="m-0 text-[24px] font-semibold tabular-nums tracking-[-0.02em]">{card.opp_proj?.toFixed(1) ?? '–'}</p></div>
      </div>
      {unchanged(card)
        ? <p className="m-0 border-b border-line bg-soft px-4 py-2.5 text-[13px] text-ink-2">{NO_CHANGES}</p>
        : <p className="m-0 border-b border-line bg-warm px-4 py-2.5 text-[13px] text-ink-2 break-words">{card.summary}</p>}
      <ul className="m-0 flex list-none flex-col divide-y divide-line p-0">{card.starters.map((s) => <StarterRow key={s.slot} s={s} />)}</ul>
      <details className="border-t border-line px-4 py-3 text-[13px]">
        <summary className="flex min-h-11 cursor-pointer items-center text-ink-2 lg:min-h-6">{`Bench · ${card.bench.length}`}</summary>
        <ul className="m-0 mt-2 flex list-none flex-col gap-1 p-0">
          {card.bench.map((b) => <li key={b.player.id} className="flex gap-2"><span className="min-w-0 flex-1 break-words">{`${b.player.name} · ${b.player.pos}`}</span><span className="tabular-nums text-ink-2">{b.proj.toFixed(1)}</span></li>)}
        </ul>
      </details>
    </article>
  );
}

export function Sleeper({ rows }: { rows: Row[] | undefined }) {
  const [league, setLeague] = useState<string>();
  const [unconfigured, setUnconfigured] = useState('');
  const [refresh, setRefresh] = useState(0);
  const [status, setStatus] = useState('');
  useEffect(() => void sleeperLeague().then(setLeague, () => setUnconfigured('Sleeper is not set up on this server.')), []);
  const { session, artifact, card, error } = useLineup(league, rows, refresh);
  const run = useCallback(() => {
    setStatus('Starting…');
    runLineup().then(() => (setStatus('Started. The new card appears here when it is sealed.'), setRefresh((n) => n + 1)), (e: Error) => setStatus(`Could not start: ${e.message}`));
  }, []);
  return (
    <div className="mx-auto w-full max-w-[1240px] px-4 pt-6 pb-10 sm:px-6">
      <h1 className="m-0 text-[22px] font-bold tracking-[-0.02em]">Sleeper</h1>
      {card && <p className="m-0 mt-1 text-[13px] text-ink-2">{`${card.team} · week ${card.week}`}</p>}
      {unconfigured && <p role="alert" className="mt-3 text-[13px] text-ink-2">{unconfigured}</p>}
      {error && <p role="alert" className="mt-3 text-[13px] text-danger">{error}</p>}
      {league && <ThisWeek card={card} artifact={artifact} running={Boolean(session?.running)} onRun={run} status={status} />}
      {card && artifact && session && <LineupCard card={card} artifact={artifact} sessionKey={session.key} />}
    </div>
  );
}
