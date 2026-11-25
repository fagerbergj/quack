import { useEffect, useReducer, useRef } from 'react';
import { checkFailed, type Check, type Verdict } from './api.ts';
import { byWhom, CheckRow, Mark, type About } from './CheckList.tsx';

export const chev = <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="chev flex-none text-muted transition-transform"><path d="M9 6l6 6-6 6" /></svg>;

// The round's score is a weighted share the verdict gives no threshold for, so the headline counts what to fix instead.
function headline(v: Verdict, failed: number): string {
  if (v.passed) return 'Double-check passed';
  const fix = `${failed} to fix`;
  return v.limitReached ? `Double-check failed at the ${v.cap === 'gates' ? 'gate' : 'revision'} limit · ${fix}` : `Double-check failed · ${fix}`;
}

/** A version's check result: every criterion with pass or fail and the judge's note; a failed version opens on what its revision must fix. */
export function CheckResult({ verdict, open, summary, about }: { verdict: Verdict; open: boolean; summary?: string; about?: About }) {
  const failed = verdict.checks.filter(checkFailed);
  const later = verdict.checks.filter((c) => c.deferred === 'approval');
  const gated = verdict.checks.filter((c) => c.deferred === 'gate');
  const rest = verdict.checks.filter((c) => !checkFailed(c) && !c.deferred);
  const group = (title: string, checks: Check[]) => checks.length > 0 && (
    <div className="flex flex-col">
      <h3 className="m-0 pt-1 text-[11.5px] font-semibold tracking-wide text-muted uppercase">{title}</h3>
      {checks.map((c) => <CheckRow key={`${c.criterion}.${c.id}`} check={c} mark about={about} />)}
    </div>
  );
  return (
    <details open={open} className="text-[13px]">
      <summary className={`flex min-h-11 cursor-pointer list-none items-center gap-2 lg:min-h-8 [&::-webkit-details-marker]:hidden ${verdict.passed ? 'text-success' : 'text-danger'}`}>
        {chev}
        <span>{summary ?? headline(verdict, failed.length)}</span>
      </summary>
      <div className="ml-[18px] flex flex-col gap-2 text-ink">
        {group(verdict.limitReached ? 'Released with these unfixed' : 'The revision must fix', failed)}
        {group('Checked, nothing found', rest)}
        {group('Run when you approve', later)}
        {group('Run once the gates pass', gated)}
      </div>
    </details>
  );
}

/** A running check's live counts: items settled by resolver, with the judge model now, and not yet answered; absent until the judge sends them. */
type Counts = { settled?: Record<string, number>; escalating?: number; waiting?: number };
type Live = { checks: Array<Omit<Check, 'met' | 'notes'> & Counts & { met?: number | null }> };

// What the judge publishes while it runs; anything else (another tool's details) is not a check in progress.
export function liveChecks(details: unknown): Live['checks'] | undefined {
  const checks = (details as Partial<Live> | null | undefined)?.checks;
  return Array.isArray(checks) && checks.length > 0 && checks.every((c) => typeof c?.id === 'string') ? checks : undefined;
}

export const checkedCount = (checks: Live['checks']) => checks.filter((c) => c.met !== undefined).length;

type LiveCheck = Live['checks'][number];
const failedNow = (c: LiveCheck) => c.met !== undefined && checkFailed({ ...c, met: c.met, notes: [] });
const keyOf = (c: LiveCheck) => `${c.criterion}.${c.id}`;

/** How far the double-check has got and what it has found so far; checks are named by the problem they look for. */
export const progressLabel = (checks: Live['checks']) => {
  const failed = checks.filter(failedNow).length;
  return `Double-checking · ${checkedCount(checks)} of ${checks.length} · ${failed ? `${failed} problem${failed > 1 ? 's' : ''}` : 'nothing found yet'}`;
};

const doneSummary = (done: Live['checks']) => {
  const n = (f: (c: LiveCheck) => boolean) => done.filter(f).length;
  const parts = [[n(failedNow), 'problem'], [n((c) => c.met !== null && !failedNow(c)), 'nothing found'], [n((c) => c.met === null), "didn't apply"]] as const;
  return parts.filter(([k]) => k > 0).map(([k, w]) => (w === 'problem' ? `${k} problem${k > 1 ? 's' : ''}` : `${k} ${w}`)).join(' · ');
};

