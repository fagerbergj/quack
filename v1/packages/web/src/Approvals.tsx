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
    <section aria-labelledby="approvals-h">
      <h2 id="approvals-h">Approvals waiting</h2>
      {error && <p role="alert">{error}</p>}
      <ul>
        {items.map((a) => (
          <li key={a.id}>
            <details>
              <summary>{a.tool}</summary>
              <pre>{JSON.stringify(a.args, null, 2)}</pre>
            </details>
            <button type="button" onClick={() => decide(a.id, 'approve')}>{`Approve ${a.tool}`}</button>{' '}
            <button type="button" onClick={() => decide(a.id, 'reject')}>{`Reject ${a.tool}`}</button>
          </li>
        ))}
      </ul>
    </section>
  );
}
