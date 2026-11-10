import { PrChip } from './prState.tsx';
import { checkState, Diffstat, headMeta, parseOpening, type PrMeta } from './PrCard.tsx';
import { prOf } from './pulls.ts';

type Checks = NonNullable<PrMeta['checks']>;

// Skipped checks (the image job skips on every PR) say nothing, so they never show.
function checkLines(checks: Checks): Array<{ text: string; tone: string }> {
  const live = checks.filter((c) => checkState(c) !== 'skipped');
  if (!live.length) return [];
  const bad = live.filter((c) => checkState(c) !== 'passing');
  if (!bad.length) return [{ text: '✓ All passed', tone: 'text-success' }];
  return bad.map((c) => (checkState(c) === 'failing' ? { text: `✗ ${c.name} failed`, tone: 'text-danger' } : { text: `… ${c.name} running`, tone: 'text-muted' }));
}

function Changes({ files, url }: { files: NonNullable<PrMeta['files']>; url: string }) {
  return (
    <>
      <dt className="text-muted">Changes</dt>
      <dd className="m-0 flex flex-col gap-1">
        <span className="tabular-nums"><Diffstat files={files} changed /></span>
        <a href={url} target="_blank" rel="noopener noreferrer" className="flex min-h-11 items-center text-link no-underline hover:underline lg:min-h-0">Open on GitHub ↗</a>
      </dd>
    </>
  );
}

/** The pull request a review session is about: state, title, branches, checks and size, from the opening message and the service's meta; a row with no data is left out. */
export function PrSection({ sessionKey, opening, meta, title }: { sessionKey: string; title: string; opening?: string; meta: readonly Record<string, unknown>[] }) {
  const key = prOf(sessionKey);
  if (!key) return null;
  const { repo, number } = key;
  const o = opening ? parseOpening(opening) : undefined;
  const pr = headMeta(meta, o);
  const lines = pr?.checks ? checkLines(pr.checks) : [];
  return (
    <section aria-labelledby="rail-pr">
      <h2 id="rail-pr" className="m-0 mb-3 text-xs font-semibold tracking-[.06em] text-muted uppercase">Pull request</h2>
      <div className="-mx-3 flex flex-col items-start gap-3 rounded-2xl bg-soft px-3 py-3.5">
        <PrChip sessionKey={sessionKey} />
        <p className="m-0 text-[14.5px] leading-snug font-semibold [overflow-wrap:anywhere]">{`#${number} ${pr?.title ?? o?.title ?? title}`}</p>
        {(o?.headRef || o?.baseRef) && <code className="font-mono text-[12px] text-ink-2 [overflow-wrap:anywhere]">{[o.headRef, o.baseRef].filter(Boolean).join(' → ')}</code>}
        {(lines.length > 0 || pr?.files?.length) && (
          <dl className="m-0 grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-[13px]">
            {lines.length > 0 && (
              <>
                <dt className="text-muted">Checks at review</dt>
                <dd className="m-0 flex flex-col gap-0.5">{lines.map((l) => <span key={l.text} className={l.tone}>{l.text}</span>)}</dd>
              </>
            )}
            {pr?.files?.length ? <Changes files={pr.files} url={`https://github.com/${repo}/pull/${number}`} /> : null}
          </dl>
        )}
      </div>
    </section>
  );
}
