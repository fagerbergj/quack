import { ArtifactBoundary } from './ArtifactBoundary.tsx';
import { sentence } from './extension.ts';
import { EXTENSIONS } from './extensions/index.ts';
import type { SealedArtifact } from './api.ts';
import { CheckResult } from './CheckResult.tsx';
import { StatusIcon } from './StatusIcon.tsx';

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

/** Each version's place in the ledger: its revision round (failed versions just before it, in a row). */
export function ledger(items: SealedArtifact[]): Array<{ a: SealedArtifact; round: number }> {
  const sorted = items.toSorted((x, y) => x.sealedAt - y.sealedAt || x.version - y.version);
  return sorted.map((a) => {
    const same = sorted.filter((b) => b.kind === a.kind);
    const before = same.slice(0, same.indexOf(a)).reverse();
    const round = before.findIndex((b) => b.verdict?.passed !== false);
    return { a, round: round < 0 ? before.length : round };
  });
}

/** The anchor a version's ledger item carries, which the context rail scrolls to. */
export const ledgerId = (a: SealedArtifact) => `v-${a.kind}-${a.version}-${a.sealedAt}`;

const link = 'flex min-h-11 min-w-11 cursor-pointer items-center border-0 bg-transparent p-0 text-[13px] text-link no-underline hover:underline lg:min-h-0';

// One version in the transcript: its line and, when it failed, its check result; the session's pane draws the version itself.
export function LedgerItem({ a, round, running, onOpen }: { a: SealedArtifact; round: number; running: boolean; onOpen: (a: SealedArtifact) => void }) {
  const named = namedBy(a, running);
  const hasCard = Boolean(artifactCard(a));
  const label = [a.version > 1 ? `Version ${a.version}` : '', round > 0 ? `revision ${round}` : ''].filter(Boolean).join(' · ');
  return (
    <div data-ledger id={ledgerId(a)} className="flex flex-col gap-1 text-[13px] text-muted">
      <div className="flex items-center gap-2">
        {!named?.previous && <StatusIcon verdict={a.verdict} />}
        <span className="min-w-0 truncate">{lineOf(a, named)}</span>
        {label && <span className="flex-none text-[12px]">{label}</span>}
        <button type="button" aria-label={`Open ${artifactName(a)}${a.version > 1 ? ` version ${a.version}` : ''}`} onClick={() => onOpen(a)} className={link}>Open</button>
        {!hasCard && a.links?.map((l) => <a key={l.url} href={l.url} target="_blank" rel="noopener noreferrer" className={link}>{`${l.label} ↗`}</a>)}
      </div>
      {a.verdict && !a.verdict.passed && <CheckResult verdict={a.verdict} open />}
    </div>
  );
}
