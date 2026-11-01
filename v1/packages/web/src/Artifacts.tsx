import { useEffect, useState } from 'react';
import { listArtifacts, type SealedArtifact } from './api.ts';

// Checks that did not fully hold; a score under the threshold can fail a version with none listed.
export const failing = (a: SealedArtifact) => (a.verdict?.checks ?? []).filter((c) => c.met !== null && c.met < 1).map((c) => `${c.criterion}.${c.id}`);

export function StatusIcon({ verdict }: { verdict: SealedArtifact['verdict'] }) {
  const [glyph, tone] = !verdict ? ['•', 'bg-soft text-muted'] : verdict.passed ? ['✓', 'bg-success-bg text-success'] : ['✗', 'bg-danger-bg text-danger'];
  return <span aria-hidden="true" className={`grid h-4 w-4 flex-none place-items-center rounded-full text-[10px] ${tone}`}>{glyph}</span>;
}

function Item({ a, onOpen }: { a: SealedArtifact; onOpen: (a: SealedArtifact) => void }) {
  const status = a.verdict ? (a.verdict.passed ? 'checked' : 'checks failed') : 'not checked';
  const failed = failing(a);
  const checks = (a.verdict?.checks ?? []).filter((c) => c.met !== null);
  return (
    <li className="flex flex-col gap-2">
      <div className="flex items-center gap-1.5 text-xs font-medium text-muted">
        <StatusIcon verdict={a.verdict} />
        <span className="min-w-0 flex-1">{`${a.kind} v${a.version} sealed · ${status}`}</span>
        <button type="button" aria-label={`Open ${a.kind} v${a.version}`} onClick={() => onOpen(a)} className="min-h-11 cursor-pointer rounded-lg border border-line bg-surface px-3 text-xs font-medium text-ink-2 hover:text-ink lg:min-h-0 lg:py-1">Open</button>
      </div>
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
export function useArtifacts(sessionKey: string, refresh: number) {
  const [items, setItems] = useState<SealedArtifact[]>([]);
  const [error, setError] = useState('');
  useEffect(() => void listArtifacts(sessionKey).then(setItems, (e: Error) => setError(e.message)), [sessionKey, refresh]);
  return { items, error };
}

export function Artifacts({ items, error, onOpen }: { items: SealedArtifact[]; error: string; onOpen: (a: SealedArtifact) => void }) {
  if (!items.length && !error) return null;
  return (
    <section aria-labelledby="artifacts-h" className="flex flex-col gap-2">
      <h2 id="artifacts-h" className="sr-only">Artifacts</h2>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      <ul className="m-0 flex list-none flex-col gap-2 p-0 text-[13px]">
        {items.map((a) => <Item key={`${a.kind}:${a.version}`} a={a} onOpen={onOpen} />)}
      </ul>
    </section>
  );
}
