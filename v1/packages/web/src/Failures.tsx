import { useState } from 'react';
import { resumeRun, type Failure } from './api.ts';

const btn = 'h-11 flex-1 cursor-pointer rounded-xl text-sm disabled:cursor-not-allowed disabled:opacity-50';

// The pond's duck, still, with the same ochre "!" the sessions page shows.
const STILL_DUCK = (
  <svg width="30" height="22" viewBox="0 0 64 48" aria-hidden="true">
    <ellipse cx="34" cy="32" rx="22" ry="11" fill="#fdfcf7" stroke="#2f3a36" strokeOpacity=".25" />
    <circle cx="20" cy="18" r="10" fill="#1f7a63" />
    <rect x="11" y="25" width="18" height="3" rx="1.5" fill="#fff" />
    <path d="M12 15 L1 19 L12 22 Z" fill="#e3a92b" />
    <path d="M14 13 l6 4 M20 13 l-6 4" stroke="#0b0f0e" strokeWidth="1.8" strokeLinecap="round" />
  </svg>
);

function FailedRun({ sessionKey, f }: { sessionKey: string; f: Failure }) {
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const [first, ...rest] = f.error.split('\n');
  const run = () => {
    setBusy(true);
    setError('');
    resumeRun(sessionKey).catch((e: Error) => setError(e.message)).finally(() => setBusy(false));
  };
  const copy = () => navigator.clipboard.writeText(f.error).then(() => setCopied(true), () => setError('Could not copy the error.'));
  return (
    <section aria-label="Failed run" className="flex flex-col gap-2.5 rounded-2xl border border-danger-line bg-danger-bg p-3.5">
      <div className="flex flex-wrap items-center gap-2">
        {STILL_DUCK}
        <span className="text-[11px] font-bold tracking-[.06em] text-danger">RUN FAILED</span>
        <span className="min-w-0 rounded-full bg-surface px-2.5 py-0.5 text-xs font-medium text-ink-2 [overflow-wrap:anywhere]">{f.step}</span>
      </div>
      <p className="m-0 font-mono text-[12.5px] text-danger [overflow-wrap:anywhere]">{first}</p>
      {rest.length > 0 && (
        <details className="text-[13px] text-ink-2">
          <summary className="flex min-h-11 cursor-pointer items-center text-link">Full error</summary>
          <pre className="m-0 max-h-64 overflow-auto rounded-lg bg-surface p-2.5 font-mono text-xs whitespace-pre-wrap [overflow-wrap:anywhere]">{f.error}</pre>
        </details>
      )}
      <p className="m-0 text-[13px] text-ink-2">Run again resumes at this step and keeps the earlier ones.</p>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      <div className="flex gap-2">
        <button type="button" disabled={busy} onClick={run} className={`${btn} border-0 bg-ink font-semibold text-on-ink hover:bg-ink-2`}>Run again</button>
        <button type="button" onClick={copy} className={`${btn} border border-line-strong bg-transparent text-ink hover:bg-soft`}>{copied ? 'Copied' : 'Copy error'}</button>
      </div>
    </section>
  );
}

/** The step a run ended on; a failed run leaves no transcript, so this is the only place its error shows. */
export function SessionFailures({ sessionKey, items }: { sessionKey: string; items: Failure[] }) {
  return <>{items.map((f) => <FailedRun key={f.step} sessionKey={sessionKey} f={f} />)}</>;
}
