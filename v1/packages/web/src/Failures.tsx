import { useEffect, useState } from 'react';
import { listFailures, type Failure } from './api.ts';

const POLL_MS = 10_000;

/** The steps a workflow run ended on; a failed run leaves no transcript, so this is the only place its error shows. */
export function SessionFailures({ sessionKey }: { sessionKey: string }) {
  const [items, setItems] = useState<Failure[]>([]);
  useEffect(() => {
    let live = true;
    const load = () => listFailures(sessionKey).then((f) => live && setItems(f), () => {});
    void load();
    const timer = setInterval(load, POLL_MS);
    return () => { live = false; clearInterval(timer); };
  }, [sessionKey]);
  return <>{items.map((f) => (
    <p key={f.step} role="alert" className="m-0 rounded-xl border border-danger-line bg-danger-bg/60 px-3.5 py-2.5 text-[13px] text-danger [overflow-wrap:anywhere]"><strong>Step {f.step} failed:</strong> {f.error}</p>
  ))}</>;
}
