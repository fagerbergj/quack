import { useEffect, useState } from 'react';
import { age } from './sessions.ts';
import { decideApproval, listApprovals, type Approval } from './api.ts';

const clip = (s = '', n: number) => (s.length > n ? `${s.slice(0, n)}...` : s);

// `refresh` changes whenever the session saw tool activity, which is when an approval can appear.
export function Approvals({ sessionKey, refresh, onCount }: { sessionKey: string; refresh: number; onCount?: (n: number) => void }) {
  const [items, setItems] = useState<Approval[]>([]);
  const [error, setError] = useState('');
  const load = () => listApprovals(sessionKey).then(setItems, (e: Error) => setError(e.message));
  useEffect(() => void load(), [sessionKey, refresh]);
  useEffect(() => onCount?.(items.length), [items.length]);
  const decide = (id: string, decision: 'approve' | 'reject') =>
    decideApproval(sessionKey, id, decision).then(load, (e: Error) => setError(e.message));
  if (!items.length && !error) return null;
  return (
    <div className="flex flex-col gap-3">
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      {items.map((a) => (
        <section key={a.id} aria-label="Approval" className="flex flex-col gap-2.5 rounded-2xl border border-warning/40 bg-warm p-3.5">
          <div className="flex items-center gap-2">
            <span className="text-[11px] font-bold tracking-[.06em] text-warning">NEEDS YOU</span>
            <span className="ml-auto text-[12px] text-muted">{age(new Date(a.requestedAt).toISOString(), Date.now())}</span>
          </div>
          <p className="m-0 text-[14.5px] font-semibold">{a.tool}</p>
          <details className="text-[13px] text-ink-2">
            <summary className="cursor-pointer">{clip(JSON.stringify(a.args), 100)}</summary>
            <pre className="m-0 mt-2 overflow-x-auto rounded-lg bg-soft p-2.5 font-mono text-[12px]">{JSON.stringify(a.args, null, 2)}</pre>
          </details>
          <div className="flex gap-2">
            <button type="button" aria-label={`Approve ${a.tool}`} onClick={() => decide(a.id, 'approve')} className="h-11 flex-1 cursor-pointer rounded-xl border-0 bg-ink text-sm font-semibold text-on-ink hover:bg-ink-2">Approve</button>
            <button type="button" aria-label={`Reject ${a.tool}`} onClick={() => decide(a.id, 'reject')} className="h-11 flex-1 cursor-pointer rounded-xl border border-line-strong bg-transparent text-sm text-ink hover:text-danger">Reject</button>
          </div>
        </section>
      ))}
    </div>
  );
}
