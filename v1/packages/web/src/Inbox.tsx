import { useState } from 'react';
import { decideApproval } from './api.ts';
import { age, titleOf, type Row } from './sessions.ts';

const btn = 'min-h-11 cursor-pointer rounded-lg px-3 text-xs lg:min-h-0 lg:py-1';

// Args are known only on the per-session fallback; the index records the tool alone.
const summary = (args: unknown) => (args === undefined ? '' : JSON.stringify(args).slice(0, 140));

function Item({ row, a, now, done }: { row: Row; a: Row['approvals'][number]; now: number; done: () => void }) {
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState('');
  const [error, setError] = useState('');
  const decide = (decision: 'approve' | 'reject') =>
    decideApproval(row.key, a.id, decision, reason.trim()).then(done, (e: Error) => setError(e.message));
  const args = summary(a.args);
  return (
    <li className="flex flex-col gap-2 px-4 py-4 sm:px-5">
      <div className="flex items-start gap-3">
        <span className="flex min-w-0 flex-1 flex-col gap-1">
          <span className="font-mono text-[14px] font-semibold [overflow-wrap:anywhere]">{a.tool}</span>
          {args && <code className="text-[12px] text-ink-2 [overflow-wrap:anywhere]">{args}</code>}
          <a href={`#/s/${encodeURIComponent(row.key)}`} className="text-[13px] text-link [overflow-wrap:anywhere]">{titleOf(row)}</a>
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
            <button type="button" onClick={() => decide('reject')} className={`${btn} bg-ink font-semibold text-on-ink hover:bg-ink-2`}>{`Confirm reject ${a.tool}`}</button>
            <button type="button" onClick={() => setRejecting(false)} className={`${btn} border border-line bg-surface font-medium text-ink-2`}>Cancel</button>
          </>
        ) : (
          <>
            <button type="button" onClick={() => decide('approve')} className={`${btn} bg-ink font-semibold text-on-ink hover:bg-ink-2`}>{`Approve ${a.tool}`}</button>
            <button type="button" onClick={() => setRejecting(true)} className={`${btn} border border-line bg-surface font-medium text-ink-2 hover:text-danger`}>{`Reject ${a.tool}`}</button>
          </>
        )}
      </span>
    </li>
  );
}

export function Inbox({ rows, now, reload }: { rows: Row[] | undefined; now: number; reload: () => void }) {
  const waiting = (rows ?? []).flatMap((row) => row.approvals.map((a) => ({ row, a })));
  return (
    <div className="mx-auto w-full max-w-[960px] px-4 pt-8 pb-10 sm:px-6">
      <div className="mb-4 flex flex-wrap items-baseline gap-3">
        <h1 className="m-0 text-[22px] font-bold tracking-[-0.02em]">Inbox</h1>
        {rows && <span className="text-[13px] text-muted">{waiting.length ? `${waiting.length} waiting for your approval` : 'Nothing is waiting on you'}</span>}
      </div>
      <ul className="m-0 flex list-none flex-col divide-y divide-line rounded-[22px] border border-line bg-surface p-0 shadow-card empty:hidden">
        {waiting.map(({ row, a }) => <Item key={`${row.key}:${a.id}`} row={row} a={a} now={now} done={reload} />)}
      </ul>
    </div>
  );
}
