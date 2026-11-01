import { useState } from 'react';
import { decideApproval } from './api.ts';
import { DecisionCard } from './Decisions.tsx';
import { age, titleOf, type Row } from './sessions.ts';

const btn = 'min-h-11 cursor-pointer rounded-lg px-3 text-xs lg:min-h-0 lg:py-1';

// Args are known only on the per-session fallback; the index records the tool alone.
const summary = (args: unknown) => (args === undefined ? '' : JSON.stringify(args).slice(0, 140));

function Item({ row, a, now, done }: { row: Row; a: Row['approvals'][number]; now: number; done: () => void }) {
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const decide = (decision: 'approve' | 'reject') => {
    if (busy) return;
    setBusy(true);
    void decideApproval(row.key, a.id, decision, reason.trim()).then(done, (e: Error) => setError(e.message)).finally(() => setBusy(false));
  };
  const args = summary(a.args);
  return (
    <li className="flex flex-col gap-2 px-4 py-4 sm:px-5">
      <div className="flex items-start gap-3">
        <span className="flex min-w-0 flex-1 flex-col gap-1">
          <span className="font-mono text-[14px] font-semibold [overflow-wrap:anywhere]">{a.tool}</span>
          {args && <code className="text-[12px] text-ink-2 [overflow-wrap:anywhere]">{args}</code>}
          <a href={`#/s/${encodeURIComponent(row.key)}`} className="line-clamp-2 text-[13px] text-link max-lg:-my-2 max-lg:py-3 [overflow-wrap:anywhere]">{titleOf(row)}</a>
        </span>
        <time className="whitespace-nowrap text-[13px] tabular-nums text-muted">{age(new Date(a.requestedAt).toISOString(), now)}</time>
      </div>
      {rejecting && (
        <label className="flex flex-col gap-1 text-[13px] text-ink-2">
          Reason
          <input value={reason} onChange={(e) => setReason(e.target.value)} className="h-11 rounded-lg border border-line bg-soft px-3 text-sm outline-none focus:border-line-strong lg:h-9" />
        </label>
      )}
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      <span className="flex gap-2">
        {rejecting ? (
          <>
            <button type="button" aria-label={`Confirm reject ${a.tool}`} disabled={busy} onClick={() => decide('reject')} className={`${btn} bg-ink font-semibold text-on-ink hover:bg-ink-2`}>Confirm reject</button>
            <button type="button" onClick={() => setRejecting(false)} className={`${btn} border border-line bg-surface font-medium text-ink-2`}>Cancel</button>
          </>
        ) : (
          <>
            <button type="button" aria-label={`Approve ${a.tool}`} disabled={busy} onClick={() => decide('approve')} className={`${btn} bg-ink font-semibold text-on-ink hover:bg-ink-2`}>Approve</button>
            <button type="button" aria-label={`Reject ${a.tool}`} onClick={() => setRejecting(true)} className={`${btn} border border-line bg-surface font-medium text-ink-2 hover:text-danger`}>Reject</button>
          </>
        )}
      </span>
    </li>
  );
}

export function Inbox({ rows, error, now, reload }: { rows: Row[] | undefined; error: string; now: number; reload: () => void }) {
  const waiting = (rows ?? []).flatMap((row) => row.approvals.map((a) => ({ row, a })));
  const asked = (rows ?? []).flatMap((row) => row.decisions.map((d) => ({ row, d })));
  const summary = [waiting.length && `${waiting.length} waiting for your approval`, asked.length && `${asked.length} waiting for your answer`].filter(Boolean).join(' · ');
  return (
    <div className="mx-auto w-full max-w-[960px] px-4 pt-8 pb-10 sm:px-6">
      <div className="mb-4 flex flex-wrap items-baseline gap-3">
        <h1 className="m-0 text-[22px] font-bold tracking-[-0.02em]">Inbox</h1>
        {rows && !error && <span className="text-[13px] text-muted">{summary || 'Nothing is waiting on you'}</span>}
      </div>
      {error && <p role="alert" className="m-0 mb-3 text-[13px] text-danger">{error}</p>}
      <ul className="m-0 flex list-none flex-col divide-y divide-line rounded-[22px] border border-line bg-surface p-0 shadow-card empty:hidden">
        {asked.map(({ row, d }) => <li key={`${row.key}:${d.step}`} className="p-3 sm:p-4"><DecisionCard sessionKey={row.key} d={d} now={now} title={titleOf(row)} done={reload} /></li>)}
        {waiting.map(({ row, a }) => <Item key={`${row.key}:${a.id}`} row={row} a={a} now={now} done={reload} />)}
      </ul>
    </div>
  );
}
