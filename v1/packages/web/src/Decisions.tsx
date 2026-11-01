import { useEffect, useState } from 'react';
import { answerDecision, listDecisions, type Decision } from './api.ts';
import { age } from './sessions.ts';

const POLL_MS = 10_000;

/** A question a workflow escalated to a person; each option is a button. */
export function DecisionCard({ sessionKey, d, now, title, done }: { sessionKey: string; d: Decision; now: number; title?: string; done: () => void }) {
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const choose = (choice: string) => {
    if (busy) return;
    setBusy(true);
    void answerDecision(sessionKey, d.step, choice).then(done, (e: Error) => setError(e.message)).finally(() => setBusy(false));
  };
  return (
    <section aria-label="Decision" className="flex flex-col gap-2.5 rounded-2xl border border-warning/40 bg-warm/60 p-3.5">
      <div className="flex items-center gap-2">
        <span className="text-[11px] font-bold tracking-[.06em] text-warning">NEEDS YOU</span>
        {d.askedAt > 0 && <time className="ml-auto text-xs tabular-nums text-muted">{age(new Date(d.askedAt).toISOString(), now)}</time>}
      </div>
      <p className="m-0 text-[14.5px] font-semibold [overflow-wrap:anywhere]">{d.question}</p>
      {title && <a href={`#/s/${encodeURIComponent(sessionKey)}`} className="line-clamp-2 text-[13px] text-link max-lg:-my-2 max-lg:py-3 [overflow-wrap:anywhere]">{title}</a>}
      {d.evidence && (
        <details className="text-[13px] text-ink-2">
          <summary className="cursor-pointer">Evidence</summary>
          <pre className="m-0 mt-2 max-h-60 overflow-auto whitespace-pre-wrap rounded-lg bg-soft p-2.5 font-mono text-[12px] [overflow-wrap:anywhere]">{d.evidence}</pre>
        </details>
      )}
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      <div className="flex flex-wrap gap-2">
        {d.options.map((o) => (
          <button key={o} type="button" disabled={busy} onClick={() => choose(o)} className="min-h-11 min-w-0 flex-1 cursor-pointer rounded-xl border border-line-strong bg-surface px-3 text-sm font-medium text-ink [overflow-wrap:anywhere] hover:bg-soft">{o}</button>
        ))}
      </div>
    </section>
  );
}

/** The session view's open questions; a workflow raises one without any stream event, so it polls. */
export function SessionDecisions({ sessionKey }: { sessionKey: string }) {
  const [items, setItems] = useState<Decision[]>([]);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let live = true;
    const load = () => listDecisions(sessionKey).then((d) => live && setItems(d), () => {});
    void load();
    const timer = setInterval(load, POLL_MS);
    return () => { live = false; clearInterval(timer); };
  }, [sessionKey, tick]);
  return <>{items.map((d) => <DecisionCard key={d.step} sessionKey={sessionKey} d={d} now={Date.now()} done={() => setTick((t) => t + 1)} />)}</>;
}
