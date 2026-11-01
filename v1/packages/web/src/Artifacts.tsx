import { useEffect, useState } from 'react';
import { listArtifacts, type SealedArtifact } from './api.ts';

// Checks that did not fully hold; a score under the threshold can fail a version with none listed.
export const failing = (a: SealedArtifact) => (a.verdict?.checks ?? []).filter((c) => c.met !== null && c.met < 1).map((c) => `${c.criterion}.${c.id}`);

export function StatusIcon({ verdict }: { verdict: SealedArtifact['verdict'] }) {
  const [glyph, tone] = !verdict ? ['•', 'bg-soft text-muted'] : verdict.passed ? ['✓', 'bg-success-bg text-success'] : ['✗', 'bg-danger-bg text-danger'];
  return <span aria-hidden="true" className={`grid h-4 w-4 flex-none place-items-center rounded-full text-[10px] ${tone}`}>{glyph}</span>;
}

const label = (kind: string) => {
  const name = kind.split('.').at(-1) ?? kind;
  return name.charAt(0).toUpperCase() + name.slice(1);
};

// Sealed, checked, posted: one muted status line each. The drawer's Checks tab holds the detail.
function Item({ a, onOpen }: { a: SealedArtifact; onOpen: (a: SealedArtifact) => void }) {
  const n = failing(a).length;
  const status = !a.verdict ? 'not checked' : a.verdict.passed ? 'checks passed' : `${n || ''} ${n === 1 ? 'check' : 'checks'} failed`.trim();
  const link = 'flex min-h-11 cursor-pointer items-center border-0 bg-transparent p-0 text-[13px] text-link no-underline hover:underline lg:min-h-0';
  return (
    <li className="flex items-center gap-2 text-[13px] text-muted">
      <StatusIcon verdict={a.verdict} />
      <span className="min-w-0 truncate">{`${label(a.kind)} v${a.version} sealed · ${status}`}</span>
      <button type="button" aria-label={`Open ${a.kind} v${a.version}`} onClick={() => onOpen(a)} className={link}>Open</button>
      {a.links?.map((l) => <a key={l.url} href={l.url} target="_blank" rel="noopener noreferrer" className={link}>{`${l.label} ↗`}</a>)}
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
  const [all, setAll] = useState(false);
  if (!items.length && !error) return null;
  // Only the newest version of each kind shows; earlier ones stay a toggle away.
  const newest = new Map(items.map((a) => [a.kind, a.version]));
  for (const a of items) newest.set(a.kind, Math.max(a.version, newest.get(a.kind)!));
  const shown = all ? items : items.filter((a) => a.version === newest.get(a.kind));
  const hidden = items.length - shown.length;
  return (
    <section aria-labelledby="artifacts-h" className="flex flex-col gap-2">
      <h2 id="artifacts-h" className="sr-only">Artifacts</h2>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      <ul className="m-0 flex list-none flex-col gap-2 p-0 text-[13px]">
        {shown.map((a) => <Item key={`${a.kind}:${a.version}`} a={a} onOpen={onOpen} />)}
      </ul>
      {(hidden > 0 || all) && (
        <button type="button" onClick={() => setAll(!all)} className="min-h-11 cursor-pointer self-start border-0 bg-transparent p-0 text-[13px] text-muted hover:text-ink lg:min-h-0">
          {all ? 'Hide earlier versions' : `${hidden} earlier ${hidden === 1 ? 'version' : 'versions'}`}
        </button>
      )}
    </section>
  );
}
