import { useEffect, useState } from 'react';
import { listArtifacts, type SealedArtifact } from '../../api.ts';
import type { Row } from '../../sessions.ts';

// Session keys are sleeper:<league>:<week>:<job>[:...]; trade keys carry the offer after the job.
const KEY = /^sleeper:([^:]+):(\d+):([a-z-]+)(?::|$)/;

export type Sess = { key: string; week: number; job: string; running: boolean };
// A sealed result: the artifact and its parsed card content (any shape the job sealed).
export type Result = Sess & { artifact: SealedArtifact; card: Record<string, any> };

export function sessionsOf(league: string | undefined, rows: Row[] | undefined): Sess[] {
  if (!league) return [];
  return (rows ?? []).flatMap((r) => {
    const m = KEY.exec(r.key);
    return m && m[1] === league ? [{ key: r.key, week: Number(m[2]), job: m[3]!, running: r.running }] : [];
  });
}

const newest = (all: Sess[], job: string) => all.filter((s) => s.job === job).sort((a, b) => b.week - a.week)[0];

/** The current week shows its lineup and each other job's latest result; a past week shows only its own sessions. History is read for any week's result. */
export function wanted(all: Sess[], week: number, current: number): Sess[] {
  const history = newest(all, 'history');
  const own = all.filter((s) => s.week === week);
  if (week !== current) return history && !own.includes(history) ? [...own, history] : own;
  const latest = ['waivers', 'trade-finder', 'trends', 'digest', 'retro', 'history'].flatMap((j) => newest(all, j) ?? []);
  return [...own.filter((s) => s.job === 'lineup'), ...latest];
}

function parse(s: Sess, all: SealedArtifact[]): Result | undefined {
  const artifact = all.filter((a) => a.kind === 'sleeper.cards').sort((a, b) => b.version - a.version)[0];
  try {
    return artifact ? { ...s, artifact, card: JSON.parse(artifact.content) } : undefined;
  } catch {
    return undefined;
  }
}

/** Each session's newest sealed result, read again when one starts or finishes or `refresh` changes; each card shows as its own read lands. */
export function useResults(sessions: Sess[], refresh: number) {
  const [results, setResults] = useState<Record<string, Result>>({});
  const [error, setError] = useState('');
  const stamp = sessions.map((s) => `${s.key}|${s.running}`).join(',');
  useEffect(() => {
    let live = true;
    // A stopped session boots to answer, so one slow read must not hold back the rest; earlier results stay up meanwhile.
    const one = (s: Sess) => listArtifacts(s.key).then((a) => {
      const r = parse(s, a);
      if (live && r) setResults((prev) => ({ ...prev, [r.key]: r }));
    });
    Promise.all(sessions.map(one)).then(() => live && setError(''), (e: Error) => live && setError(e.message));
    return () => void (live = false);
  }, [stamp, refresh]);
  return { results, error };
}

export type WeekResult = { won: boolean | undefined; started: number; opp?: number; left: number; best: number };

/** How the week ended: the retro's own figures, else the history job's row for that week. Undefined when neither has it. */
export function weekResult(week: number, results: Result[]): WeekResult | undefined {
  const retro = results.find((r) => r.job === 'retro' && r.week === week && r.card.kind === 'retro' && !r.card.bye);
  if (retro) return { won: retro.card.won, started: retro.card.started, opp: retro.card.opp, left: retro.card.left, best: retro.card.best };
  const history = results.find((r) => r.job === 'history' && r.card.kind === 'history');
  const row = history?.card.seasons?.find((s: { in_progress: boolean }) => s.in_progress)?.weeks?.find((w: { week: number }) => w.week === week);
  return row && { won: row.result === 'W' ? true : row.result === 'L' ? false : undefined, started: row.started, opp: row.opp, left: row.left, best: row.best };
}