/** A check with no new answer for this long reads "Waiting…": its items are queued for a model. */
export const STILL_MS = 120_000;
const doneOf = (c: LiveCheck) => (c.units ?? 0) - (c.waiting ?? 0) - (c.escalating ?? 0);

// When each running check last gained an answer; a still check sends nothing, so a 15 s tick re-reads the clock.
function useStill(running: LiveCheck[]): (c: LiveCheck) => boolean {
  const seen = useRef(new Map<string, { done: number; at: number }>());
  const [, tick] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    const t = setInterval(tick, 15_000);
    return () => clearInterval(t);
  }, []);
  const now = Date.now();
  for (const c of running) if (seen.current.get(keyOf(c))?.done !== doneOf(c)) seen.current.set(keyOf(c), { done: doneOf(c), at: now });
  return (c) => c.waiting !== undefined && now - seen.current.get(keyOf(c))!.at >= STILL_MS;
}

const pct = (k: number, n: number) => `${(100 * k) / Math.max(n, 1)}%`;

// The breakdown sums to the items done: answers no model could give, and items their check no longer needed, close the gap.
function tipOf(c: LiveCheck, w: ReturnType<typeof byWhom>, done: number, inFlight: number) {
  const rest = done - w.clef - w.judge - w.code - w.reused;
  const parts = [w.clef && `${w.clef} by Clef`, w.judge && `${w.judge} by the judge model`, w.code && `${w.code} by code`, w.reused && `${w.reused} from earlier versions`, rest && `${rest} undecided or not needed`];
  return [`${done} of ${c.units} done`, ...parts, inFlight && `${inFlight} with the judge model now`, c.waiting && `${c.waiting} waiting`].filter(Boolean).join(', ');
}

/** One running check: "42/60" over a bar of items settled without the judge model, settled by it, with it now, and waiting. */
function RunningRow({ c, still }: { c: LiveCheck; still: boolean }) {
  if (c.waiting === undefined || c.units === undefined) return <li className="flex min-h-8 items-center gap-2"><span className="min-w-0 flex-1 break-words">{c.label}</span><span className="flex-none text-[11px] text-muted">Checking</span></li>;
  const [n, inFlight, done, w] = [c.units, c.escalating ?? 0, doneOf(c), byWhom(c.settled)];
  const tip = `${still ? `No new answer for ${STILL_MS / 60_000} min. ` : ''}${tipOf(c, w, done, inFlight)}`;
  return (
    <li title={tip} className="grid grid-cols-[1fr_auto] items-baseline gap-x-2.5 gap-y-1 py-1.5">
      <span className="min-w-0 break-words">{c.label}</span>
      <span aria-hidden="true" className={`text-[12px] tabular-nums ${still ? 'text-warning' : 'text-muted'}`}>{still ? 'Waiting…' : `${done}/${n}`}</span>
      <span aria-hidden="true" data-bar className="col-span-2 flex h-1 overflow-hidden rounded-full bg-line">
        <span className="bg-accent-ink" style={{ width: pct(done - w.judge, n) }} />
        <span className="bg-info" style={{ width: pct(w.judge, n) }} />
        <span className="bg-info/40" style={{ width: pct(inFlight, n) }} />
      </span>
      <span className="sr-only">{tip}</span>
    </li>
  );
}

/** The double-check while it runs: each running check with its count and bar, and the finished ones folded away under their marks. */
export function CheckProgress({ checks }: { checks: Live['checks'] }) {
  const running = checks.filter((c) => c.met === undefined);
  const done = checks.filter((c) => c.met !== undefined);
  const still = useStill(running);
  return (
    <div className="ml-[34px] flex flex-col gap-2 text-[13px] max-sm:ml-0">
      <ul aria-label="Checks running" className="m-0 flex list-none flex-col p-0">
        {running.map((c) => <RunningRow key={keyOf(c)} c={c} still={still(c)} />)}
      </ul>
      {done.length > 0 && (
        <details>
          <summary className="flex min-h-11 cursor-pointer list-none items-center text-muted hover:text-ink lg:min-h-8 [&::-webkit-details-marker]:hidden">{`Done · ${doneSummary(done)}`}</summary>
          <ul aria-label="Checks done" className="m-0 flex list-none flex-col p-0">
            {done.map((c) => <li key={keyOf(c)} className="flex min-h-8 items-center gap-2"><Mark check={{ ...c, met: c.met!, notes: [] }} /><span className="min-w-0 flex-1 break-words">{c.label}</span></li>)}
          </ul>
        </details>
      )}
    </div>
  );
}
