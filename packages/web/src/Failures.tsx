import { useState } from 'react';
import { resumeRun, type Failure } from './api.ts';
import { Duck } from './Pond.tsx';

const btn = 'h-11 flex-1 cursor-pointer rounded-xl text-sm disabled:cursor-not-allowed disabled:opacity-50';

/** Resumes a session's failed run at the step it ended on; `error` says why it could not. */
export function useRunAgain(sessionKey: string, done?: () => void) {
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const run = () => {
    setBusy(true);
    setError('');
    resumeRun(sessionKey).then(done, (e: Error) => setError(e.message)).finally(() => setBusy(false));
  };
  return { run, busy, error, setError };
}

function FailedRun({ sessionKey, f }: { sessionKey: string; f: Failure }) {
  const { run, busy, error, setError } = useRunAgain(sessionKey);
  const [copied, setCopied] = useState(false);
  const [first, ...rest] = f.error.split('\n');
  const copy = () => navigator.clipboard.writeText(f.error).then(() => setCopied(true), () => setError('Could not copy the error.'));
  return (
    <section aria-label="Failed run" className="flex flex-col gap-2.5 rounded-2xl border border-danger-line bg-danger-bg p-3.5">
      <div className="flex flex-wrap items-center gap-2">
        <Duck failed width="30" height="22" />
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
