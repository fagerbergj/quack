import type { Failure } from './api.ts';

/** The steps a run ended on; a failed run leaves no transcript, so this is the only place its error shows. */
export function SessionFailures({ items }: { items: Failure[] }) {
  return <>{items.map((f) => (
    <p key={f.step} role="alert" className="m-0 rounded-xl border border-danger-line bg-danger-bg/60 px-3.5 py-2.5 text-[13px] text-danger [overflow-wrap:anywhere]"><strong>Step {f.step} failed:</strong> {f.error}</p>
  ))}</>;
}
