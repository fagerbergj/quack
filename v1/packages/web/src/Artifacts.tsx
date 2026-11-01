import { useEffect, useState } from 'react';
import { listArtifacts, type SealedArtifact } from './api.ts';

// Checks that did not fully hold; a score under the threshold can fail a version with none listed.
const failing = (a: SealedArtifact) => (a.verdict?.checks ?? []).filter((c) => c.met !== null && c.met < 1).map((c) => `${c.criterion}.${c.id}`);

function Item({ a }: { a: SealedArtifact }) {
  const status = a.verdict ? (a.verdict.passed ? 'checked' : 'checks failed') : 'not checked';
  const failed = failing(a);
  const checks = (a.verdict?.checks ?? []).filter((c) => c.met !== null);
  return (
    <li className="flex flex-col gap-2">
      <details>
        <summary className="flex cursor-pointer list-none items-center gap-1.5 text-xs font-medium text-muted [&::-webkit-details-marker]:hidden">
          <span aria-hidden="true" className="grid h-4 w-4 place-items-center rounded-full bg-soft text-[10px] text-accent-ink">✓</span>
          {`${a.kind} v${a.version} sealed · ${status}`}
        </summary>
        <pre className="m-0 mt-2 overflow-x-auto whitespace-pre-wrap rounded-lg bg-soft p-2.5 font-mono text-[12px] text-ink-2">{a.content}</pre>
      </details>
      {a.verdict && !a.verdict.passed && (
        <div className="rounded-xl border border-danger-line bg-danger-bg/60 px-3.5 py-2.5 text-[13px]">
          <p className="m-0 font-semibold text-danger">{`Judge: ${checks.filter((c) => (c.met ?? 0) >= 1).length} of ${checks.length} checks passed`}</p>
          {failed.length > 0 && <p className="m-0 mt-1 font-mono text-[12px] text-ink-2">{`Failing: ${failed.join(', ')}`}</p>}
        </div>
      )}
    </li>
  );
}

// `refresh` changes whenever the session saw tool activity, which is when a version can be sealed or checked.
export function Artifacts({ sessionKey, refresh }: { sessionKey: string; refresh: number }) {
  const [items, setItems] = useState<SealedArtifact[]>([]);
  const [error, setError] = useState('');
  useEffect(() => void listArtifacts(sessionKey).then(setItems, (e: Error) => setError(e.message)), [sessionKey, refresh]);
  if (!items.length && !error) return null;
  return (
    <section aria-labelledby="artifacts-h" className="flex flex-col gap-2">
      <h2 id="artifacts-h" className="sr-only">Artifacts</h2>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      <ul className="m-0 flex list-none flex-col gap-2 p-0 text-[13px]">
        {items.map((a) => <Item key={`${a.kind}:${a.version}`} a={a} />)}
      </ul>
    </section>
  );
}
