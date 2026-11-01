import { useEffect, useState } from 'react';
import { decideApproval, listApprovals, type Approval } from './api.ts';

// `refresh` changes whenever the session saw tool activity, which is when an approval can appear.
export function Approvals({ sessionKey, refresh }: { sessionKey: string; refresh: number }) {
  const [items, setItems] = useState<Approval[]>([]);
  const [error, setError] = useState('');
  const load = () => listApprovals(sessionKey).then(setItems, (e: Error) => setError(e.message));
  useEffect(() => void load(), [sessionKey, refresh]);
  const decide = (id: string, decision: 'approve' | 'reject') =>
    decideApproval(sessionKey, id, decision).then(load, (e: Error) => setError(e.message));
  if (!items.length && !error) return null;
  return (
    <section aria-labelledby="approvals-h" className="rounded-[22px] border border-line bg-warm/60 p-4">
      <h2 id="approvals-h" className="m-0 mb-2">Approvals waiting</h2>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      <ul className="m-0 flex list-none flex-col gap-3 p-0">
        {items.map((a) => (
          <li key={a.id} className="flex flex-col gap-2 text-[13px]">
            <details>
              <summary>{a.tool}</summary>
              <pre className="m-0 mt-2 overflow-x-auto rounded-lg bg-soft p-2.5 font-mono text-[12px] text-ink-2">{JSON.stringify(a.args, null, 2)}</pre>
            </details>
            <span className="flex gap-2">
              <button type="button" onClick={() => decide(a.id, 'approve')} className="cursor-pointer rounded-lg bg-ink px-3 py-1 text-xs font-semibold text-on-ink hover:bg-ink-2">{`Approve ${a.tool}`}</button>
              <button type="button" onClick={() => decide(a.id, 'reject')} className="cursor-pointer rounded-lg border border-line bg-surface px-3 py-1 text-xs font-medium text-ink-2 hover:text-danger">{`Reject ${a.tool}`}</button>
            </span>
          </li>
        ))}
      </ul>
    </section>
  );
}
