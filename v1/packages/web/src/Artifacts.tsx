import { useEffect, useState } from 'react';
import { ArtifactBoundary } from './ArtifactBoundary.tsx';
import { sentence } from './extension.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { listArtifacts, type SealedArtifact } from './api.ts';
import { CheckResult } from './CheckResult.tsx';

// A pass is a tick with a tooltip, a fail an x; no verdict yet shows nothing.
export function StatusIcon({ verdict }: { verdict: SealedArtifact['verdict'] }) {
  if (!verdict) return null;
  const [glyph, tone] = verdict.passed ? ['✓', 'bg-success-bg text-success'] : ['✗', 'bg-danger-bg text-danger'];
  return <span role={verdict.passed ? 'img' : undefined} aria-label={verdict.passed ? 'Double-checked' : undefined} aria-hidden={verdict.passed ? undefined : true} title={verdict.passed ? 'Double-checked' : undefined} className={`grid h-4 w-4 flex-none place-items-center rounded-full text-[10px] ${tone}`}>{glyph}</span>;
}

// An extension may draw its artifact as a card; undefined leaves the plain line. A card that throws falls back to the saved text.
export function artifactCard(a: SealedArtifact, now?: { running: boolean; meta: readonly Record<string, unknown>[] }) {
  const card = EXTENSIONS.map((e) => e.card?.(a, now)).find(Boolean);
  return card && <ArtifactBoundary artifact={a}>{card}</ArtifactBoundary>;
}

// What an artifact is called anywhere it shows by name; the raw kind never does.
export const artifactName = (a: SealedArtifact) => EXTENSIONS.map((e) => e.artifact?.(a, false)).find(Boolean)?.name || 'Result';
const namedBy = (a: SealedArtifact, running: boolean) => EXTENSIONS.map((e) => e.artifact?.(a, running)).find(Boolean);

// Plain words for the line: the owning extension names the thing; an unknown kind is just a result, never its raw kind.
function lineOf(a: SealedArtifact, named: ReturnType<typeof namedBy>): string {
  const name = named?.name ?? 'Result';
  if (named?.posted) return named.posted;
  if (a.verdict?.passed === false) return sentence(`${name} didn't pass its checks`.trim());
  return sentence(`${name} ready`.trim());
}

/** Each version's place in the ledger: its revision round (failed versions just before it, in a row), and whether it is the newest of its kind. */
export function ledger(items: SealedArtifact[]): Array<{ a: SealedArtifact; round: number; latest: boolean }> {
  const sorted = items.toSorted((x, y) => x.sealedAt - y.sealedAt || x.version - y.version);
  return sorted.map((a) => {
    const same = sorted.filter((b) => b.kind === a.kind);
    const before = same.slice(0, same.indexOf(a)).reverse();
    const round = before.findIndex((b) => b.verdict?.passed !== false);
    return { a, round: round < 0 ? before.length : round, latest: same.at(-1) === a };
  });
}

const link = 'flex min-h-11 min-w-11 cursor-pointer items-center border-0 bg-transparent p-0 text-[13px] text-link no-underline hover:underline lg:min-h-0';

// One version in the transcript: its line (the newest of a kind as the owning extension's card) and its check result.
export function LedgerItem({ a, round, latest, running, meta, onOpen }: { a: SealedArtifact; round: number; latest: boolean; running: boolean; meta: readonly Record<string, unknown>[]; onOpen: (a: SealedArtifact) => void }) {
  const named = namedBy(a, running);
  const hasCard = Boolean(artifactCard(a));
  const card = latest && !named?.previous ? artifactCard(a, { running, meta }) : undefined;
  const label = [a.version > 1 ? `Version ${a.version}` : '', round > 0 ? `revision ${round}` : ''].filter(Boolean).join(' · ');
  return (
    <div data-ledger className="flex flex-col gap-1 text-[13px] text-muted">
      <div className="flex items-center gap-2">
        {!named?.previous && <StatusIcon verdict={a.verdict} />}
        <span className="min-w-0 truncate">{lineOf(a, named)}</span>
        {label && <span className="flex-none text-[12px]">{label}</span>}
        <button type="button" aria-label={`Open ${artifactName(a)}${a.version > 1 ? ` version ${a.version}` : ''}`} onClick={() => onOpen(a)} className={link}>Open</button>
        {!hasCard && a.links?.map((l) => <a key={l.url} href={l.url} target="_blank" rel="noopener noreferrer" className={link}>{`${l.label} ↗`}</a>)}
      </div>
      {card}
      {a.verdict && <CheckResult verdict={a.verdict} open={!a.verdict.passed} />}
  </div>
  );
}

// `refresh` changes whenever the session saw tool activity, which is when a version can be ready or checked.
export function useArtifacts(sessionKey: string, refresh: number) {
  const [items, setItems] = useState<SealedArtifact[]>([]);
  const [error, setError] = useState('');
  useEffect(() => void listArtifacts(sessionKey).then(setItems, (e: Error) => setError(e.message)), [sessionKey, refresh]);
  return { items, error };
}
