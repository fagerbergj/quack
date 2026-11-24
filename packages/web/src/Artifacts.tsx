import { ArtifactBoundary } from './ArtifactBoundary.tsx';
import { sentence } from './extension.ts';
import { EXTENSIONS } from './extensions/index.ts';
import type { SealedArtifact } from './api.ts';
import { CheckResult } from './CheckResult.tsx';
import { byVerdict, versionOf, type MarkOf } from './delivered.ts';
import { VersionMark } from './VersionMenu.tsx';

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

/** Each version's place in the ledger: its revision round (failed versions just before it, in a row), and which draft it is when
 * several seals share the version a person knows it by. */
export function ledger(items: SealedArtifact[]): Array<{ a: SealedArtifact; round: number; draft?: number }> {
  const sorted = items.toSorted((x, y) => x.sealedAt - y.sealedAt || x.version - y.version);
  return sorted.map((a) => {
    const same = sorted.filter((b) => b.kind === a.kind);
    const before = same.slice(0, same.indexOf(a)).reverse();
    const round = before.findIndex((b) => b.verdict?.passed !== false);
    const drafts = same.filter((b) => versionOf(b) === versionOf(a));
    return { a, round: round < 0 ? before.length : round, ...(drafts.length > 1 && { draft: drafts.indexOf(a) + 1 }) };
  });
}

/** The anchor a version's ledger item carries, which the context rail scrolls to. */
export const ledgerId = (a: SealedArtifact) => `v-${a.kind}-${a.version}-${a.sealedAt}`;

const link = 'flex min-h-11 min-w-11 cursor-pointer items-center border-0 bg-transparent p-0 text-[13px] text-link no-underline hover:underline lg:min-h-0';

// Which save of a version this is, or how many failed versions came just before it: detail for the tooltip, never the line.
const retry = (draft: number | undefined, round: number) => (draft ? `save ${draft}` : round > 0 ? `after ${round} failed ${round === 1 ? 'try' : 'tries'}` : '');

// One version in the transcript: its line, its number and mark (standing, save and retries in the tooltip) and, when it failed, its check result.
// The socket carries only a kind's newest content, so an older version's notes show without their claims here; its pane reads them.
export function LedgerItem({ a, round, draft, running, onOpen, markOf = byVerdict }: { a: SealedArtifact; round: number; draft?: number; running: boolean; onOpen: (a: SealedArtifact) => void; markOf?: MarkOf }) {
  const named = namedBy(a, running);
  const hasCard = Boolean(artifactCard(a));
  const n = versionOf(a);
  // An unjudged record (an approval) also carries a null verdict, so the line leaves "checking" to the pane, as it always has.
  const s = markOf(a);
  const m = a.verdict === null && s.mark === 'checking' ? {} : s;
  const detail = [`v${n}`, m.status, retry(draft, round)].filter(Boolean).join(', ');
  return (
    <div data-ledger id={ledgerId(a)} className="flex flex-col gap-1 text-[13px] text-muted">
      <div className="flex items-center gap-2">
        <span className="min-w-0 truncate">{lineOf(a, named)}</span>
        <span data-detail title={detail} className="flex flex-none items-center gap-1.5 text-[12px]">
          <span aria-hidden="true" className="font-mono font-semibold text-ink-2">{`v${n}`}</span>{!named?.previous && <VersionMark mark={m.mark} />}<span className="sr-only">{detail}</span>
        </span>
        <button type="button" aria-label={`Open ${artifactName(a)} ${detail}`} onClick={() => onOpen(a)} className={link}>Open</button>
        {!hasCard && a.links?.map((l) => <a key={l.url} href={l.url} target="_blank" rel="noopener noreferrer" className={link}>{`${l.label} ↗`}</a>)}
      </div>
      {a.verdict && !a.verdict.passed && <CheckResult verdict={a.verdict} open about={a} />}
    </div>
  );
}
