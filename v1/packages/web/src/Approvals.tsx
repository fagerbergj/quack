import { useEffect, useState } from 'react';
import { sessionApprovals, type Approval } from './api.ts';
import { ApprovalCard } from './Needs.tsx';

// `refresh` changes whenever the session saw tool activity, which is when an approval can appear.
export function useApprovals(sessionKey: string, refresh: number) {
  const [items, setItems] = useState<Approval[]>([]);
  const [decided, setDecided] = useState<string[]>([]);
  const [error, setError] = useState('');
  useEffect(() => void sessionApprovals(sessionKey).then(setItems, (e: Error) => setError(e.message)), [sessionKey, refresh]);
  // A decided card stays gone: the index lags the session's own record, so the next read can still list it.
  return { items: items.filter((a) => !decided.includes(a.id)), error, decide: (id: string) => setDecided((d) => [...d, id]) };
}

export function ApprovalCards({ sessionKey, title, items, error, decide }: { sessionKey: string; title: string; items: Approval[]; error: string; decide: (id: string) => void }) {
  return (
    <>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      {items.map((a) => <ApprovalCard key={a.id} sessionKey={sessionKey} title={title} a={a} now={Date.now()} done={() => decide(a.id)} />)}
    </>
  );
}
