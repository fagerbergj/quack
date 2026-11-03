import { useEffect, useState } from 'react';
import { listArtifacts, type SealedArtifact } from './api.ts';
import { ArtifactBoundary } from './ArtifactBoundary.tsx';
import { delivered, versionOf } from './delivered.ts';
import { SessionKeyContext } from './extension.ts';
import { EXTENSIONS } from './extensions/index.ts';

const fullOf = (a: SealedArtifact) => EXTENSIONS.map((e) => e.full?.(a)).find(Boolean);

// One artifact as its own page, for kinds whose layout needs more room than the drawer; `version` picks one, else the newest.
export function ArtifactPage({ sessionKey, version }: { sessionKey: string; version?: number }) {
  const [items, setItems] = useState<SealedArtifact[]>();
  const [error, setError] = useState('');
  const [picked, setPicked] = useState(version);
  useEffect(() => setPicked(version), [version]);
  useEffect(() => void listArtifacts(sessionKey).then((all) => setItems(delivered(all.filter(fullOf))), (e: Error) => setError(e.message)), [sessionKey]);
  // The URL carries the version so a reload or shared link lands on it.
  const pick = (v: number) => {
    setPicked(v);
    location.hash = `#/s/${encodeURIComponent(sessionKey)}/spec?v=${v}`;
  };
  const a = items?.find((x) => versionOf(x) === picked) ?? items?.[0];
  return (
    <div className="mx-auto w-full max-w-[1280px] px-4 pt-3 pb-10 sm:px-6">
      <div className="mb-3 flex items-center gap-3">
        <a href={`#/s/${encodeURIComponent(sessionKey)}`} className="flex min-h-11 items-center text-[13.5px] text-link no-underline hover:underline">‹ Back to the session</a>
        {items && items.length > 1 && (
          <label className="ml-auto flex items-center gap-2 text-[13px] text-ink-2">
            Version
            <select value={a && versionOf(a)} onChange={(e) => pick(Number(e.target.value))} className="h-11 rounded-full border border-line bg-soft px-3 text-[13px] outline-none">
              {items.map((v) => <option key={v.version} value={versionOf(v)}>{`Version ${versionOf(v)}`}</option>)}
            </select>
          </label>
        )}
      </div>
      {error && <p role="alert" className="text-[13px] text-danger">{error}</p>}
      {items && !a && <p className="text-sm text-muted">Nothing to show here yet.</p>}
      {a && <SessionKeyContext value={sessionKey}><ArtifactBoundary artifact={a}>{fullOf(a)}</ArtifactBoundary></SessionKeyContext>}
    </div>
  );
}
