import { useState } from 'react';
import { approvalOf, decideApproval, listAsks, questionOf, resumeRun, type Approval, type Ask } from './api.ts';
import { Args } from './JsonView.tsx';
import { QuestionCard } from './Questions.tsx';
import { age, failedLine, needId, titleOf, type Row } from './sessions.ts';
import { useLoad } from './useLoad.ts';

const btn = 'min-h-11 cursor-pointer rounded-lg px-3 text-xs lg:min-h-0 lg:py-1';
const prim = `${btn} bg-ink font-semibold text-on-ink hover:bg-ink-2 disabled:opacity-50`;
const card = 'flex flex-col gap-2 rounded-2xl border p-3.5';

// A one-line `summary` in an approval's args is its human description.
export function describe(args: unknown): string | undefined {
  const s = (args as { summary?: unknown } | null | undefined)?.summary;
  return typeof s === 'string' && s.trim() ? s.trim() : undefined;
}

/** One pending approval, answerable in place; `done` runs once the server has the decision or says it was already made. */
export function ApprovalCard({ sessionKey, title, a, now, done }: { sessionKey: string; title: string; a: Approval; now: number; done: () => void }) {
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState('');
  const [error, setError] = useState('');
  // Which decision is in flight: a stopped session boots before it records one, so the card says so at once.
  const [busy, setBusy] = useState<'approve' | 'reject'>();
  const decide = (decision: 'approve' | 'reject') => {
    if (busy) return;
    setBusy(decision);
    setError('');
    void decideApproval(sessionKey, a.id, decision, reason.trim()).then(done, (e: Error) => setError(e.message)).finally(() => setBusy(undefined));
  };
  const what = describe(a.args);
  const href = `#/s/${encodeURIComponent(sessionKey)}`;
  return (
    <section id={needId(a.id)} aria-label="Approval" aria-busy={!!busy} className={`${card} border-warning/40 bg-warm`}>
      <div className="flex items-start gap-3">
        <span className="flex min-w-0 flex-1 flex-col gap-1">
          {what ? <span className="text-[14.5px] font-semibold [overflow-wrap:anywhere]">{what}</span> : <a href={href} className="text-[14.5px] font-semibold text-ink no-underline [overflow-wrap:anywhere]">{`${title} wants to continue`}</a>}
          {what && <a href={href} className="line-clamp-2 text-[13px] text-link max-lg:-my-2 max-lg:py-3 [overflow-wrap:anywhere]">{title}</a>}
        </span>
        <time className="whitespace-nowrap text-[13px] tabular-nums text-muted">{age(new Date(a.requestedAt).toISOString(), now)}</time>
      </div>
      <details className="text-[13px] text-ink-2">
        <summary className="flex min-h-11 cursor-pointer items-center text-link">Details</summary>
        <p className="m-0 font-mono text-[12px] text-ink">{a.tool}</p>
        {a.args !== undefined && <Args value={a.args} />}
      </details>
      {rejecting && (
        <label className="flex flex-col gap-1 text-[13px] text-ink-2">
          Reason
          <input value={reason} onChange={(e) => setReason(e.target.value)} className="h-11 rounded-lg border border-line bg-surface px-3 text-sm outline-none focus:border-line-strong lg:h-9" />
        </label>
      )}
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      <span className="flex gap-2">
        {rejecting ? (
          <>
            <button type="button" aria-label={`Confirm reject ${title}`} disabled={!!busy} onClick={() => decide('reject')} className={prim}>{busy === 'reject' ? 'Rejecting…' : 'Confirm reject'}</button>
            <button type="button" disabled={!!busy} onClick={() => setRejecting(false)} className={`${btn} border border-line bg-surface font-medium text-ink-2 disabled:opacity-50`}>Cancel</button>
          </>
        ) : (
          <>
            <button type="button" aria-label={`Approve ${title}`} disabled={!!busy} onClick={() => decide('approve')} className={prim}>{busy === 'approve' ? 'Approving…' : 'Approve'}</button>
            <button type="button" aria-label={`Reject ${title}`} disabled={!!busy} onClick={() => setRejecting(true)} className={`${btn} border border-line bg-surface font-medium text-ink-2 hover:text-danger disabled:opacity-50`}>Reject</button>
          </>
        )}
      </span>
    </section>
  );
}

function FailedCard({ row, done }: { row: Row; done: () => void }) {
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const run = () => {
    setBusy(true);
    setError('');
    resumeRun(row.key).then(done, (e: Error) => setError(e.message)).finally(() => setBusy(false));
  };
  const title = titleOf(row);
  return (
    <section aria-label="Failed run" className={`${card} border-danger-line bg-danger-bg`}>
      <a href={`#/s/${encodeURIComponent(row.key)}`} className="text-[14.5px] font-semibold text-ink no-underline [overflow-wrap:anywhere]">{title}</a>
      <p className="m-0 text-[13px] text-danger [overflow-wrap:anywhere]">{failedLine(row.failures[0]!)}</p>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      <span className="flex"><button type="button" aria-label={`Run ${title} again`} disabled={busy} onClick={run} className={prim}>Run again</button></span>
    </section>
  );
}

const NO_ASKS: Ask[] = [];
const askId = (a: Ask) => `${a.session}:${a.id}`;

// The rows say which sessions wait and since when, so the asks are read again only when that changes.
// shortcut: an ask swapped for another between ticks, at the same count and oldest, shows at the next change; carry the newest ask's time on the row if that bites.
function usePending(rows: Row[]) {
  const stamp = rows.filter((r) => r.pendingAsks).map((r) => `${r.key}:${r.pendingAsks}:${r.oldestAskAt}`).join(',');
  return useLoad(() => (stamp ? listAsks('state:pending') : Promise.resolve(NO_ASKS)), [stamp], NO_ASKS);
}

/** Everything waiting on the person, answerable in place. A running session's failure is already being retried, so it is not listed. */
export function Needs({ rows, now, reload }: { rows: Row[]; now: number; reload: () => void }) {
  const { data: asks, error } = usePending(rows);
  // A resolved card goes at once, before the next tick reads the rows again; a re-ask of the same id is newer, so it shows.
  const [resolved, setResolved] = useState<string[]>([]);
  const asked = (a: Ask) => `${askId(a)}@${a.createdAt}`;
  const open = asks.filter((a) => !resolved.includes(asked(a)));
  const done = (a: Ask) => () => (setResolved((d) => [...d, asked(a)]), reload());
  const titled = (key: string) => {
    const row = rows.find((r) => r.key === key);
    return row ? titleOf(row) : key;
  };
  const failed = rows.filter((r) => r.failures.length);
  const count = open.length + failed.length;
  if (!count && !error) return null;
  return (
    <section aria-label="Needs you" className="flex flex-col gap-2">
      <h2 className="m-0 font-sans text-xs font-semibold tracking-[.06em] text-warning uppercase">{`Needs you · ${count}`}</h2>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      {open.map((a) => a.type === 'question'
        ? <QuestionCard key={askId(a)} sessionKey={a.session} d={questionOf(a)} now={now} title={titled(a.session)} done={done(a)} />
        : <ApprovalCard key={askId(a)} sessionKey={a.session} title={titled(a.session)} a={approvalOf(a)} now={now} done={done(a)} />)}
      {failed.map((row) => <FailedCard key={row.key} row={row} done={reload} />)}
    </section>
  );
}
