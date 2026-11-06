import { useState } from 'react';
import type { Approval } from './api.ts';
import { ApprovalCard } from './Needs.tsx';

/** The session's pending approvals, as its socket reports them; a decided card goes at once rather than when the session's next frame lands. */
export function useApprovals(pending: Approval[]) {
  const [decided, setDecided] = useState<string[]>([]);
  return { items: pending.filter((a) => !decided.includes(a.id)), decide: (id: string) => setDecided((d) => [...d, id]) };
}

export function ApprovalCards({ sessionKey, title, items, decide }: { sessionKey: string; title: string; items: Approval[]; decide: (id: string) => void }) {
  return (
    <>
      {items.map((a) => <ApprovalCard key={a.id} sessionKey={sessionKey} title={title} a={a} now={Date.now()} done={() => decide(a.id)} />)}
    </>
  );
}
