import { checkFailed, type Check, type Verdict } from './api.ts';
import { CheckRow, Pill } from './CheckList.tsx';

const chev = <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="chev flex-none text-muted transition-transform"><path d="M9 6l6 6-6 6" /></svg>;

// A score only when partial: a failed gate can sit beside a weighted 100%, and "100%" on a fail reads as a contradiction.
function headline(v: Verdict, failed: number): string {
  const pct = v.score > 0 && v.score < 1 ? ` · ${Math.round(v.score * 100)}%` : '';
  if (v.passed) return `Double-check passed${pct}`;
  const fix = `${failed} to fix`;
  return v.limitReached ? `Double-check failed at the revision limit · ${fix}${pct}` : `Double-check failed · ${fix}${pct}`;
}

// Checks are named by the problem they look for, so a passed one only needs a pill when it says more than "not found".
const telling = (c: Check) => c.met === null || (c.met > 0 && c.met < 1);

/** A version's check result: every criterion with pass or fail and the judge's note; a failed version opens on what its revision must fix. */
export function CheckResult({ verdict, open }: { verdict: Verdict; open: boolean }) {
  const failed = verdict.checks.filter(checkFailed);
  const rest = verdict.checks.filter((c) => !checkFailed(c));
  const group = (title: string, checks: Check[], all: boolean) => checks.length > 0 && (
    <div className="flex flex-col">
      <h3 className="m-0 pt-1 text-[11.5px] font-semibold tracking-wide text-muted uppercase">{title}</h3>
      {checks.map((c) => <CheckRow key={`${c.criterion}.${c.id}`} check={c} pill={all || telling(c)} />)}
    </div>
  );
  return (
    <details open={open} className="text-[13px]">
      <summary className={`flex min-h-11 cursor-pointer list-none items-center gap-2 lg:min-h-8 [&::-webkit-details-marker]:hidden ${verdict.passed ? 'text-success' : 'text-danger'}`}>
        {chev}
        <span>{headline(verdict, failed.length)}</span>
      </summary>
      <div className="ml-[18px] flex flex-col gap-2 text-ink">
        {group(verdict.limitReached ? 'Released with these unfixed' : 'The revision must fix', failed, true)}
        {group('Checked, nothing found', rest, false)}
      </div>
    </details>
  );
}

type Live = { checks: Array<Omit<Check, 'met' | 'notes'> & { met?: number | null }> };

// What the judge publishes while it runs; anything else (another tool's details) is not a check in progress.
export function liveChecks(details: unknown): Live['checks'] | undefined {
  const checks = (details as Partial<Live> | null | undefined)?.checks;
  return Array.isArray(checks) && checks.length > 0 && checks.every((c) => typeof c?.id === 'string') ? checks : undefined;
}

export const checkedCount = (checks: Live['checks']) => checks.filter((c) => c.met !== undefined).length;

type LiveCheck = Live['checks'][number];
const failedNow = (c: LiveCheck) => c.met !== undefined && checkFailed({ ...c, met: c.met, notes: [] });
const SEGMENT = (c: LiveCheck) => (c.met === undefined ? 'bg-line' : c.met === null ? 'bg-soft' : failedNow(c) ? 'bg-danger' : 'bg-success');
const STATE = (c: LiveCheck) => (c.met === undefined ? 'Checking' : c.met === null ? "Didn't apply" : failedNow(c) ? 'Fail' : 'Nothing found');

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

/** The double-check while it runs: a bar with one segment per check, the checks still running, and each finished one folded away. */
export function CheckProgress({ checks }: { checks: Live['checks'] }) {
  const running = checks.filter((c) => c.met === undefined);
  const done = checks.filter((c) => c.met !== undefined);
  return (
    <div className="ml-[34px] flex flex-col gap-2 text-[13px]">
      <div aria-hidden="true" className="flex h-1.5 gap-0.5">{checks.map((c) => <span key={`${c.criterion}.${c.id}`} className={`flex-1 rounded-full ${SEGMENT(c)}`} />)}</div>
      <ul aria-label="Checks running" className="m-0 flex list-none flex-col p-0">
        {running.map((c) => <li key={`${c.criterion}.${c.id}`} className="flex min-h-8 items-center gap-2"><span className="min-w-0 flex-1 break-words">{c.label}</span><span className="flex-none text-[11px] text-muted">Checking</span></li>)}
      </ul>
      {done.length > 0 && (
        <details>
          <summary className="flex min-h-11 cursor-pointer list-none items-center text-muted hover:text-ink lg:min-h-8 [&::-webkit-details-marker]:hidden">{`Done · ${doneSummary(done)}`}</summary>
          <ul aria-label="Checks done" className="m-0 flex list-none flex-col p-0">
            {done.map((c) => <li key={`${c.criterion}.${c.id}`} className="flex min-h-8 items-center gap-2"><span className="min-w-0 flex-1 break-words">{c.label}</span><span className={`flex-none text-[11px] ${failedNow(c) ? 'text-danger' : 'text-muted'}`}>{STATE(c)}</span></li>)}
          </ul>
        </details>
      )}
    </div>
  );
}
