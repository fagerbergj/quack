import { useState } from 'react';
import { decideApproval, resumeRun } from './api.ts';
import { QuestionCard } from './Questions.tsx';
import { age, failedLine, titleOf, type Row } from './sessions.ts';

const btn = 'min-h-11 cursor-pointer rounded-lg px-3 text-xs lg:min-h-0 lg:py-1';
const prim = `${btn} bg-ink font-semibold text-on-ink hover:bg-ink-2 disabled:opacity-50`;
const card = 'flex flex-col gap-2 rounded-2xl border p-3.5';

// A one-line `summary` in an approval's args is its human description.
export function describe(args: unknown): string | undefined {
  const s = (args as { summary?: unknown } | null | undefined)?.summary;
  return typeof s === 'string' && s.trim() ? s.trim() : undefined;
}

function ApprovalCard({ row, a, now, done }: { row: Row; a: Row['approvals'][number]; now: number; done: () => void }) {
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const decide = (decision: 'approve' | 'reject') => {
    if (busy) return;
    setBusy(true);
    void decideApproval(row.key, a.id, decision, reason.trim()).then(done, (e: Error) => setError(e.message)).finally(() => setBusy(false));
  };
  const title = titleOf(row);
  const what = describe(a.args);
  const href = `#/s/${encodeURIComponent(row.key)}`;
  return (
    <section aria-label="Approval" className={`${card} border-warning/40 bg-warm`}>
      <div className="flex items-start gap-3">
        <span className="flex min-w-0 flex-1 flex-col gap-1">
          {what ? <span className="text-[14.5px] font-semibold [overflow-wrap:anywhere]">{what}</span> : <a href={href} className="text-[14.5px] font-semibold text-ink no-underline [overflow-wrap:anywhere]">{`${title} wants to continue`}</a>}
          {what && <a href={href} className="line-clamp-2 text-[13px] text-link max-lg:-my-2 max-lg:py-3 [overflow-wrap:anywhere]">{title}</a>}
        </span>
        <time className="whitespace-nowrap text-[13px] tabular-nums text-muted">{age(new Date(a.requestedAt).toISOString(), now)}</time>
      </div>
      <details className="text-[13px] text-ink-2">
        <summary className="flex min-h-11 cursor-pointer items-center text-link">Details</summary>
        <pre className="m-0 max-h-60 overflow-auto rounded-lg bg-soft p-2.5 font-mono text-[12px] whitespace-pre-wrap [overflow-wrap:anywhere]">{[a.tool, a.args === undefined ? '' : JSON.stringify(a.args, null, 2)].filter(Boolean).join('\n')}</pre>
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
            <button type="button" aria-label={`Confirm reject ${title}`} disabled={busy} onClick={() => decide('reject')} className={prim}>Confirm reject</button>
            <button type="button" onClick={() => setRejecting(false)} className={`${btn} border border-line bg-surface font-medium text-ink-2`}>Cancel</button>
          </>
        ) : (
          <>
            <button type="button" aria-label={`Approve ${title}`} disabled={busy} onClick={() => decide('approve')} className={prim}>Approve</button>
            <button type="button" aria-label={`Reject ${title}`} onClick={() => setRejecting(true)} className={`${btn} border border-line bg-surface font-medium text-ink-2 hover:text-danger`}>Reject</button>
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

/** Everything waiting on the person, answerable in place. A running session's failure is already being retried, so it is not listed. */
export function Needs({ rows, now, reload }: { rows: Row[]; now: number; reload: () => void }) {
  const asked = rows.flatMap((row) => row.questions.map((d) => ({ row, d })));
  const waiting = rows.flatMap((row) => row.approvals.map((a) => ({ row, a })));
  const failed = rows.filter((r) => r.failures.length && !r.running);
  const count = asked.length + waiting.length + failed.length;
  if (!count) return null;
  return (
    <section aria-label="Needs you" className="flex flex-col gap-2">
      <h2 className="m-0 font-sans text-xs font-semibold tracking-[.06em] text-warning uppercase">{`Needs you · ${count}`}</h2>
      {asked.map(({ row, d }) => <QuestionCard key={`${row.key}:${d.id}`} sessionKey={row.key} d={d} now={now} title={titleOf(row)} done={reload} />)}
      {waiting.map(({ row, a }) => <ApprovalCard key={`${row.key}:${a.id}`} row={row} a={a} now={now} done={reload} />)}
      {failed.map((row) => <FailedCard key={row.key} row={row} done={reload} />)}
    </section>
  );
}
